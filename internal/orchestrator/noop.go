package orchestrator

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

// Noop is an in-memory Orchestrator.
//
// It exists so the control plane can boot, and its HTTP handlers can be tested,
// without a cluster. It is not a simulator: it records what it was asked to do
// and reports workloads as running.
type Noop struct {
	mu         sync.Mutex
	namespaces map[string]NamespaceSpec
	apps       map[string]AppSpec
}

// Compile-time check that Noop satisfies the interface.
var _ Orchestrator = (*Noop)(nil)

// NewNoop returns an empty in-memory orchestrator.
func NewNoop() *Noop {
	return &Noop{
		namespaces: make(map[string]NamespaceSpec),
		apps:       make(map[string]AppSpec),
	}
}

func (n *Noop) EnsureNamespace(_ context.Context, spec NamespaceSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	spec.Limits = spec.Limits.OrDefaults()
	n.namespaces[spec.Name] = spec
	return nil
}

func (n *Noop) DeleteNamespace(_ context.Context, name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.namespaces, name)
	for key, spec := range n.apps {
		if spec.Namespace == name {
			delete(n.apps, key)
		}
	}
	return nil
}

func (n *Noop) ApplyApp(_ context.Context, spec AppSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.apps[spec.Ref.String()] = spec
	return nil
}

func (n *Noop) DeleteApp(_ context.Context, ref Ref) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.apps, ref.String())
	return nil
}

func (n *Noop) AppStatus(_ context.Context, ref Ref) (AppStatus, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	spec, ok := n.apps[ref.String()]
	if !ok {
		return AppStatus{}, ErrNotFound
	}
	if spec.Replicas == 0 {
		return AppStatus{Phase: PhaseStopped}, nil
	}
	return AppStatus{
		Phase:     PhaseRunning,
		Desired:   spec.Replicas,
		Ready:     spec.Replicas,
		Available: spec.Replicas,
	}, nil
}

func (n *Noop) Ping(context.Context) error { return nil }

// ClusterSummary reports what was applied, not a simulated cluster.
//
// Node and capacity figures stay zero because there is no cluster to report
// on. Inventing plausible numbers here would make a disconnected install look
// healthy, which is precisely the state an operator needs to notice.
func (n *Noop) ClusterSummary(context.Context) (ClusterSummary, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	var pods int
	for _, spec := range n.apps {
		pods += int(spec.Replicas)
	}
	return ClusterSummary{Pods: pods}, nil
}

// Nodes returns nothing: an in-memory orchestrator has no machines.
func (n *Noop) Nodes(context.Context) ([]NodeInfo, error) { return nil, nil }

// Events returns nothing: there is no cluster to emit any.
func (n *Noop) Events(context.Context, int) ([]EventInfo, error) { return nil, nil }

// Volumes returns nothing: the engine's in-memory mode provisions no storage.
func (n *Noop) Volumes(context.Context, OwnerID) ([]VolumeInfo, error) { return nil, nil }

// Pods synthesises one entry per replica of each applied workload, so the
// dashboard has something coherent to render without a cluster.
func (n *Noop) Pods(_ context.Context, opts PodListOptions) ([]PodInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	var out []PodInfo
	for _, spec := range n.apps {
		if opts.Namespace != "" && spec.Namespace != opts.Namespace {
			continue
		}
		for i := range int(spec.Replicas) {
			out = append(out, PodInfo{
				Name:      fmt.Sprintf("%s-%d", spec.Name, i),
				Namespace: spec.Namespace,
				Phase:     "Running",
				Ready:     1,
				Total:     1,
				App:       spec.Name,
				Owner:     spec.Owner,
			})
		}
	}
	slices.SortFunc(out, func(a, b PodInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// Namespaces returns the namespaces recorded so far, sorted. Test helper.
func (n *Noop) Namespaces() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Sorted(maps.Keys(n.namespaces))
}

// Apps returns the workload refs recorded so far, sorted. Test helper.
func (n *Noop) Apps() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Sorted(maps.Keys(n.apps))
}
