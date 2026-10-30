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
	"strconv"
	"strings"
	"testing"
	"time"
)

// The scenario: eight hours of Traefik's counters as the store scraped them,
// every thirty seconds from two hours before t0, for three apps and the
// platform. The store is away for ninety minutes from 12:30 (t0 is 10:00);
// what was sent meanwhile is still in the counters when it comes back.
//
// What each app sent is worked out by hand, in the tests, from what happens
// here — not from the samples by the same rule the code applies.

const scrape = 30 * time.Second

type egressScenario struct {
	t0                time.Time
	web, sleepy, api  Service
	counters, targets []storedSeries
}

// storedSeries is one series as the store holds it.
type storedSeries struct {
	labels  map[string]string
	samples []rawSample
}

func (s egressScenario) at(minutes float64) time.Time {
	return s.t0.Add(time.Duration(minutes * float64(time.Minute)))
}

// scraping reports whether the store scraped at t: not while it was away.
func (s egressScenario) scraping(t time.Time) bool {
	return t.Before(s.at(150)) || !t.Before(s.at(240))
}

// counter is what the store read of a counter that Traefik held from first
// to last, starting at start and rising by step every scrape interval: the
// counter runs on while the store is away, and falls to 7 at resetAt, a
// replica restarting with seven bytes sent since.
func (s egressScenario) counter(labels map[string]string, first, last time.Time, start, step float64, resetAt time.Time) storedSeries {
	out := storedSeries{labels: labels}
	v := start
	for t := first; !t.After(last); t = t.Add(scrape) {
		if t.Equal(resetAt) {
			v = 7
		}
		if s.scraping(t) {
			out.samples = append(out.samples, rawSample{ms: t.UnixMilli(), v: v})
		}
		v += step
	}
	return out
}

func traefikLabels(service, instance, code string) map[string]string {
	return map[string]string{"job": "traefik", "instance": instance, "service": service, "code": code}
}

func newEgressScenario(t0 time.Time, nsWeb, nsSleepy, nsAPI string) egressScenario {
	s := egressScenario{
		t0:     t0,
		web:    Service{Name: "web", Namespace: nsWeb},
		sleepy: Service{Name: "sleepy", Namespace: nsSleepy},
		api:    Service{Name: "api", Namespace: nsAPI},
	}
	begin, end := s.at(-120), s.at(360)
	var never time.Time
	web := TraefikService(Service{Name: "web", Namespace: nsWeb, Port: 8080})
	sleepy := TraefikService(Service{Name: "sleepy", Namespace: nsSleepy, Port: 8080})
	waker := TraefikService(Service{Name: "sleepy-waker", Namespace: nsSleepy, Port: 80})

	s.counters = []storedSeries{
		// web is served by Traefik replicas a and b at once. a restarts in
		// place at 10:30.
		s.counter(traefikLabels(web, "a", "200"), begin, end, 1_000_000, 100, s.at(30)),
		s.counter(traefikLabels(web, "b", "200"), begin, end, 5_000_000, 50, never),
		// web's first 404 is at 11:10: a new counter whose first sample is
		// a 4 MB response, far more than any rise after it.
		s.counter(traefikLabels(web, "a", "404"), s.at(70), end, 4_000_000, 1, never),
		// Replica c is added at 15:00:30 and serves 30 bytes before its first
		// scrape.
		s.counter(traefikLabels(web, "c", "200"), s.at(300.5), end, 30, 10, never),

		// sleepy sleeps from 10:20 to 11:50. Traefik drops its counter when
		// its route moves to the waker, and starts it again from zero when
		// it wakes; the first 30 s after the wake are a 4 MB download.
		s.counter(traefikLabels(sleepy, "a", "200"), begin, s.at(20), 500_000, 1_000, never),
		s.counter(traefikLabels(sleepy, "a", "200"), s.at(110), end, 4_000_000, 1_000, never),
		// The waker served sleepy's waking page for the minute before.
		s.counter(traefikLabels(waker, "a", "200"), s.at(109), s.at(109.5), 2_000, 500, never),

		// api's port changed at 10:40: the old label stops, the new one
		// starts from nothing, and both are api's.
		s.counter(traefikLabels(TraefikService(Service{Name: "api", Namespace: nsAPI, Port: 8080}), "a", "200"),
			begin, s.at(40), 2_000_000, 10, never),
		s.counter(traefikLabels(TraefikService(Service{Name: "api", Namespace: nsAPI, Port: 80}), "a", "200"),
			s.at(40.5), end, 30, 10, never),

		// The platform's own services, and an app deleted long ago: nobody's.
		s.counter(traefikLabels("datahuru-buckets-garage-3900@kubernetes", "a", "200"), begin, end, 9_000_000, 9_999, never),
		s.counter(traefikLabels("datahuru-datahuru-80@kubernetes", "a", "200"), begin, end, 1, 7_777, never),
		s.counter(traefikLabels("yacht-ffffffffffffffff-gone-80@kubernetes", "a", "200"), begin, end, 1, 5_555, never),
	}
	// What web sent its database over the private network, counted by the
	// kernel and not by Traefik: not egress, however much of it there is.
	private := s.counter(map[string]string{"job": "cadvisor", "instance": "node-a", "namespace": nsWeb},
		begin, end, 1, 1_000_000, never)
	private.labels["__name__"] = "container_network_transmit_bytes_total"
	s.counters = append(s.counters, private)
	for _, c := range s.counters {
		if c.labels["__name__"] == "" {
			c.labels["__name__"] = egressMetric
		}
	}

	// The scraper's record of each Traefik replica: answered every scrape it
	// made.
	up := func(instance string, first time.Time) storedSeries {
		u := s.counter(map[string]string{"__name__": "up", "job": "traefik", "instance": instance}, first, end, 1, 0, never)
		return u
	}
	s.targets = []storedSeries{up("a", begin), up("b", begin), up("c", s.at(300.5))}
	return s
}

