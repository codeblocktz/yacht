package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Egress for metering: the bytes each app sent to the internet in a window,
// exactly enough to charge for.
//
// Public egress is what PublicEgress and the Observability page already call
// it: the response bytes Traefik counts per service, which is what the
// platform's front door carried out to whoever asked. Traffic between apps
// goes Service to Service on the cluster's own network and never passes
// through Traefik, so it is not in the counter to begin with; nor is the
// platform's own traffic — the dashboard, the buckets' S3 API — which Traefik
// counts under services in no app's namespace, and which is dropped here
// because no app is named by them. A sleeping app's requests go to its waker
// Service, in the app's own namespace; what the waker sent for it is the
// app's.
//
// What the counter cannot tell apart is a request from inside the cluster
// made to an app's public hostname: it enters through the front door like
// any other and is counted like any other. Nor does it see what an app sends
// out on its own account — calls it makes to other services on the internet —
// or what a TCP proxy carries, which kube-proxy forwards without Traefik.
//
// Why the counters are read raw and integrated here, rather than with the
// store's increase(): increase() has to guess what a counter was before its
// first sample, and guesses wrong both ways for what Traefik does. Traefik
// drops a service's counters when the service leaves its configuration —
// every time an app sleeps, its route moves to the waker — and starts them
// again from zero when it comes back, so the first sample after a wake is all
// traffic; increase() discards a first sample much larger than the next rise,
// which is exactly a wake's first burst. increase_pure() counts every first
// sample from zero instead, which charges a counter's whole life after the
// store has been away. Neither can know what is known here: whether the
// store was scraping the Traefik replica, and the series was not in what it
// served (it was gone, and came back from zero), or the store was not
// scraping at all (the counter ran on, and its rise across the gap is the
// traffic of the gap).

// ErrWindowOpen is a window that ended too recently for the store to have
// every sample in it yet. Asked again a minute later, it has.
var ErrWindowOpen = errors.New("observe: the window ended too recently to have been read in full")

// egressSettle is how long after a window ends before it is read: a scrape
// interval for the last sample before the end to have been written, and
// margin for the store to make it searchable.
const egressSettle = time.Minute

// egressBridge is how far before a window's start a counter's last sample is
// still found, and what the scraper did since is read. A gap in scraping
// shorter than this — the store down for a day — loses nothing: what was sent
// during it is the counter's rise across it, counted in the window the first
// sample after falls in. It is longer than a day so that a meter reading the
// hours after an outage of up to a day reads every one of them the same way.
const egressBridge = 25 * time.Hour

// egressMetric is Traefik's counter of the bytes in every response, per
// service, and per status, method and protocol beside.
const egressMetric = "traefik_service_responses_bytes_total"

