package observe

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Every panel read from Traefik's counters counts a series that is new while
// the store was scraping from zero: a status code's first requests are the
// requests an app's error rate exists to show.
func TestTraefikPanelsCountANewSeriesFromZero(t *testing.T) {
	rec := &recorder{body: `{"status":"success","data":{"resultType":"matrix","result":[]}}`}
	ms, _ := NewMetricStore(rec.server(t).URL, nil)
	if _, err := ms.Dashboard(context.Background(), teamScope(t), WindowFor(time.Now(), time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, f := range rec.forms {
		q := f.Get("query")
		if !strings.Contains(q, "traefik_") {
			continue
		}
		if strings.Contains(q, "rate(traefik_") || strings.Contains(q, "(increase(traefik_") && !strings.Contains(q, "increase_pure(traefik_") {
			t.Errorf("a Traefik counter is read with a rate that drops a new series' first sample: %s", q)
		}
		if !strings.Contains(q, "increase_pure(traefik_") || !strings.Contains(q, "max_over_time(up[") {
			t.Errorf("a Traefik counter is not counted from zero while its job was being scraped: %s", q)
		}
	}
}

// requestScenario is what QA saw on the live install (OBSERVE-2): an app
// whose 200s had been counted for a while, then 30 requests answered 404 and
// 10 answered 500, each of those codes new to Traefik, and so each series'
// first sample already holding its burst. Beside it, a service whose counter
// the store first sees after being away itself, holding a day's traffic that
// belongs to no window on the chart.
type requestScenario struct {
	t0       time.Time
	ours     Service // the app with the burst
	returned Service // the app whose counter predates the store's return
	series   []storedSeries
}

func newRequestScenario(t0 time.Time, run string) requestScenario {
	s := requestScenario{
		t0:       t0,
		ours:     Service{Name: "web", Namespace: randomNamespace()},
		returned: Service{Name: "api", Namespace: randomNamespace()},
	}
	at := func(scrape int) int64 {
		return t0.Add(time.Duration(scrape)*30*time.Second + 7*time.Second).UnixMilli()
	}
	// Thirty scrapes, a quarter of an hour, starting ten minutes before t0
	// for the job the burst is counted under.
	counter := func(svc Service, job, code string, from int, values ...float64) storedSeries {
		c := storedSeries{labels: map[string]string{
			"__name__": "traefik_service_requests_total", "job": job, "instance": job + "-0",
			"service": TraefikService(svc), "code": code, "method": "GET", "protocol": "http",
		}}
		for i, v := range values {
			c.samples = append(c.samples, rawSample{ms: at(from + i), v: v})
		}
		return c
	}
	up := func(job string, from, to int) storedSeries {
		u := storedSeries{labels: map[string]string{"__name__": "up", "job": job, "instance": job + "-0"}}
		for i := from; i < to; i++ {
			u.samples = append(u.samples, rawSample{ms: at(i), v: 1})
		}
		return u
	}
	steady := make([]float64, 50)
	for i := range steady {
		steady[i] = 100 + float64(i) // a request a scrape, all answered 200
	}
	burst404 := make([]float64, 30)
	for i := range burst404 {
		burst404[i] = 30
	}
	burst404[0] = 22 // first scraped mid-burst, the rest after
	burst500 := make([]float64, 29)
	for i := range burst500 {
		burst500[i] = 10 // the whole burst before the first scrape, nothing after
	}

	ours, away := run+"-traefik", run+"-traefik-returned"
	s.series = append(s.series,
		up(ours, -20, 30),
		counter(s.ours, ours, "200", -20, steady...),
		counter(s.ours, ours, "404", 0, burst404...),
		counter(s.ours, ours, "500", 1, burst500...),
		// The store was away until scrape 2: the counter it finds then ran
		// all along, and what it holds is not traffic of this window.
		up(away, 2, 30),
		counter(s.returned, away, "200", 2, func() []float64 {
			v := make([]float64, 28)
			for i := range v {
				v[i] = 5000 + float64(i)
			}
			return v
		}()...),
	)
	return s
}

// sumRequests is how many requests a req/s panel's series stand for: each
// point is the rate over a window of lookback, and points step apart.
func sumRequests(p Panel, series string, w Window) float64 {
	var total float64
	for _, s := range p.Series {
		if s.Name != series {
			continue
		}
		for _, pt := range s.Points {
			total += pt.V * w.Step.Seconds()
		}
	}
	return total
}

// backfilled waits until samples written into the past can be searched, then
// empties the store's cache of answers. The store keeps what it answered for
// any time more than five minutes ago, as settled, so a test writing hours
// back would otherwise be read what that time held before it wrote: its own
// polling, or an earlier run's queries over the same hour.
func backfilled(t *testing.T, base, selector string, at time.Time) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.PostForm(base+"/api/v1/query", url.Values{
			"query": {"count(" + selector + ")"}, "time": {strconv.FormatInt(at.Unix(), 10)}, "nocache": {"1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), `"value"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became searchable: %s", selector, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	resp, err := http.Get(base + "/internal/resetRollupResultCache")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// Against a real VictoriaMetrics, when one is given: a status code's first
// burst is on the Requests chart and in the error rate, and a counter found
// after the store was away is not a spike.
//
//	docker run -p 8428:8428 victoriametrics/victoria-metrics:v1.154.1
//	YACHT_TEST_VICTORIAMETRICS_URL=http://127.0.0.1:8428 go test ./internal/observe
func TestRequestsAgainstVictoriaMetrics(t *testing.T) {
	base := os.Getenv("YACHT_TEST_VICTORIAMETRICS_URL")
	if base == "" {
		t.Skip("set YACHT_TEST_VICTORIAMETRICS_URL to test against a running VictoriaMetrics")
	}
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	run := randomNamespace()
	s := newRequestScenario(t0, run)
	importSamples(t, base, s.series)
	backfilled(t, base, `up{job="`+run+`-traefik"}`, t0)
	ms, err := NewMetricStore(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewScope("team-a", []Service{s.ours, s.returned})
	if err != nil {
		t.Fatal(err)
	}
	// A 15-minute chart from five minutes before the burst: 30-second steps
	// over a minute's window, as the page draws it.
	w := WindowFor(t0.Add(10*time.Minute), 15*time.Minute)

	var requests, errorRate Panel
	panels, err := ms.Dashboard(context.Background(), scope, w)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range panels {
		switch p.Key {
		case PanelRequests:
			requests = p
		case PanelErrorRate:
			errorRate = p
		}
	}
	if requests.Err != "" || errorRate.Err != "" {
		t.Fatalf("panels failed: %q, %q", requests.Err, errorRate.Err)
	}

	// 2xx: our 1 a scrape over the window's 20 scrapes, and the returned
	// app's 1 a scrape after its first sample, never its 5000.
	for class, want := range map[string]float64{"4xx": 30, "5xx": 10} {
		if got := sumRequests(requests, class, w); math.Abs(got-want) > 0.5 {
			t.Errorf("%s requests on the chart = %.2f, want %.0f", class, got, want)
		}
	}
	if got := sumRequests(requests, "2xx", w); got > 100 {
		t.Errorf("2xx requests on the chart = %.2f: a counter found after the store was away was counted whole", got)
	}

	var peak float64
	for _, sr := range errorRate.Series {
		if sr.Name != "web" {
			continue
		}
		for _, pt := range sr.Points {
			peak = math.Max(peak, pt.V)
		}
	}
	if peak == 0 {
		t.Errorf("the error rate never rose above 0 with 10 server errors in the window: %+v", errorRate.Series)
	}
}

// An app's requests are asked of the controller's lines alone, for the app's
// hosts alone, both in the store's extra filter where the query cannot widen
// them; and no hosts is no question asked.
func TestRequestsAreConfinedToTheControllerAndTheHosts(t *testing.T) {
	rec := &recorder{body: `{"_time":"2026-10-30T19:40:20Z","kubernetes.pod_namespace":"kube-system","kubernetes.container_name":"traefik",` +
		`"StartUTC":"2026-10-30T19:40:19.9Z","RequestHost":"web.up.example.test","RequestMethod":"GET","RequestPath":"/a","DownstreamStatus":"500","DownstreamContentSize":"12","Duration":"1500000","ClientHost":"203.0.113.9"}` + "\n" +
		`{"_time":"2026-10-30T19:40:21Z","kubernetes.pod_namespace":"kube-system","kubernetes.container_name":"traefik","RequestHost":"theirs.up.example.test","RequestMethod":"GET"}` + "\n" +
		// A collector that keeps a line whole: read from its message.
		`{"_time":"2026-10-30T19:40:22Z","_msg":"{\"StartUTC\":\"2026-10-30T19:40:21.5Z\",\"RequestHost\":\"web.up.example.test\",\"RequestMethod\":\"POST\",\"DownstreamStatus\":201,\"Duration\":2000000}"}` + "\n"}
	ls, _ := NewLogStore(rec.server(t).URL, nil)
	now := time.Now()

	if got, _, err := ls.Requests(context.Background(), nil, now.Add(-time.Hour), now, 10); err != nil || got != nil || len(rec.forms) != 0 {
		t.Fatalf("no hosts asked the store %d times for %v, %v", len(rec.forms), got, err)
	}

	got, more, err := ls.Requests(context.Background(), []string{"web.up.example.test", "Shop.example.test"}, now.Add(-time.Hour), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	filter := rec.forms[0].Get("extra_filters")
	for _, want := range []string{
		`{kubernetes.pod_namespace in ("kube-system","traefik"), kubernetes.container_name="traefik"}`,
		`RequestHost:~"(?i)^(web\\.up\\.example\\.test|shop\\.example\\.test)$"`,
	} {
		if !strings.Contains(filter, want) {
			t.Errorf("extra filter %s does not hold %s", filter, want)
		}
	}
	if more || len(got) != 2 {
		t.Fatalf("got %+v (more %v), want the app's two requests", got, more)
	}
	if a := got[0]; a.Path != "/a" || a.Status != 500 || a.Bytes != 12 || a.Duration != 1500*time.Microsecond ||
		a.Client != "203.0.113.9" || !a.At.Equal(time.Date(2026, 10, 30, 19, 40, 19, 9e8, time.UTC)) {
		t.Errorf("first request read as %+v", a)
	}
	if b := got[1]; b.Method != "POST" || b.Status != 201 || b.Duration != 2*time.Millisecond {
		t.Errorf("a whole line read as %+v", b)
	}

	// One more than asked comes back: there were more than a page.
	if _, more, _ := ls.Requests(context.Background(), []string{"web.up.example.test"}, now.Add(-time.Hour), now, 1); !more {
		t.Error("two requests for a limit of one did not say there were more")
	}
}

// Against a real VictoriaLogs, when one is given: Traefik's lines as the node
// collector stores them, each of the JSON's fields a field, are found by host,
// older than any rotation; and an app's own lines shaped like the
// controller's, in a container it called traefik, are not.
//
//	docker run -p 9428:9428 victoriametrics/victoria-logs:v1.53.0
//	YACHT_TEST_VICTORIALOGS_URL=http://127.0.0.1:9428 go test ./internal/observe
func TestRequestsAgainstVictoriaLogs(t *testing.T) {
	base := os.Getenv("YACHT_TEST_VICTORIALOGS_URL")
	if base == "" {
		t.Skip("set YACHT_TEST_VICTORIALOGS_URL to test against a running VictoriaLogs")
	}
	host := randomNamespace() + ".up.example.test"
	now := time.Now().UTC()
	var body strings.Builder
	access := func(ns, h, path string, status int, at time.Time) {
		fmt.Fprintf(&body, `{"_time":%q,"time":%q,"level":"info","msg":"",`+
			`"kubernetes.pod_namespace":%q,"kubernetes.pod_name":"traefik-68n9c","kubernetes.container_name":"traefik",`+
			`"StartUTC":%q,"RequestHost":%q,"RequestMethod":"GET","RequestPath":%q,"RequestProtocol":"HTTP/1.1",`+
			`"DownstreamStatus":%d,"DownstreamContentSize":12,"Duration":1500000,"ClientHost":"203.0.113.9"}`+"\n",
			at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), ns, at.Format(time.RFC3339Nano), h, path, status)
	}
	// 140 requests before a rotation an hour ago, 10 after: all in the store.
	for i := range 150 {
		status := 200
		if i >= 100 && i < 130 {
			status = 404
		} else if i >= 130 && i < 140 {
			status = 500
		}
		access("kube-system", host, fmt.Sprintf("/r%d", i), status, now.Add(-time.Hour).Add(time.Duration(i)*time.Second))
	}
	access("kube-system", "someone-else.up.example.test", "/theirs", 200, now.Add(-time.Minute))
	access(randomNamespace(), host, "/impostor", 200, now.Add(-time.Minute))
	resp, err := http.Post(base+"/insert/jsonline?_stream_fields=kubernetes.pod_namespace,kubernetes.pod_name,kubernetes.container_name&_time_field=time",
		"application/stream+json", strings.NewReader(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	ls, _ := NewLogStore(base, nil)
	var got []orchestrator.HTTPLogLine
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, _, err = ls.Requests(context.Background(), []string{strings.ToUpper(host)}, now.Add(-24*time.Hour), now, 5000)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) >= 150 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(got) != 150 {
		t.Fatalf("read %d requests, want the 150 made to the host", len(got))
	}
	codes := map[int]int{}
	for _, l := range got {
		codes[l.Status]++
		if l.Path == "/impostor" || l.Path == "/theirs" {
			t.Errorf("read %s, which is not the controller's record of this host", l.Path)
		}
	}
	if codes[200] != 110 || codes[404] != 30 || codes[500] != 10 {
		t.Errorf("statuses %v, want 110 200s, 30 404s and 10 500s", codes)
	}
	if got[0].Path != "/r0" || got[149].Path != "/r149" {
		t.Errorf("read from %s to %s, want oldest first", got[0].Path, got[149].Path)
	}

	newest, more, err := ls.Requests(context.Background(), []string{host}, now.Add(-24*time.Hour), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !more || len(newest) != 10 || newest[9].Path != "/r149" {
		t.Errorf("a page of 10 = %d lines to %v, more %v; want the newest 10", len(newest), newest[len(newest)-1].Path, more)
	}
}