func (s egressScenario) services() []Service { return []Service{s.web, s.sleepy, s.api} }

// hourly is what each app sent in each of the six hours from t0, by hand.
func (s egressScenario) hourly() []map[Service]int64 {
	return []map[Service]int64{
		// 10:00–11:00. web: replica a's 120 rises of 100, less the one its
		// restart replaced with 7; replica b's 120 of 50. sleepy: 40 rises of
		// 1000 before it slept. api: 80 rises of 10 on the old port, then 30
		// counted from nothing and 39 rises of 10 on the new.
		{s.web: 119*100 + 7 + 120*50, s.sleepy: 40 * 1_000, s.api: 80*10 + 30 + 39*10},
		// 11:00–12:00. web: a and b as before, and the new 404 counter: its
		// whole first sample, then 100 rises of 1. sleepy: the waker's 2000
		// and 500, the whole first sample after the wake, then 20 rises.
		{s.web: 120*100 + 120*50 + 4_000_000 + 100, s.sleepy: 2_000 + 500 + 4_000_000 + 20*1_000, s.api: 120 * 10},
		// 12:00–13:00: the store scraped until 12:29:30, 59 rises.
		{s.web: 59 * (100 + 50 + 1), s.sleepy: 59 * 1_000, s.api: 59 * 10},
		// 13:00–14:00: one sample, at 14:00, 181 scrape intervals after the
		// last; the counters' rise across the gap is the traffic of the gap.
		{s.web: 181 * (100 + 50 + 1), s.sleepy: 181 * 1_000, s.api: 181 * 10},
		{s.web: 120 * (100 + 50 + 1), s.sleepy: 120 * 1_000, s.api: 120 * 10},
		// 15:00–16:00, and replica c: its 30 bytes counted from nothing, then
		// 119 rises of 10.
		{s.web: 120*(100+50+1) + 30 + 119*10, s.sleepy: 120 * 1_000, s.api: 120 * 10},
	}
}