// Egress is the bytes each of services sent to the internet through the
// ingress in the window from..to, keyed by the service as given. A service
// that sent nothing is absent. Port is ignored: an app is matched by its
// namespace and name, so one whose port changed within the window has what
// it sent under each, and what its waker sent while it slept.
//
// Install-wide, unlike everything else this package reads: it is for the
// operator's meter, which charges every team, and is never reached from a
// request. The store is asked for every service's counters and only the
// services given are kept, so no one's traffic is assigned to anyone but the
// app whose namespace it was counted under.
//
// The window is whole seconds; from and to are truncated to them. A sample
// belongs to it when it was taken after from and at or before to, so windows
// that meet divide the samples between them, with none in both or neither: a
// day read as twenty-four hours is the day read at once.
//
// Each counter — one per Traefik replica, service, status, method and
// protocol — is integrated on its own and the counters summed: each request
// is counted by the one replica that served it, so none is counted twice. A
// sample's share is its rise over the counter's sample before, or after a
// fall — the replica restarting — its whole value. A counter that was not in
// a scrape its replica answered between two samples had been dropped, and
// counts its whole value again; one not scraped at all between them ran on,
// and counts the rise. A counter's first sample is counted whole when the
// store was scraping its replica, or the replica's siblings, before it: the
// counter is new. When the store was scraping nothing of that job for the
// whole bridge, the first sample is not counted — it may hold a day's traffic
// that nothing can place.
//
// Reading a window again reads the same samples and gives the same answer,
// for as long as the store keeps them. A window that ended less than a minute
// ago is refused with ErrWindowOpen rather than read short; a store that
// cannot be reached, or answers with only part of its data, is an error
// rather than nothing, so a meter tries again instead of charging too little.
func (m *MetricStore) Egress(ctx context.Context, from, to time.Time, services []Service) (map[Service]int64, error) {
	if m == nil {
		return nil, ErrNotConfigured
	}
	from, to = from.Truncate(time.Second), to.Truncate(time.Second)
	if to.Before(from) {
		return nil, fmt.Errorf("observe: an egress window ends (%s) before it starts (%s)",
			to.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339))
	}
	if m.now().Sub(to) < egressSettle {
		return nil, ErrWindowOpen
	}
	out := map[Service]int64{}
	if !to.After(from) || len(services) == 0 {
		return out, nil
	}

	window, err := m.export(ctx, egressMetric, from, to)
	if err != nil {
		return nil, err
	}
	if len(window) == 0 {
		return out, nil
	}
	before, err := m.lastBefore(ctx, from)
	if err != nil {
		return nil, err
	}
	start := from.Add(-egressBridge)
	scrapes, err := m.scrapes(ctx, window, start, to)
	if err != nil {
		return nil, err
	}

	sent := map[string]float64{}
	after, through := from.UnixMilli(), to.UnixMilli()
	for key, s := range window {
		var prev *rawSample
		if b, ok := before[key]; ok {
			prev = &b
		}
		target := scrapes.of(s.labels)
		for _, c := range s.samples {
			if c.ms <= after || c.ms > through {
				continue
			}
			sent[s.labels["service"]] += target.rise(prev, c)
			prev = &c
		}
	}
	return attribute(sent, services), nil
}

// rawSample is one stored sample: Unix milliseconds and a value.
type rawSample struct {
	ms int64
	v  float64
}

// rawSeries is one counter: its labels, and its samples in time order.
type rawSeries struct {
	labels  map[string]string
	samples []rawSample
}

// seriesKey names a series by its labels, without the metric name, which the
// store keeps on some answers and drops from others.
func seriesKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		if k != "__name__" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(strconv.Quote(k))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(labels[k]))
		b.WriteByte(',')
	}
	return b.String()
}

// export is every sample of the series match selects from start to end, both
// included, as the store holds them: no rollup, no guess. The store may send
// a series in several pieces; they are put back together.
func (m *MetricStore) export(ctx context.Context, match string, start, end time.Time) (map[string]*rawSeries, error) {
	body, err := post(ctx, m.client, m.base, "/api/v1/export", url.Values{
		"match[]": {match},
		"start":   {strconv.FormatInt(start.Unix(), 10)},
		"end":     {strconv.FormatInt(end.Unix(), 10)},
	})
	if err != nil {
		return nil, err
	}
	defer body.Close()
	out := map[string]*rawSeries{}
	dec := json.NewDecoder(body)
	for {
		var line struct {
			Metric     map[string]string `json:"metric"`
			Values     []float64         `json:"values"`
			Timestamps []int64           `json:"timestamps"`
		}
		if err := dec.Decode(&line); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("observe: read the metrics store's export: %w", err)
		}
		if len(line.Values) != len(line.Timestamps) {
			return nil, errors.New("observe: the metrics store exported a series with a value missing")
		}
		key := seriesKey(line.Metric)
		s := out[key]
		if s == nil {
			s = &rawSeries{labels: line.Metric}
			out[key] = s
		}
		for i, v := range line.Values {
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				s.samples = append(s.samples, rawSample{ms: line.Timestamps[i], v: v})
			}
		}
	}
	for _, s := range out {
		sort.Slice(s.samples, func(i, j int) bool { return s.samples[i].ms < s.samples[j].ms })
	}
	return out, nil
}

