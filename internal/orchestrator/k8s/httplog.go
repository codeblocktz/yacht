package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// ingressSelectors finds the controller writing the access log.
//
// A list rather than one, because the label an ingress controller carries is
// the controller's choice: k3s ships Traefik, and an install that replaced it
// with ingress-nginx is not a broken install. Tried in order, first match wins.
var ingressSelectors = []struct{ namespace, selector string }{
	{"kube-system", "app.kubernetes.io/name=traefik"},
	{"traefik", "app.kubernetes.io/name=traefik"},
	{"ingress-nginx", "app.kubernetes.io/name=ingress-nginx"},
}

// httpLogTail is how much of the controller's log is read.
//
// The whole cluster's traffic goes through one log, so finding one app's
// requests means reading everyone's and discarding the rest. This is the cost
// of that, and the reason the panel says so: a busy cluster can push an app's
// last request out of this window in seconds.
const httpLogTail = 5000

// HTTPLogs returns the ingress controller's record of requests to some hosts.
//
// The host filter is the whole of the tenancy boundary here, and it is the
// reason this takes hosts rather than an app: one controller logs every
// tenant's traffic, so the caller resolves which hosts belong to the app —
// from an owner-scoped query — and this never decides that for itself.
func (o *Orchestrator) HTTPLogs(
	ctx context.Context, opts orchestrator.HTTPLogOptions,
) (orchestrator.HTTPLogs, error) {
	if len(opts.Hosts) == 0 {
		// No hosts means nothing could match. Returned empty rather than
		// unfiltered, which is the failure that would turn this into a way to
		// read every tenant's traffic.
		return orchestrator.HTTPLogs{}, nil
	}

	pods, ns, err := o.ingressPods(ctx)
	if err != nil {
		return orchestrator.HTTPLogs{}, err
	}
	if len(pods) == 0 {
		return orchestrator.HTTPLogs{Reason: orchestrator.HTTPLogNoController}, nil
	}

	want := make(map[string]bool, len(opts.Hosts))
	for _, h := range opts.Hosts {
		want[strings.ToLower(h)] = true
	}

	out := orchestrator.HTTPLogs{}
	var sawAccessLine bool

	for _, pod := range pods {
		lines, saw := o.readAccessLog(ctx, ns, pod, want)
		out.Lines = append(out.Lines, lines...)
		sawAccessLine = sawAccessLine || saw
	}

	if !sawAccessLine {
		// The controller is there and is not writing access logs. Told apart
		// from "no requests" because they need opposite things done about
		// them: one is a setting, the other is patience.
		out.Reason = orchestrator.HTTPLogNotEnabled
	}
	return out, nil
}

// ingressPods finds the controller's pods.
func (o *Orchestrator) ingressPods(ctx context.Context) ([]string, string, error) {
	for _, s := range ingressSelectors {
		list, err := o.client.CoreV1().Pods(s.namespace).
			List(ctx, metav1.ListOptions{LabelSelector: s.selector})
		if err != nil {
			// A namespace this install does not have is not an error worth
			// stopping for; the next selector may match.
			continue
		}
		var names []string
		for _, p := range list.Items {
			if p.Status.Phase == corev1.PodRunning {
				names = append(names, p.Name)
			}
		}
		if len(names) > 0 {
			return names, s.namespace, nil
		}
	}
	return nil, "", nil
}

// readAccessLog pulls one controller pod's log and keeps the matching requests.
//
// Reports separately whether any access line was seen at all, which is how
// "access logging is switched off" is told from "this app had no traffic" —
// the two look identical from an empty result.
func (o *Orchestrator) readAccessLog(
	ctx context.Context, namespace, pod string, want map[string]bool,
) ([]orchestrator.HTTPLogLine, bool) {
	tail := int64(httpLogTail)
	stream, err := o.client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		TailLines: &tail,
	}).Stream(ctx)
	if err != nil {
		return nil, false
	}
	defer stream.Close() //nolint:errcheck // reading is done

	var (
		out []orchestrator.HTTPLogLine
		saw bool
	)
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line, ok := parseAccessLine(sc.Bytes())
		if !ok {
			continue
		}
		saw = true
		if want[strings.ToLower(line.Host)] {
			out = append(out, line)
		}
	}
	return out, saw
}

// accessLine is the subset of Traefik's JSON access log this reads.
//
// A named subset rather than a map, so a field that changes shape upstream is
// a compile error here rather than a blank column on the page.
type accessLine struct {
	StartUTC    string `json:"StartUTC"`
	Host        string `json:"RequestHost"`
	Method      string `json:"RequestMethod"`
	Path        string `json:"RequestPath"`
	Protocol    string `json:"RequestProtocol"`
	Status      int    `json:"DownstreamStatus"`
	Size        int64  `json:"DownstreamContentSize"`
	DurationNS  int64  `json:"Duration"`
	ClientHost  string `json:"ClientHost"`
	ServiceName string `json:"ServiceName"`
}

// parseAccessLine reads one line, reporting whether it was an access record.
//
// The controller's log holds its own startup and error output alongside the
// access lines, so anything that is not JSON with a request host in it is
// somebody else's line and is skipped rather than shown as a request.
func parseAccessLine(raw []byte) (orchestrator.HTTPLogLine, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return orchestrator.HTTPLogLine{}, false
	}
	var a accessLine
	if err := json.Unmarshal(raw, &a); err != nil || a.Host == "" || a.Method == "" {
		return orchestrator.HTTPLogLine{}, false
	}

	line := orchestrator.HTTPLogLine{
		Host: a.Host, Method: a.Method, Path: a.Path, Protocol: a.Protocol,
		Status: a.Status, Bytes: a.Size, Client: a.ClientHost,
		Duration: time.Duration(a.DurationNS),
	}
	if t, err := time.Parse(time.RFC3339Nano, a.StartUTC); err == nil {
		line.At = t
	}
	return line, true
}

// HTTPLogHint is the configuration that makes this work.
//
// Returned as text for the page to show rather than applied. Yacht does not
// own the ingress controller: it is installed by whoever built the cluster,
// its access log is their setting, and a platform that quietly rewrote it
// would be changing something every other workload depends on.
func (o *Orchestrator) HTTPLogHint() string {
	return `apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: traefik
  namespace: kube-system
spec:
  valuesContent: |-
    logs:
      access:
        enabled: true
        format: json
        fields:
          headers:
            defaultMode: drop`
}