// fakeVM serves the scenario's samples as the store would: raw, for export,
// and the last sample before a moment, for the two instant queries Egress
// asks. Nothing in it integrates a counter.
type fakeVM struct {
	s                         egressScenario
	requests                  []string
	partial, refusing, broken bool
}

var lastQuery = regexp.MustCompile(`^(t?)last_over_time\(` + egressMetric + `\[25h\]\)$`)

func (f *fakeVM) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.requests = append(f.requests, r.URL.Path+" "+r.PostForm.Encode())
		switch {
		case f.refusing:
			io.WriteString(w, `{"status":"error","error":"no"}`)
		case f.broken:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case r.URL.Path == "/api/v1/export":
			f.export(w, r)
		case r.URL.Path == "/api/v1/query":
			f.last(w, r)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeVM) export(w http.ResponseWriter, r *http.Request) {
	match := r.PostForm.Get("match[]")
	start, _ := strconv.ParseInt(r.PostForm.Get("start"), 10, 64)
	end, _ := strconv.ParseInt(r.PostForm.Get("end"), 10, 64)
	var from []storedSeries
	switch match {
	case egressMetric:
		from = f.s.counters
	case `up{job=~"traefik"}`:
		from = f.s.targets
	default:
		http.Error(w, "unexpected match "+match, http.StatusBadRequest)
		return
	}
	enc := json.NewEncoder(w)
	for _, s := range from {
		if s.labels["__name__"] != strings.SplitN(match, "{", 2)[0] {
			continue
		}
		line := map[string]any{"metric": s.labels, "values": []float64{}, "timestamps": []int64{}}
		var values []float64
		var times []int64
		for _, x := range s.samples {
			if x.ms >= start*1000 && x.ms <= end*1000 {
				values, times = append(values, x.v), append(times, x.ms)
			}
		}
		if len(values) == 0 {
			continue
		}
		// In two pieces, as the store sometimes sends a series.
		half := len(values) / 2
		line["values"], line["timestamps"] = values[:half], times[:half]
		enc.Encode(line)
		line["values"], line["timestamps"] = values[half:], times[half:]
		enc.Encode(line)
	}
}

func (f *fakeVM) last(w http.ResponseWriter, r *http.Request) {
	m := lastQuery.FindStringSubmatch(r.PostForm.Get("query"))
	if m == nil || r.PostForm.Get("nocache") != "1" {
		http.Error(w, "unexpected query", http.StatusBadRequest)
		return
	}
	at, _ := strconv.ParseInt(r.PostForm.Get("time"), 10, 64)
	var rows []map[string]any
	for _, s := range f.s.counters {
		if s.labels["__name__"] != egressMetric {
			continue
		}
		var last *rawSample
		for i, x := range s.samples {
			if x.ms > (at-25*3600)*1000 && x.ms <= at*1000 {
				last = &s.samples[i]
			}
		}
		if last == nil {
			continue
		}
		v := strconv.FormatFloat(last.v, 'f', -1, 64)
		if m[1] == "t" {
			v = strconv.FormatFloat(float64(last.ms)/1000, 'f', -1, 64)
		}
		rows = append(rows, map[string]any{"metric": s.labels, "value": []any{at, v}})
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status": "success", "isPartial": f.partial,
		"data": map[string]any{"resultType": "vector", "result": rows},
	})
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
	for svc, n := range want {
		if got[svc] != n {
			t.Errorf("%s: %s sent %d, want %d", what, svc.Name, got[svc], n)
		}
	}
	for svc, n := range got {
		if _, ok := want[svc]; !ok {
			t.Errorf("%s: %s sent %d, and is not one of the apps", what, svc.Name, n)
		}
	}
}

