package k8s

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// NamespaceUsage reads each namespace's pods from metrics-server and sums
// their containers.
//
// One list per namespace rather than one across the cluster: each namespace is
// one app the caller already resolved by owner, so this reads exactly the pods
// it was asked about and the cluster-wide list — every team's pods — is never
// fetched to be filtered here.
func (o *Orchestrator) NamespaceUsage(
	ctx context.Context, namespaces []string,
) (map[string]orchestrator.Usage, error) {
	if o.metrics == nil {
		return nil, fmt.Errorf("%w: no metrics-server", orchestrator.ErrNotSupported)
	}
	out := make(map[string]orchestrator.Usage, len(namespaces))
	for _, ns := range namespaces {
		list, err := o.metrics.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("k8s: read usage in %s: %w", ns, err)
		}
		var u orchestrator.Usage
		for _, p := range list.Items {
			for _, c := range p.Containers {
				u.CPUMillis += c.Usage.Cpu().MilliValue()
				u.MemoryBytes += c.Usage.Memory().Value()
			}
		}
		if len(list.Items) > 0 {
			out[ns] = u
		}
	}
	return out, nil
}

var _ orchestrator.UsageReader = (*Orchestrator)(nil)