// lastBefore is each counter's last sample at or before at, within the
// bridge: its value and when it was taken, from two instant queries over the
// same series.
func (m *MetricStore) lastBefore(ctx context.Context, at time.Time) (map[string]rawSample, error) {
	look := promDuration(egressBridge)
	values, err := m.instantBySeries(ctx, fmt.Sprintf(`last_over_time(%s[%s])`, egressMetric, look), at)
	if err != nil {
		return nil, err
	}
	times, err := m.instantBySeries(ctx, fmt.Sprintf(`tlast_over_time(%s[%s])`, egressMetric, look), at)
	if err != nil {
		return nil, err
	}
	out := make(map[string]rawSample, len(values))
	for key, v := range values {
		ts, ok := times[key]
		if !ok {
			// The same series in both answers, always; were one ever missing,
			// the sample is taken as at the window's start, which can only
			// make a gap look shorter and count less.
			ts = float64(at.Unix())
		}
		out[key] = rawSample{ms: int64(math.Round(ts * 1000)), v: v}
	}
	return out, nil
}

// instantBySeries is an instant query's answer by series.
func (m *MetricStore) instantBySeries(ctx context.Context, query string, at time.Time) (map[string]float64, error) {
	result, partial, err := m.queryPartial(ctx, "/api/v1/query", url.Values{
		"query":   {query},
		"time":    {strconv.FormatInt(at.Unix(), 10)},
		"nocache": {"1"},
	})
	if err != nil {
		return nil, err
	}
	if partial {
		return nil, errors.New("observe: the metrics store answered with only part of its data")
	}
	out := make(map[string]float64, len(result))
	for _, r := range result {
		if len(r.Value) != 2 {
			continue
		}
		if v, ok := sampleValue(r.Value[1]); ok {
			out[seriesKey(r.Metric)] = v
		}
	}
	return out, nil
}

// scrapeRecord is what the scraper did, from its own up series: for each
// target (job and instance), when it scraped and whether the target
// answered; and for each job, when it scraped any of the job's targets.
type scrapeRecord struct {
	targets map[[2]string][]rawSample
	jobs    map[string][]int64
}

// scrapes reads the up series of every job the counters were scraped by. The
// scraper writes it for each scrape of each target, at the same moment as
// what the scrape returned, whatever the scrape configuration keeps.
func (m *MetricStore) scrapes(ctx context.Context, counters map[string]*rawSeries, start, end time.Time) (scrapeRecord, error) {
	rec := scrapeRecord{targets: map[[2]string][]rawSample{}, jobs: map[string][]int64{}}
	seen := map[string]bool{}
	var jobs []string
	for _, s := range counters {
		if j := s.labels["job"]; j != "" && !seen[j] {
			seen[j] = true
			jobs = append(jobs, regexp.QuoteMeta(j))
		}
	}
	if len(jobs) == 0 {
		return rec, nil
	}
	sort.Strings(jobs)
	up, err := m.export(ctx, `up{job=~`+strconv.Quote(strings.Join(jobs, "|"))+`}`, start, end)
	if err != nil {
		return rec, err
	}
	for _, s := range up {
		job := s.labels["job"]
		rec.targets[[2]string{job, s.labels["instance"]}] = s.samples
		for _, x := range s.samples {
			rec.jobs[job] = append(rec.jobs[job], x.ms)
		}
	}
	for j := range rec.jobs {
		sort.Slice(rec.jobs[j], func(a, b int) bool { return rec.jobs[j][a] < rec.jobs[j][b] })
	}
	return rec, nil
}

// targetScrapes is the scrape record of one counter's target.
type targetScrapes struct {
	up  []rawSample // the target's own scrapes: 1 answered, 0 did not
	job []int64     // every scrape of the job's targets
}

