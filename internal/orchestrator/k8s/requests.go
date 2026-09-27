package k8s

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Request counts, for deciding when an app is idle.
//
// Read from Traefik's own per-service counters rather than from its access
// log. The access log is what the HTTP panel shows, and it is the right thing
// to read a request from; it is the wrong thing to watch for the absence of
// one. Reading it costs one line per request across the whole cluster, it is
// kept only as long as the controller's log is, and a request that fell
// outside what was read would look exactly like no request. A counter costs
// one line per app, however busy, and a count that moved is proof of a
// request however long ago it was last read.

// requestsMetric is Traefik's counter of requests per service.
const requestsMetric = "traefik_service_requests_total"

// defaultMetricsPort is where Traefik's chart serves its Prometheus metrics,
// for a pod that does not name the port.
const defaultMetricsPort = "9100"

// RequestCounts reads every Traefik pod's counters through the API server,
// and totals them per app.
//
// Through the API server's pod proxy rather than by dialling the pod, so it
// works from outside the cluster with a kubeconfig exactly as from inside it.
// Every pod or none: a count missing one pod's share would drop when that
// pod's reading failed and never see the requests it served.
func (o *Orchestrator) RequestCounts(
	ctx context.Context, refs []orchestrator.Ref,
) (map[orchestrator.Ref]int64, error) {
	pods, err := o.traefikPods(ctx)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("%w: no Traefik ingress controller is running to count requests",
			orchestrator.ErrNotSupported)
	}

	want := make(map[string]orchestrator.Ref, len(refs))
	out := make(map[orchestrator.Ref]int64, len(refs))
	for _, ref := range refs {
		want[traefikServiceName(ref)] = ref
		out[ref] = 0
	}

	for _, pod := range pods {
		body, err := o.client.CoreV1().Pods(pod.Namespace).
			ProxyGet("http", pod.Name, metricsPort(pod), "/metrics", nil).DoRaw(ctx)
		if err != nil {
			return nil, fmt.Errorf("k8s: read request counts from %s: %w", pod.Name, err)
		}
		counts, published := parseRequestCounts(body)
		if !published {
			return nil, fmt.Errorf("%w: Traefik is not publishing Prometheus metrics, "+
				"so requests cannot be counted", orchestrator.ErrNotSupported)
		}
		for service, n := range counts {
			if ref, ok := want[service]; ok {
				out[ref] += n
			}
		}
	}
	return out, nil
}

// traefikPods is the running Traefik pods, from the selectors the access log
// is read through. Another controller's are not counted: nothing here reads
// its metrics.
func (o *Orchestrator) traefikPods(ctx context.Context) ([]corev1.Pod, error) {
	for _, s := range ingressSelectors {
		if !strings.Contains(s.selector, "traefik") {
			continue
		}
		list, err := o.client.CoreV1().Pods(s.namespace).
			List(ctx, metav1.ListOptions{LabelSelector: s.selector})
		if err != nil {
			continue
		}
		var running []corev1.Pod
		for _, p := range list.Items {
			if p.Status.Phase == corev1.PodRunning {
				running = append(running, p)
			}
		}
		if len(running) > 0 {
			return running, nil
		}
	}
	return nil, nil
}

// metricsPort is the port the pod names "metrics", or the chart's default.
func metricsPort(p corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			if port.Name == "metrics" {
				return strconv.Itoa(int(port.ContainerPort))
			}
		}
	}
	return defaultMetricsPort
}

// traefikServiceName is how Traefik names the service an app's Ingress routes
// to: namespace, Service, port, and the provider it came from.
func traefikServiceName(ref orchestrator.Ref) string {
	return ref.Namespace + "-" + ref.Name + "-" + strconv.Itoa(int(servicePort)) + "@kubernetes"
}

// parseRequestCounts totals the requests counter per service in a Prometheus
// text exposition, summing its code, method and protocol series. published
// is whether the page is Traefik's at all — a page with none of its metrics
// is a controller that is not publishing them, not one that routed nothing.
func parseRequestCounts(body []byte) (counts map[string]int64, published bool) {
	counts = map[string]int64{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "traefik_") {
			published = true
		}
		if !strings.HasPrefix(line, requestsMetric+"{") {
			continue
		}
		service, value, ok := serviceSample(line)
		if ok {
			counts[service] += value
		}
	}
	return counts, published
}

// serviceSample reads one sample line: its service label and its value.
func serviceSample(line string) (string, int64, bool) {
	end := strings.LastIndexByte(line, '}')
	if end < 0 {
		return "", 0, false
	}
	labels := line[len(requestsMetric)+1 : end]
	const key = `service="`
	at := strings.Index(labels, key)
	if at < 0 {
		return "", 0, false
	}
	rest := labels[at+len(key):]
	quote := strings.IndexByte(rest, '"')
	if quote < 0 {
		return "", 0, false
	}
	fields := strings.Fields(line[end+1:])
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return "", 0, false
	}
	return rest[:quote], int64(v), true
}
