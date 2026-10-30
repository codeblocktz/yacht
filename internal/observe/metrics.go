package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MetricStore reads series from VictoriaMetrics, through the Prometheus
// query API.
//
// What it reads is scraped by the store itself from two places, each of which
// already counts what is wanted: the kubelet's cAdvisor endpoint, for every
// container's CPU and memory, and Traefik's Prometheus endpoint, for every
// app's requests, their status, their latency and the bytes they move. No app
// is instrumented and nothing runs in a team's namespace to collect any of it.
//
// Some queries use MetricsQL, VictoriaMetrics' superset of PromQL —
// histogram_quantiles, which reads four percentiles out of one histogram in
// one pass — so a Prometheus behind this address would answer only some
// panels. That is deliberate: the store is the installer's choice, and this
// is the one it installs.
type MetricStore struct {
	base   *url.URL
	client *http.Client

	// now is the clock a window is judged closed by: time.Now, but for tests.
	now func() time.Time
}

// NewMetricStore returns a store for the VictoriaMetrics at rawURL, or nil
// with no error when rawURL is empty.
func NewMetricStore(rawURL string, client *http.Client) (*MetricStore, error) {
	u, err := storeURL("YACHT_METRICS_URL", rawURL)
	if err != nil || u == nil {
		return nil, err
	}
	if client == nil {
		client = defaultClient()
	}
	return &MetricStore{base: u, client: client, now: time.Now}, nil
}

// Point is one sample: Unix milliseconds and a value, which is how the chart
// script reads it.
type Point struct {
	At time.Time
	V  float64
}

// MarshalJSON writes a point as [ms, value], half the size of an object per
// point and the shape the chart already expects.
func (p Point) MarshalJSON() ([]byte, error) {
	return []byte("[" + strconv.FormatInt(p.At.UnixMilli(), 10) + "," +
		strconv.FormatFloat(p.V, 'g', 6, 64) + "]"), nil
}

// Series is one line on a chart.
type Series struct {
	Name   string  `json:"name"`
	Points []Point `json:"points"`
}

// Last is the series' latest value, and whether it has one.
func (s Series) Last() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	return s.Points[len(s.Points)-1].V, true
}

// Units a panel is drawn in, which the chart script formats its axis by.
const (
	UnitCores    = "cores"
	UnitBytes    = "bytes"
	UnitBytesSec = "bytes/s"
	UnitReqSec   = "req/s"
	UnitRatio    = "ratio"
	UnitSeconds  = "seconds"
)

