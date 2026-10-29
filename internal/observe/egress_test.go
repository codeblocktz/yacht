package observe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sample is one scrape of one counter.
type sample struct {
	at time.Time
	v  float64
}

// counter is one series as the store holds it: Traefik keeps one per
// service, status, method and protocol on each of its replicas, and the
// scraper labels it with the replica it read.
type counter struct {
	metric, service, instance, code string
	samples                         []sample
}

// sent is what the counter's samples say was sent in (from, to]: each rise,
// and after a fall — the replica restarting — the whole of the new value, as
// what it counted from zero since. A counter's first sample counts from zero
// when it starts inside the scenario, as a replica that came up does.
func (c counter) sent(from, to time.Time, startsFresh bool) float64 {
	var total float64
	for i, s := range c.samples {
		if !s.at.After(from) || s.at.After(to) {
			continue
		}
		switch {
		case i == 0 && startsFresh:
			total += s.v
		case i == 0:
		case s.v >= c.samples[i-1].v:
			total += s.v - c.samples[i-1].v
		default:
			total += s.v
		}
	}
	return total
}

// scrapes is a counter scraped every thirty seconds from from to to, rising
// by step each time from start, with what happens at given moments: a value
// it falls to (a restart), or a stretch it is not scraped (a gap).
func scrapes(from, to time.Time, start, step float64, resetAt time.Time, gapFrom, gapTo time.Time) []sample {
	var out []sample
	v := start
	for t := from; !t.After(to); t = t.Add(30 * time.Second) {
		if t.Equal(resetAt) {
			v = 7
		}
		if gapFrom.IsZero() || t.Before(gapFrom) || !t.Before(gapTo) {
			out = append(out, sample{at: t, v: v})
		}
		v += step
	}
	return out
}

// egressScenario is two hours of traffic from t0, for three apps in two
// teams and for the platform, told the way the store would have scraped it.
type egressScenario struct {
	t0       time.Time
	web, api Service // team-a's web, team-b's api
	worker   Service // team-b's worker, scraped with a gap
	counters []counter
	// fresh is the counters that start inside the scenario.
	fresh map[int]bool
}

func newEgressScenario(t0 time.Time, nsWeb, nsAPI, nsWorker string) egressScenario {
	s := egressScenario{
		t0:     t0,
		web:    Service{Name: "web", Namespace: nsWeb},
		api:    Service{Name: "api", Namespace: nsAPI},
		worker: Service{Name: "worker", Namespace: nsWorker},
		fresh:  map[int]bool{},
	}
	early, end := t0.Add(-2*time.Hour), t0.Add(2*time.Hour)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	var none time.Time
	add := func(c counter, fresh bool) {
		if fresh {
			s.fresh[len(s.counters)] = true
		}
		s.counters = append(s.counters, c)
	}
	webLabel := TraefikService(Service{Name: "web", Namespace: nsWeb, Port: 8080})

	// web is served by two Traefik replicas at once, each counting the
	// requests it carried. Replica a restarts in place half an hour in: its
	// counter falls to 7. Replica b's pod is replaced at ninety minutes: its
	// series ends and replica c's begins, from nothing.
	add(counter{egressMetric, webLabel, "traefik-a", "200",
		scrapes(early, end, 1_000_000, 100, at(30), none, none)}, false)
	add(counter{egressMetric, webLabel, "traefik-b", "200",
		scrapes(early, at(89).Add(30*time.Second), 5_000_000, 50, none, none, none)}, false)
	add(counter{egressMetric, webLabel, "traefik-c", "200",
		scrapes(at(90), end, 20, 50, none, none, none)}, true)
	// A second status on replica a: a counter of its own, summed with the rest.
	add(counter{egressMetric, webLabel, "traefik-a", "404",
		scrapes(early, end, 300, 3, none, none, none)}, false)

	// api's port changed forty minutes in: the old label stops, the new one
	// starts from nothing, and both are api's.
	add(counter{egressMetric, TraefikService(Service{Name: "api", Namespace: nsAPI, Port: 8080}), "traefik-a", "200",
		scrapes(early, at(40), 2_000_000, 10, none, none, none)}, false)
	add(counter{egressMetric, TraefikService(Service{Name: "api", Namespace: nsAPI, Port: 80}), "traefik-a", "200",
		scrapes(at(40).Add(30*time.Second), end, 30, 10, none, none, none)}, true)

	// worker's counter goes unscraped for twenty minutes across the hour —
	// the store restarting — and what it sent meanwhile shows up as one
	// rise in the first scrape after.
	add(counter{egressMetric, TraefikService(Service{Name: "worker", Namespace: nsWorker, Port: 80}), "traefik-a", "200",
		scrapes(early, end, 4_000_000, 25, none, at(50), at(70))}, false)

	// The platform's own services, and an app deleted since: Traefik counts
	// them, and they are nobody's to be charged.
	add(counter{egressMetric, "datahuru-buckets-garage-3900@kubernetes", "traefik-a", "200",
		scrapes(early, end, 9_000_000, 9_999, none, none, none)}, false)
	add(counter{egressMetric, "datahuru-datahuru-80@kubernetes", "traefik-a", "200",
		scrapes(early, end, 1, 7_777, none, none, none)}, false)
	add(counter{egressMetric, "yacht-ffffffffffffffff-gone-80@kubernetes", "traefik-a", "200",
		scrapes(early, end, 1, 5_555, none, none, none)}, false)

	// What web sent its database over the private network, counted by the
	// kernel and not by Traefik: not egress, however much of it there is.
	add(counter{"container_network_transmit_bytes_total", webLabel, "node-a", "200",
		scrapes(early, end, 1, 1_000_000, none, none, none)}, false)
	return s
}