// checkScenario reads each hour, and the six at once, and holds them to what
// was sent.
func checkScenario(t *testing.T, ms *MetricStore, s egressScenario) {
	t.Helper()
	ctx := context.Background()
	whole := map[Service]int64{}
	for i, want := range s.hourly() {
		from := s.t0.Add(time.Duration(i) * time.Hour)
		got, err := ms.Egress(ctx, from, from.Add(time.Hour), s.services())
		if err != nil {
			t.Fatal(err)
		}
		sameEgress(t, from.Format("15:04"), got, want)
		for svc, n := range want {
			whole[svc] += n
		}
	}
	got, err := ms.Egress(ctx, s.t0, s.t0.Add(6*time.Hour), s.services())
	if err != nil {
		t.Fatal(err)
	}
	sameEgress(t, "the six hours at once", got, whole)
}

// Every Traefik replica's counters are summed. A restart, a counter dropped
// while its app slept and begun again, a new counter and a new replica each
// count from zero, however large their first sample; a port change keeps
// what was sent under both, the waker's is the app's, and what was sent while
// the store was away for ninety minutes is counted once. The platform's
// services, a deleted app's and the private network's traffic are nobody's.
func TestEgressIsWhatEachAppSentThroughTheIngress(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	s := newEgressScenario(t0, "yacht-aaaaaaaaaaaaaaaa", "yacht-bbbbbbbbbbbbbbbb", "yacht-cccccccccccccccc")
	ms := fakeStore(t, &fakeVM{s: s}, t0.Add(8*time.Hour))
	checkScenario(t, ms, s)

	// Asked again, the same answer.
	first, _ := ms.Egress(context.Background(), t0, t0.Add(time.Hour), s.services())
	again, _ := ms.Egress(context.Background(), t0, t0.Add(time.Hour), s.services())
	sameEgress(t, "the first hour again", again, first)
}

// The first sample of a counter is counted only when the store was scraping
// before it. With nothing scraped for the whole bridge — a store that was
// away longer, or has just been installed — it may hold anything.
func TestAFirstSampleWithNoScrapeBeforeItIsNotCounted(t *testing.T) {
	at := int64(1_800_000_000_000)
	c := rawSample{ms: at, v: 9_000_000}
	var nothing targetScrapes
	if got := nothing.rise(nil, c); got != 0 {
		t.Errorf("with nothing scraped before = %v, want 0", got)
	}
	failing := targetScrapes{up: []rawSample{{ms: at - 30_000, v: 0}}, job: []int64{at - 30_000}}
	if got := failing.rise(nil, c); got != 0 {
		t.Errorf("with the replica never answering = %v, want 0", got)
	}
	answered := targetScrapes{up: []rawSample{{ms: at - 30_000, v: 1}}, job: []int64{at - 30_000}}
	if got := answered.rise(nil, c); got != c.v {
		t.Errorf("with the replica answering before = %v, want all of it", got)
	}
	// Away, not dropped: no scrape between two samples is a counter that ran on.
	prev := rawSample{ms: at - 3*3600_000, v: 8_000_000}
	away := targetScrapes{up: []rawSample{{ms: prev.ms, v: 1}, {ms: at, v: 1}}, job: []int64{prev.ms, at}}
	if got := away.rise(&prev, c); got != 1_000_000 {
		t.Errorf("across a gap in scraping = %v, want the rise", got)
	}
	// Dropped, not away: the replica answered between, without the counter.
	if got := answered.rise(&prev, c); got != c.v {
		t.Errorf("after a scrape without the counter = %v, want all of it", got)
	}
}