// Panel is one block of a dashboard.
type Panel struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Unit    string   `json:"unit"`
	Stacked bool     `json:"stacked,omitempty"`
	Series  []Series `json:"series"`

	// Start and End are the window, so a chart with a gap at either end
	// draws it rather than stretching what it has across the width.
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`

	// Err is why this panel could not be read, when the others could.
	Err string `json:"-"`
}

// Empty reports whether the panel has nothing to draw.
func (p Panel) Empty() bool {
	for _, s := range p.Series {
		if len(s.Points) > 0 {
			return false
		}
	}
	return true
}

// Window is the range a dashboard covers, and how far apart its points are.
type Window struct {
	Start, End time.Time
	Step       time.Duration
}

// WindowFor is the window ending at end that spans span, with around seventy
// points: enough for a line to read as a shape, few enough to send whole.
//
// Never finer than thirty seconds, the interval the store scrapes at; a step
// below that only repeats samples.
func WindowFor(end time.Time, span time.Duration) Window {
	step := span / 70
	if step < 30*time.Second {
		step = 30 * time.Second
	}
	step = step.Round(time.Second)
	end = end.UTC().Truncate(step)
	return Window{Start: end.Add(-span), End: end, Step: step}
}

// lookback is the rate window for a step: at least two scrapes, so that a
// rate always has two samples to work from, and never less than the step, so
// no traffic between two points goes uncounted.
func (w Window) lookback() string {
	l := w.Step
	if l < time.Minute {
		l = time.Minute
	}
	return promDuration(l)
}

// The dashboard's panels.
const (
	PanelCPU       = "cpu"
	PanelMemory    = "memory"
	PanelEgress    = "egress"
	PanelIngress   = "ingress"
	PanelRequests  = "requests"
	PanelErrorRate = "error_rate"
	PanelLatency   = "latency"
)

// panelSpec is how one panel is read: its query, and which label of the
// answer names each series.
type panelSpec struct {
	key, title, unit string
	stacked          bool
	// query is given the namespace matcher, the Traefik service matcher and
	// the rate window, in that order.
	query string
	by    string
}

// Container series exclude the pod's own cgroup (container="") and the pause
// container, which cAdvisor reports beside the app's and which would count
// every pod's memory twice.
var panelSpecs = []panelSpec{
	{key: PanelCPU, title: "CPU", unit: UnitCores, by: "namespace",
		query: `sum by (namespace) (rate(container_cpu_usage_seconds_total{%[1]s,container!="",container!="POD"}[%[3]s]))`},
	{key: PanelMemory, title: "Memory", unit: UnitBytes, by: "namespace",
		query: `sum by (namespace) (container_memory_working_set_bytes{%[1]s,container!="",container!="POD"})`},
	{key: PanelEgress, title: "Public network egress", unit: UnitBytesSec, by: "service",
		query: `sum by (service) (rate(traefik_service_responses_bytes_total{%[2]s}[%[3]s]))`},
	{key: PanelIngress, title: "Public network ingress", unit: UnitBytesSec, by: "service",
		query: `sum by (service) (rate(traefik_service_requests_bytes_total{%[2]s}[%[3]s]))`},
	// One series per status class, stacked, so the height is the traffic and
	// the colours are how it went.
	{key: PanelRequests, title: "Requests", unit: UnitReqSec, stacked: true, by: "class",
		query: `sum by (class) (label_replace(rate(traefik_service_requests_total{%[2]s}[%[3]s]), "class", "${1}xx", "code", "([1-5])[0-9][0-9]"))`},
	// Server errors over every request, per service. A service that served
	// no 5xx has no 5xx series at all, so its errors are "or" the traffic
	// times nothing: a rate of zero, not a missing line. A service with no
	// traffic still divides by zero, which the store answers with no sample,
	// and the chart draws a gap rather than a rate of nothing.
	{key: PanelErrorRate, title: "Request error rate", unit: UnitRatio, by: "service",
		query: `(sum by (service) (rate(traefik_service_requests_total{%[2]s,code=~"5.."}[%[3]s])) or ` +
			`0 * sum by (service) (rate(traefik_service_requests_total{%[2]s}[%[3]s]))) / ` +
			`sum by (service) (rate(traefik_service_requests_total{%[2]s}[%[3]s]))`},
	// A websocket, a server-sent stream or a long poll is one request held
	// for as long as the page is open, which Traefik times like any other.
	// Counted, every open page drags p95 to the slowest bucket and reads as
	// a slow service. So the open-ended bucket is replaced by the largest
	// bounded one: a response time is of requests that finished within the
	// buckets, and those held longer are left out rather than averaged in.
	{key: PanelLatency, title: "Response time", unit: UnitSeconds, by: "quantile",
		query: `histogram_quantiles("quantile", 0.5, 0.9, 0.95, 0.99, ` +
			`sum by (le) (rate(traefik_service_request_duration_seconds_bucket{%[2]s,le!="+Inf"}[%[3]s])) or ` +
			`label_set(max(sum by (le) (rate(traefik_service_request_duration_seconds_bucket{%[2]s,le!="+Inf"}[%[3]s]))), "le", "+Inf"))`},
}

// Dashboard reads every panel for a scope over a window.
//
// Panels are read one after another and each failure stays with its panel:
// a cluster whose ingress publishes no metrics still has CPU and memory to
// show, and the page says which block could not be read rather than nothing.
func (m *MetricStore) Dashboard(ctx context.Context, scope Scope, w Window) ([]Panel, error) {
	if m == nil {
		return nil, ErrNotConfigured
	}
	ns, err := scope.namespaceMatcher()
	if err != nil {
		return nil, err
	}
	svc, err := scope.serviceMatcher()
	if err != nil {
		return nil, err
	}
	var out []Panel
	failures := 0
	for _, spec := range panelSpecs {
		p := Panel{Key: spec.key, Title: spec.title, Unit: spec.unit, Stacked: spec.stacked, Start: w.Start, End: w.End}
		series, err := m.rangeQuery(ctx, fmt.Sprintf(spec.query, ns, svc, w.lookback()), w)
		if err != nil {
			p.Err = err.Error()
			failures++
		} else {
			p.Series = name(scope, spec.by, series)
		}
		out = append(out, p)
	}
	if failures == len(panelSpecs) {
		// Every panel failing is the store, not the panels: say so once.
		return nil, fmt.Errorf("observe: the metrics store could not be read: %s", out[0].Err)
	}
	return out, nil
}

// PublicEgress is the bytes each service in the scope sent to the internet
// through the ingress since a moment, by service name.
//
// What Traefik counts as response bytes: what the platform's front door
// carried out. An app's own outbound calls do not pass through it and are not
// in this number, which is why it is named for what it is.
func (m *MetricStore) PublicEgress(ctx context.Context, scope Scope, since, now time.Time) (map[string]int64, error) {
	if m == nil {
		return nil, ErrNotConfigured
	}
	svc, err := scope.serviceMatcher()
	if err != nil {
		return nil, err
	}
	span := now.Sub(since)
	if span < time.Minute {
		return map[string]int64{}, nil
	}
	query := fmt.Sprintf(`sum by (service) (increase(traefik_service_responses_bytes_total{%s}[%s]))`,
		svc, promDuration(span.Truncate(time.Second)))
	result, err := m.query(ctx, "/api/v1/query", url.Values{
		"query": {query}, "time": {strconv.FormatInt(now.Unix(), 10)},
	})
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, r := range result {
		if len(r.Value) != 2 {
			continue
		}
		name, ok := scope.serviceForTraefik(r.Metric["service"])
		if !ok {
			continue
		}
		v, ok := sampleValue(r.Value[1])
		if ok {
			out[name] += int64(v)
		}
	}
	return out, nil
}

func (m *MetricStore) rangeQuery(ctx context.Context, query string, w Window) ([]promResult, error) {
	return m.query(ctx, "/api/v1/query_range", url.Values{
		"query": {query},
		"start": {strconv.FormatInt(w.Start.Unix(), 10)},
		"end":   {strconv.FormatInt(w.End.Unix(), 10)},
		"step":  {promDuration(w.Step)},
	})
}

type promResult struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
	Value  []any             `json:"value"`
}

func (m *MetricStore) query(ctx context.Context, path string, form url.Values) ([]promResult, error) {
	result, _, err := m.queryPartial(ctx, path, form)
	return result, err
}

// queryPartial is query, and whether the store said its answer is partial: a
// clustered VictoriaMetrics with a storage node away answers from the rest
// and says so. A chart may draw that; a meter must not count it.
func (m *MetricStore) queryPartial(ctx context.Context, path string, form url.Values) ([]promResult, bool, error) {
	body, err := post(ctx, m.client, m.base, path, form)
	if err != nil {
		return nil, false, err
	}
	defer body.Close()
	var resp struct {
		Status    string `json:"status"`
		Error     string `json:"error"`
		IsPartial bool   `json:"isPartial"`
		Data      struct {
			Result []promResult `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, false, fmt.Errorf("observe: read the metrics store's answer: %w", err)
	}
	if resp.Status != "success" {
		return nil, false, fmt.Errorf("observe: the metrics store refused the query: %s", resp.Error)
	}
	return resp.Data.Result, resp.IsPartial, nil
}