// want is what each app sent in (from, to], from the samples themselves.
func (s egressScenario) want(from, to time.Time) map[Service]int64 {
	label := map[string]Service{}
	for _, svc := range []Service{s.web, s.api, s.worker} {
		label[svc.Namespace+"-"+svc.Name] = svc
	}
	out := map[Service]int64{}
	for i, c := range s.counters {
		if c.metric != egressMetric {
			continue
		}
		key, _ := egressKey(c.service)
		if svc, ok := label[key]; ok {
			if n := int64(c.sent(from, to, s.fresh[i])); n > 0 {
				out[svc] += n
			}
		}
	}
	return out
}

func (s egressScenario) services() []Service { return []Service{s.web, s.api, s.worker} }

// fakeVM answers the one query Egress asks, from the scenario's samples:
// each counter's rise over the query's window — from the last sample at or
// before its start, within the five minutes before it the store looks back,
// to the last at or before its end — summed by service.
type fakeVM struct {
	s        egressScenario
	queries  []string
	partial  bool
	refusing bool
}

var increaseQuery = regexp.MustCompile(`^sum by \(service\) \(increase\(([a-z_]+)\[(\w+)\]\)\)$`)

func (f *fakeVM) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.URL.Path != "/api/v1/query" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		q := r.PostForm.Get("query")
		f.queries = append(f.queries, q+" @"+r.PostForm.Get("time")+" nocache="+r.PostForm.Get("nocache"))
		if f.refusing {
			io.WriteString(w, `{"status":"error","error":"no"}`)
			return
		}
		m := increaseQuery.FindStringSubmatch(q)
		if m == nil {
			http.Error(w, "unexpected query "+q, http.StatusBadRequest)
			return
		}
		window, err := time.ParseDuration(strings.Replace(m[2], "d", "h", 1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sec, _ := strconv.ParseInt(r.PostForm.Get("time"), 10, 64)
		end := time.Unix(sec, 0)
		start := end.Add(-window)
		by := map[string]float64{}
		for i, c := range f.s.counters {
			if c.metric != m[1] {
				continue
			}
			by[c.service] += c.rise(start, end, f.s.fresh[i])
		}
		type row struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		var rows []row
		for svc, v := range by {
			rows = append(rows, row{map[string]string{"service": svc}, []any{sec, strconv.FormatFloat(v, 'f', -1, 64)}})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success", "isPartial": f.partial,
			"data": map[string]any{"resultType": "vector", "result": rows},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rise is the counter's increase over (start, end] as the store reads it.
func (c counter) rise(start, end time.Time, startsFresh bool) float64 {
	var base *sample
	var total float64
	for i := range c.samples {
		s := c.samples[i]
		if !s.at.After(start) {
			if s.at.After(start.Add(-5 * time.Minute)) {
				base = &c.samples[i]
			}
			continue
		}
		if s.at.After(end) {
			break
		}
		switch {
		case base == nil && i == 0 && startsFresh:
			total += s.v
		case base == nil:
		case s.v >= base.v:
			total += s.v - base.v
		default:
			total += s.v
		}
		base = &c.samples[i]
	}
	return total
}

func fakeStore(t *testing.T, f *fakeVM, now time.Time) *MetricStore {
	t.Helper()
	ms, err := NewMetricStore(f.server(t).URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ms.now = func() time.Time { return now }
	return ms
}

func sameEgress(t *testing.T, what string, got, want map[Service]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d services, want %d: got %v, want %v", what, len(got), len(want), got, want)
	}
	for svc, n := range want {
		if got[svc] != n {
			t.Errorf("%s: %s sent %d, want %d", what, svc.Name, got[svc], n)
		}
	}
}

// Every Traefik replica's counters are summed, a restart is read as one, a
// replaced replica's successor counts from nothing, a port change keeps what
// was sent under both, and what was sent during a gap in scraping is counted
// once. The platform's services, a deleted app's and the private network's
// traffic are nobody's.
func TestEgressIsWhatEachAppSentThroughTheIngress(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	s := newEgressScenario(t0, "yacht-aaaaaaaaaaaaaaaa", "yacht-bbbbbbbbbbbbbbbb", "yacht-cccccccccccccccc")
	f := &fakeVM{s: s}
	ms := fakeStore(t, f, t0.Add(3*time.Hour))
	ctx := context.Background()
	hour1, hour2 := t0.Add(time.Hour), t0.Add(2*time.Hour)

	first, err := ms.Egress(ctx, t0, hour1, s.services())
	if err != nil {
		t.Fatal(err)
	}
	sameEgress(t, "first hour", first, s.want(t0, hour1))
	second, err := ms.Egress(ctx, hour1, hour2, s.services())
	if err != nil {
		t.Fatal(err)
	}
	sameEgress(t, "second hour", second, s.want(hour1, hour2))
	both, err := ms.Egress(ctx, t0, hour2, s.services())
	if err != nil {
		t.Fatal(err)
	}
	sameEgress(t, "both hours", both, s.want(t0, hour2))
	for _, svc := range s.services() {
		if first[svc]+second[svc] != both[svc] {
			t.Errorf("%s: %d + %d by the hour, %d at once", svc.Name, first[svc], second[svc], both[svc])
		}
	}

	// Spelled out for the cases the scenario is built around, so a mistake
	// in want cannot hide one in Egress. web, first hour: replica a's 120
	// rises of 100 less the one its restart replaced with 7, its other status's
	// 120 of 3, and replica b's 120 of 50.
	if w := int64(119*100 + 7 + 120*3 + 120*50); first[s.web] != w {
		t.Errorf("web sent %d in the first hour, want %d", first[s.web], w)
	}
	// worker: scraped last at 10:49:30 and next at 11:10, so the first hour
	// has 99 rises and the second the 41 steps of the gap as one rise, then
	// 100 more. None is lost or counted twice.
	if w := int64((41 + 100) * 25); second[s.worker] != w {
		t.Errorf("worker sent %d in the second hour, want %d", second[s.worker], w)
	}
	if w := int64(99 * 25); first[s.worker] != w {
		t.Errorf("worker sent %d in the first hour, want %d", first[s.worker], w)
	}

	// Asked again, the same answer.
	again, err := ms.Egress(ctx, t0, hour1, s.services())
	if err != nil {
		t.Fatal(err)
	}
	sameEgress(t, "the first hour again", again, first)

	for _, q := range f.queries {
		if !strings.HasSuffix(q, " nocache=1") || !strings.Contains(q, egressMetric+"[") {
			t.Errorf("query %s is not an uncached read of the ingress's response bytes", q)
		}
	}
}

// Windows are whole seconds: a meter's hour, read with a clock's nanoseconds
// in it, is the same hour.
func TestAnEgressWindowIsWholeSeconds(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	s := newEgressScenario(t0, "yacht-aaaaaaaaaaaaaaaa", "yacht-bbbbbbbbbbbbbbbb", "yacht-cccccccccccccccc")
	f := &fakeVM{s: s}
	ms := fakeStore(t, f, t0.Add(3*time.Hour))
	got, err := ms.Egress(context.Background(), t0.Add(400*time.Millisecond), t0.Add(time.Hour+900*time.Millisecond), s.services())
	if err != nil {
		t.Fatal(err)
	}
	sameEgress(t, "the hour", got, s.want(t0, t0.Add(time.Hour)))
	want := []string{
		fmt.Sprintf("sum by (service) (increase(%s[2h])) @%d nocache=1", egressMetric, t0.Add(time.Hour).Unix()),
		fmt.Sprintf("sum by (service) (increase(%s[1h])) @%d nocache=1", egressMetric, t0.Unix()),
	}
	if strings.Join(f.queries, "\n") != strings.Join(want, "\n") {
		t.Errorf("queries:\n%s\nwant\n%s", strings.Join(f.queries, "\n"), strings.Join(want, "\n"))
	}
}

// Nothing is ever read as zero that was not counted as zero.
func TestEgressThatCannotBeReadIsAnErrorNotNothing(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	s := newEgressScenario(t0, "yacht-aaaaaaaaaaaaaaaa", "yacht-bbbbbbbbbbbbbbbb", "yacht-cccccccccccccccc")
	ctx := context.Background()
	hour := t0.Add(time.Hour)

	var none *MetricStore
	if _, err := none.Egress(ctx, t0, hour, s.services()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no store: %v, want ErrNotConfigured", err)
	}

	f := &fakeVM{s: s}
	srv := f.server(t)
	ms, _ := NewMetricStore(srv.URL, nil)
	ms.now = func() time.Time { return hour.Add(30 * time.Second) }
	if _, err := ms.Egress(ctx, t0, hour, s.services()); !errors.Is(err, ErrWindowOpen) {
		t.Errorf("an hour that ended 30s ago: %v, want ErrWindowOpen", err)
	}
	if len(f.queries) != 0 {
		t.Errorf("an open window was read: %v", f.queries)
	}
	ms.now = func() time.Time { return hour.Add(time.Hour) }
	if _, err := ms.Egress(ctx, hour, t0, s.services()); err == nil {
		t.Error("a window that ends before it starts was read")
	}
	if got, err := ms.Egress(ctx, t0, t0, s.services()); err != nil || len(got) != 0 {
		t.Errorf("an empty window = %v, %v; want nothing", got, err)
	}

	f.partial = true
	if got, err := ms.Egress(ctx, t0, hour, s.services()); err == nil {
		t.Errorf("a partial answer was read as %v", got)
	}
	f.partial, f.refusing = false, true
	if got, err := ms.Egress(ctx, t0, hour, s.services()); err == nil {
		t.Errorf("a refused query was read as %v", got)
	}
	srv.Close()
	if got, err := ms.Egress(ctx, t0, hour, s.services()); err == nil {
		t.Errorf("an unreachable store was read as %v", got)
	}
}

func TestATraefikLabelNamesAnAppByNamespaceAndName(t *testing.T) {
	for label, want := range map[string]string{
		"yacht-aaaaaaaaaaaaaaaa-web-8080@kubernetes":    "yacht-aaaaaaaaaaaaaaaa-web",
		"yacht-aaaaaaaaaaaaaaaa-my-app-80@kubernetes":   "yacht-aaaaaaaaaaaaaaaa-my-app",
		"yacht-aaaaaaaaaaaaaaaa-web-http@kubernetes":    "",
		"yacht-aaaaaaaaaaaaaaaa-web-8080@kubernetescrd": "",
		"dashboard@internal":                            "",
		"-80@kubernetes":                                "",
	} {
		got, ok := egressKey(label)
		if ok != (want != "") || got != want {
			t.Errorf("%s = %q, %v; want %q", label, got, ok, want)
		}
	}
}

// Against a real VictoriaMetrics, when one is given: the store's own
// increase(), with its own rules for restarts, new series and gaps, gives what
// the samples say was sent, hour by hour and at once.
//
//	docker run -p 8428:8428 victoriametrics/victoria-metrics:v1.154.1
//	YACHT_TEST_VICTORIAMETRICS_URL=http://127.0.0.1:8428 go test ./internal/observe
func TestEgressAgainstVictoriaMetrics(t *testing.T) {
	base := os.Getenv("YACHT_TEST_VICTORIAMETRICS_URL")
	if base == "" {
		t.Skip("set YACHT_TEST_VICTORIAMETRICS_URL to test against a running VictoriaMetrics")
	}
	// Namespaces of this run's own, so an earlier run's samples are not
	// this one's apps.
	ns := func() string {
		b := make([]byte, 8)
		rand.Read(b)
		return "yacht-" + hex.EncodeToString(b)
	}
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	s := newEgressScenario(t0, ns(), ns(), ns())
	var body strings.Builder
	for _, c := range s.counters {
		for _, smp := range c.samples {
			fmt.Fprintf(&body, "%s{service=%q,instance=%q,code=%q} %s %d\n",
				c.metric, c.service, c.instance, c.code, strconv.FormatFloat(smp.v, 'f', -1, 64), smp.at.UnixMilli())
		}
	}
	resp, err := http.Post(base+"/api/v1/import/prometheus", "text/plain", strings.NewReader(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("import: %s", resp.Status)
	}

	ms, err := NewMetricStore(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hour1, hour2 := t0.Add(time.Hour), t0.Add(2*time.Hour)
	// Written samples are searchable within a few seconds.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := ms.Egress(ctx, t0, hour2, s.services())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == len(s.services()) && got[s.web] == s.want(t0, hour2)[s.web] || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	var sums []string
	for _, w := range [][2]time.Time{{t0, hour1}, {hour1, hour2}, {t0, hour2}} {
		got, err := ms.Egress(ctx, w[0], w[1], s.services())
		if err != nil {
			t.Fatal(err)
		}
		what := w[0].Format("15:04") + "–" + w[1].Format("15:04")
		sameEgress(t, what, got, s.want(w[0], w[1]))
		keys := make([]string, 0, len(got))
		for svc, n := range got {
			keys = append(keys, fmt.Sprintf("%s=%d", svc.Name, n))
		}
		sort.Strings(keys)
		sums = append(sums, what+" "+strings.Join(keys, " "))
	}
	t.Logf("read:\n%s", strings.Join(sums, "\n"))
}