func (r scrapeRecord) of(labels map[string]string) targetScrapes {
	job := labels["job"]
	return targetScrapes{up: r.targets[[2]string{job, labels["instance"]}], job: r.jobs[job]}
}

// rise is what sample c adds to its counter, prev being the counter's sample
// before it, or nil for none within the bridge.
func (t targetScrapes) rise(prev *rawSample, c rawSample) float64 {
	if prev == nil {
		switch {
		case t.answeredBetween(math.MinInt64, c.ms):
			// The replica answered a scrape without this counter in it:
			// the counter began since, from zero.
			return c.v
		case !t.scrapedBetween(math.MinInt64, c.ms) && t.jobScrapedBefore(c.ms):
			// A replica the store had not seen before, while it was scraping
			// the others: new, and so is every counter in it.
			return c.v
		}
		// The store scraped nothing of the job before this, or never
		// reached the replica: what the counter held is not placeable.
		return 0
	}
	if t.answeredBetween(prev.ms, c.ms) {
		// Missing from a scrape the replica answered: dropped, and begun
		// again from zero.
		return c.v
	}
	if c.v >= prev.v {
		return c.v - prev.v
	}
	// A fall is the replica restarting: everything since is new.
	return c.v
}

// answeredBetween reports whether the target answered a scrape strictly
// between two moments. A scrape and what it returned share one timestamp, so
// two samples of a counter one scrape apart have none between them.
func (t targetScrapes) answeredBetween(after, before int64) bool {
	i := sort.Search(len(t.up), func(i int) bool { return t.up[i].ms > after })
	for ; i < len(t.up) && t.up[i].ms < before; i++ {
		if t.up[i].v == 1 {
			return true
		}
	}
	return false
}

// scrapedBetween reports whether the target was scraped at all, answered or
// not, strictly between two moments.
func (t targetScrapes) scrapedBetween(after, before int64) bool {
	i := sort.Search(len(t.up), func(i int) bool { return t.up[i].ms > after })
	return i < len(t.up) && t.up[i].ms < before
}

// jobScrapedBefore reports whether any of the job's targets was scraped
// before a moment, within what was read.
func (t targetScrapes) jobScrapedBefore(before int64) bool {
	return len(t.job) > 0 && t.job[0] < before
}

// attribute gives each service what was sent under its labels: its own
// Service on any port, and its waker's.
func attribute(sent map[string]float64, services []Service) map[Service]int64 {
	byKey := map[string]Service{}
	ambiguous := map[string]bool{}
	claim := func(key string, svc Service) {
		if prev, ok := byKey[key]; ok && (prev.Namespace != svc.Namespace || prev.Name != svc.Name) {
			// Only namespaces of the engine's fixed width rule this out; with
			// any other, two apps could share the label and neither is given
			// the other's traffic.
			ambiguous[key] = true
		}
		byKey[key] = svc
	}
	for _, svc := range services {
		key := svc.Namespace + "-" + svc.Name
		claim(key, svc)
		claim(key+"-waker", svc)
	}
	out := map[Service]int64{}
	for label, n := range sent {
		key, ok := egressKey(label)
		if !ok || ambiguous[key] {
			continue
		}
		svc, ok := byKey[key]
		if !ok {
			continue
		}
		if b := int64(math.Round(n)); b > 0 {
			out[svc] += b
		}
	}
	return out
}

// egressKey is the namespace and name in a Traefik service label, without the
// port: "yacht-…-web-8080@kubernetes" is "yacht-…-web". Only the Kubernetes
// Ingress provider's labels have one; any other is the platform's.
func egressKey(label string) (string, bool) {
	base, ok := strings.CutSuffix(label, "@kubernetes")
	if !ok {
		return "", false
	}
	i := strings.LastIndexByte(base, '-')
	if i <= 0 {
		return "", false
	}
	if _, err := strconv.ParseUint(base[i+1:], 10, 16); err != nil {
		return "", false
	}
	return base[:i], true
}