// Windows are whole seconds: a meter's hour, read with a clock's nanoseconds
// in it, is the same hour.
func TestAnEgressWindowIsWholeSeconds(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	s := newEgressScenario(t0, "yacht-aaaaaaaaaaaaaaaa", "yacht-bbbbbbbbbbbbbbbb", "yacht-cccccccccccccccc")
	f := &fakeVM{s: s}
	ms := fakeStore(t, f, t0.Add(8*time.Hour))
	got, err := ms.Egress(context.Background(), t0.Add(400*time.Millisecond), t0.Add(time.Hour+900*time.Millisecond), s.services())
	if err != nil {
		t.Fatal(err)
	}
	sameEgress(t, "the hour", got, s.hourly()[0])
	hour := strconv.FormatInt(t0.Add(time.Hour).Unix(), 10)
	if !strings.Contains(f.requests[0], "start="+strconv.FormatInt(t0.Unix(), 10)) || !strings.Contains(f.requests[0], "end="+hour) {
		t.Errorf("export %s is not the hour", f.requests[0])
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
	if len(f.requests) != 0 {
		t.Errorf("an open window was read: %v", f.requests)
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
	f.refusing, f.broken = false, true
	if got, err := ms.Egress(ctx, t0, hour, s.services()); err == nil {
		t.Errorf("a failing store was read as %v", got)
	}
	srv.Close()
	if got, err := ms.Egress(ctx, t0, hour, s.services()); err == nil {
		t.Errorf("an unreachable store was read as %v", got)
	}
}

func TestATraefikLabelNamesAnAppByNamespaceAndName(t *testing.T) {
	for label, want := range map[string]string{
		"yacht-aaaaaaaaaaaaaaaa-web-8080@kubernetes":        "yacht-aaaaaaaaaaaaaaaa-web",
		"yacht-aaaaaaaaaaaaaaaa-my-app-80@kubernetes":       "yacht-aaaaaaaaaaaaaaaa-my-app",
		"yacht-aaaaaaaaaaaaaaaa-my-app-waker-80@kubernetes": "yacht-aaaaaaaaaaaaaaaa-my-app-waker",
		"yacht-aaaaaaaaaaaaaaaa-web-http@kubernetes":        "",
		"yacht-aaaaaaaaaaaaaaaa-web-8080@kubernetescrd":     "",
		"dashboard@internal":                                "",
		"-80@kubernetes":                                    "",
	} {
		got, ok := egressKey(label)
		if ok != (want != "") || got != want {
			t.Errorf("%s = %q, %v; want %q", label, got, ok, want)
		}
	}
}

// ImportEgressScenario writes the scenario into a real store, under
// namespaces of this run's own.
func importSamples(t *testing.T, base string, series []storedSeries) {
	t.Helper()
	var body strings.Builder
	for _, s := range series {
		var labels []string
		for k, v := range s.labels {
			if k != "__name__" {
				labels = append(labels, fmt.Sprintf("%s=%q", k, v))
			}
		}
		for _, x := range s.samples {
			fmt.Fprintf(&body, "%s{%s} %s %d\n", s.labels["__name__"], strings.Join(labels, ","),
				strconv.FormatFloat(x.v, 'f', -1, 64), x.ms)
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
}

func randomNamespace() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "yacht-" + hex.EncodeToString(b)
}

// Against a real VictoriaMetrics, when one is given: the store's export and
// last-sample answers, read as Egress reads them, give what was sent.
//
//	docker run -p 8428:8428 victoriametrics/victoria-metrics:v1.154.1
//	YACHT_TEST_VICTORIAMETRICS_URL=http://127.0.0.1:8428 go test ./internal/observe
func TestEgressAgainstVictoriaMetrics(t *testing.T) {
	base := os.Getenv("YACHT_TEST_VICTORIAMETRICS_URL")
	if base == "" {
		t.Skip("set YACHT_TEST_VICTORIAMETRICS_URL to test against a running VictoriaMetrics")
	}
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-8 * time.Hour)
	s := newEgressScenario(t0, randomNamespace(), randomNamespace(), randomNamespace())
	// The replicas' up series are shared with other runs; their own
	// instances keep them apart.
	run := randomNamespace()
	for _, c := range append(s.counters, s.targets...) {
		if c.labels["instance"] != "" {
			c.labels["instance"] = run + "-" + c.labels["instance"]
		}
	}
	importSamples(t, base, append(append([]storedSeries{}, s.counters...), s.targets...))

	ms, err := NewMetricStore(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Written samples are searchable within a few seconds.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := ms.Egress(context.Background(), t0, t0.Add(6*time.Hour), s.services())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == len(s.services()) || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	checkScenario(t, ms, s)
}