// name turns the store's series into the chart's, labelled by service or by
// whatever the panel is split by, and drops any whose label does not belong
// to the scope — which the query's own matcher means none can.
func name(scope Scope, by string, results []promResult) []Series {
	var out []Series
	for _, r := range results {
		label := r.Metric[by]
		var ok bool
		switch by {
		case "namespace":
			label, ok = scope.ServiceFor(label)
		case "service":
			label, ok = scope.serviceForTraefik(label)
		case "quantile":
			label, ok = quantileName(label), label != ""
		default:
			ok = label != ""
		}
		if !ok {
			continue
		}
		s := Series{Name: label}
		for _, v := range r.Values {
			ts, ok := v[0].(float64)
			if !ok {
				continue
			}
			val, ok := sampleValue(v[1])
			if !ok {
				continue
			}
			s.Points = append(s.Points, Point{At: time.Unix(int64(ts), 0).UTC(), V: val})
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// quantileName is how a percentile is said: p50 rather than 0.5.
func quantileName(q string) string {
	f, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return q
	}
	return "p" + strings.TrimSuffix(strconv.FormatFloat(f*100, 'f', 1, 64), ".0")
}

// sampleValue reads a sample, which the API sends as a string, and refuses
// NaN and the infinities: a chart cannot place them, and every one of them
// here means "no traffic to divide by".
func sampleValue(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
