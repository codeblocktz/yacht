package observe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
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
// platform's own traffic — the dashboard, the waker, the buckets' S3 API —
// which Traefik counts under services in no app's namespace, and which is
// dropped here because no app is named by them.
//
// What the counter cannot tell apart is a request from inside the cluster
// made to an app's public hostname: it enters through the front door like
// any other and is counted like any other. Nor does it see what an app sends
// out on its own account — calls it makes to other services on the internet —
// or what a TCP proxy carries, which kube-proxy forwards without Traefik.

// ErrWindowOpen is a window that ended too recently for the store to have
// every sample in it yet. Asked again a minute later, it has.
var ErrWindowOpen = errors.New("observe: the window ended too recently to have been read in full")

// egressSettle is how long after a window ends before it is read. The store
// moves an instant query for a moment within its latency offset (thirty
// seconds, -search.latencyOffset) back by that much, which would end the
// window early; and the last scrape before the end must have been written.
const egressSettle = time.Minute

// egressBridge is how far before a window's start a counter's earlier sample
// is still found. A window's bytes are the counter's rise from that sample to
// the last one in the window, so a gap in scraping across the start — the
// store restarting, say — shorter than this loses nothing: what was sent
// during it is counted in the window its first sample after falls in. A
// longer gap loses what was sent during it, and nothing is counted twice.
const egressBridge = time.Hour

// egressMetric is Traefik's counter of the bytes in every response, per
// service, and per status, method and protocol beside.
const egressMetric = "traefik_service_responses_bytes_total"

// Egress is the bytes each of services sent to the internet through the
// ingress in the window from..to, keyed by the service as given. A service
// that sent nothing is absent. Port is ignored: an app is matched by its
// namespace and name, so one whose port changed within the window has what
// it sent under each.
//
// Install-wide, unlike everything else this package reads: it is for the
// operator's meter, which charges every team, and is never reached from a
// request. The store is asked for every service's bytes and only the
// services given are kept, so the cost of the query does not grow with the
// number of apps and no one's traffic is assigned to anyone but the app whose
// namespace it was counted under.
//
// The window is whole seconds; from and to are truncated to them. A sample
// belongs to it when it was taken after from and at or before to, so windows
// that meet divide the samples between them, with none in both or neither: a
// day read as twenty-four hours is the day read at once. The counter is
// integrated by the store's increase(), which reads a fall in a counter as a
// restart and counts from zero after it; every Traefik replica, status,
// method and protocol has a counter of its own, each request is counted by
// the one replica that served it, and they are summed. A Traefik replica
// that starts during a window counts from zero, except — the store's rule —
// a first sample far larger than the counter's next rise, which is taken as
// a counter that began before anything read it and is not counted. The bytes
// a replica counted after its last scrape and before it stopped are not seen
// by anything and are not counted.
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

	// What each counter rose by from the bridge to the end, less what it had
	// risen by at the start. Both are measured from the same earlier sample,
	// so the difference is the rise between samples taken within the window,
	// and from the last one before it to the first one in it.
	start := from.Add(-egressBridge)
	through, err := m.increaseBy(ctx, start, to)
	if err != nil {
		return nil, err
	}
	before, err := m.increaseBy(ctx, start, from)
	if err != nil {
		return nil, err
	}

	byKey := map[string]Service{}
	ambiguous := map[string]bool{}
	for _, svc := range services {
		key := svc.Namespace + "-" + svc.Name
		if prev, ok := byKey[key]; ok && (prev.Namespace != svc.Namespace || prev.Name != svc.Name) {
			// Only namespaces of the engine's fixed width rule this out; with
			// any other, two apps could share the label and neither is given
			// the other's traffic.
			ambiguous[key] = true
		}
		byKey[key] = svc
	}
	for label, n := range through {
		sent := math.Round(n - before[label])
		if sent <= 0 {
			continue
		}
		key, ok := egressKey(label)
		if !ok || ambiguous[key] {
			continue
		}
		if svc, ok := byKey[key]; ok {
			out[svc] += int64(sent)
		}
	}
	return out, nil
}

// increaseBy is how much each service's response bytes rose by from start to
// end, by Traefik service label, from one instant query at end.
//
// nocache, so that a window read twice is computed from the samples twice,
// and never from a result the store kept for a query that ran while the
// window was still open.
func (m *MetricStore) increaseBy(ctx context.Context, start, end time.Time) (map[string]float64, error) {
	query := fmt.Sprintf(`sum by (service) (increase(%s[%s]))`, egressMetric, promDuration(end.Sub(start)))
	result, partial, err := m.queryPartial(ctx, "/api/v1/query", url.Values{
		"query":   {query},
		"time":    {strconv.FormatInt(end.Unix(), 10)},
		"nocache": {"1"},
	})
	if err != nil {
		return nil, err
	}
	if partial {
		return nil, errors.New("observe: the metrics store answered with only part of its data")
	}
	out := map[string]float64{}
	for _, r := range result {
		if len(r.Value) != 2 {
			continue
		}
		if v, ok := sampleValue(r.Value[1]); ok {
			out[r.Metric["service"]] += v
		}
	}
	return out, nil
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
