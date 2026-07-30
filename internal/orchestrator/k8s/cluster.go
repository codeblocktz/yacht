package k8s

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// poolLabel marks a node as belonging to a scheduling pool.
const poolLabel = "yacht/pool"

// ClusterSummary returns headline capacity and utilisation.
func (o *Orchestrator) ClusterSummary(ctx context.Context) (orchestrator.ClusterSummary, error) {
	nodes, err := o.Nodes(ctx)
	if err != nil {
		return orchestrator.ClusterSummary{}, err
	}

	summary := orchestrator.ClusterSummary{UsageKnown: o.metrics != nil}
	for _, n := range nodes {
		summary.Nodes++
		if n.Ready {
			summary.NodesReady++
		}
		summary.Pods += n.Pods
		summary.PodCapacity += n.PodCapacity
		summary.CPUCapacityMillis += n.CPUCapacityMillis
		summary.CPUUsedMillis += n.CPUUsedMillis
		summary.MemCapacityBytes += n.MemCapacityBytes
		summary.MemUsedBytes += n.MemUsedBytes
	}

	// Volume count is best-effort: a restricted credential may not be able to
	// list PVCs cluster-wide, and that should degrade the number rather than
	// fail the whole page.
	if pvcs, err := o.client.CoreV1().PersistentVolumeClaims("").
		List(ctx, metav1.ListOptions{}); err == nil {
		summary.Volumes = len(pvcs.Items)
	}

	return summary, nil
}

// Nodes lists the machines backing the cluster.
func (o *Orchestrator) Nodes(ctx context.Context) ([]orchestrator.NodeInfo, error) {
	list, err := o.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: list nodes: %w", err)
	}

	// Pods per node come from one cluster-wide list rather than a query per
	// node: on a large cluster that is the difference between one API call and
	// one per machine.
	podsByNode := map[string]int{}
	if pods, err := o.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			podsByNode[p.Spec.NodeName]++
		}
	}

	usage := o.nodeUsage(ctx)

	out := make([]orchestrator.NodeInfo, 0, len(list.Items))
	for _, n := range list.Items {
		info := orchestrator.NodeInfo{
			Name:              n.Name,
			Ready:             nodeReady(n),
			Roles:             nodeRoles(n),
			Pool:              n.Labels[poolLabel],
			Address:           nodeAddress(n),
			Version:           n.Status.NodeInfo.KubeletVersion,
			OS:                n.Status.NodeInfo.OperatingSystem,
			Architecture:      n.Status.NodeInfo.Architecture,
			CPUCapacityMillis: n.Status.Capacity.Cpu().MilliValue(),
			MemCapacityBytes:  n.Status.Capacity.Memory().Value(),
			Pods:              podsByNode[n.Name],
			PodCapacity:       int(n.Status.Capacity.Pods().Value()),
			CreatedAt:         n.CreationTimestamp.Time,
			Unschedulable:     n.Spec.Unschedulable,
		}
		if u, ok := usage[n.Name]; ok {
			info.CPUUsedMillis = u.cpuMillis
			info.MemUsedBytes = u.memBytes
			info.UsageKnown = true
		}
		out = append(out, info)
	}

	slices.SortFunc(out, func(a, b orchestrator.NodeInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

type nodeUsageSample struct {
	cpuMillis int64
	memBytes  int64
}

// nodeUsage reads live utilisation from metrics-server.
//
// Returns an empty map rather than an error when metrics are unavailable.
// metrics-server is optional in a K3s install, and a dashboard that refuses to
// render because it cannot draw a percentage bar is worse than one that omits
// the bar.
func (o *Orchestrator) nodeUsage(ctx context.Context) map[string]nodeUsageSample {
	if o.metrics == nil {
		return nil
	}
	list, err := o.metrics.MetricsV1beta1().NodeMetricses().
		List(ctx, metav1.ListOptions{})
	if err != nil {
		o.log.Debug("node metrics unavailable", "error", err.Error())
		return nil
	}

	out := make(map[string]nodeUsageSample, len(list.Items))
	for _, m := range list.Items {
		out[m.Name] = nodeUsageSample{
			cpuMillis: m.Usage.Cpu().MilliValue(),
			memBytes:  m.Usage.Memory().Value(),
		}
	}
	return out
}

// Pods lists running pods.
func (o *Orchestrator) Pods(
	ctx context.Context, opts orchestrator.PodListOptions,
) ([]orchestrator.PodInfo, error) {
	listOpts := metav1.ListOptions{}
	if opts.ManagedOnly {
		listOpts.LabelSelector = fmt.Sprintf("%s=%s",
			orchestrator.LabelManagedBy, orchestrator.ManagedByValue)
	}

	list, err := o.client.CoreV1().Pods(opts.Namespace).List(ctx, listOpts)
	if err != nil {
		return nil, fmt.Errorf("k8s: list pods: %w", err)
	}

	out := make([]orchestrator.PodInfo, 0, len(list.Items))
	for _, p := range list.Items {
		info := orchestrator.PodInfo{
			Name:      p.Name,
			Namespace: p.Namespace,
			Phase:     string(p.Status.Phase),
			Node:      p.Spec.NodeName,
			Total:     int32(len(p.Status.ContainerStatuses)),
			CreatedAt: p.CreationTimestamp.Time,
			App:       p.Labels[orchestrator.LabelApp],
			Owner:     orchestrator.OwnerID(p.Labels[orchestrator.LabelOwner]),
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				info.Ready++
			}
			info.Restarts += cs.RestartCount
		}
		out = append(out, info)
	}

	slices.SortFunc(out, func(a, b orchestrator.PodInfo) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// nodeRoles extracts roles from the conventional node-role.kubernetes.io/*
// label prefix.
func nodeRoles(n corev1.Node) []string {
	const prefix = "node-role.kubernetes.io/"
	var roles []string
	for k := range n.Labels {
		if role, ok := strings.CutPrefix(k, prefix); ok && role != "" {
			roles = append(roles, role)
		}
	}
	slices.Sort(roles)
	if len(roles) == 0 {
		roles = []string{"worker"}
	}
	return roles
}

// nodeAddress prefers the internal IP, which is what other nodes reach.
func nodeAddress(n corev1.Node) string {
	var fallback string
	for _, a := range n.Status.Addresses {
		switch a.Type {
		case corev1.NodeInternalIP:
			return a.Address
		case corev1.NodeExternalIP, corev1.NodeHostName:
			if fallback == "" {
				fallback = a.Address
			}
		}
	}
	return fallback
}
