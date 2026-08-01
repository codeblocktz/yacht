package orchestrator

import "time"

// Cluster inspection types.
//
// These deliberately use plain units — millicores and bytes — rather than the
// quantity strings Kubernetes uses on the wire. Formatting for humans is the
// caller's job, and a UI should not have to parse "2011468Ki" to draw a bar.

// NodeInfo describes one machine in the runtime.
type NodeInfo struct {
	Name    string
	Ready   bool
	Roles   []string
	Pool    string // scheduling pool, from the yacht/pool label
	Address string

	Version      string // kubelet version
	OS           string
	Architecture string

	CPUCapacityMillis int64
	CPUUsedMillis     int64
	MemCapacityBytes  int64
	MemUsedBytes      int64

	Pods          int
	PodCapacity   int
	CreatedAt     time.Time
	UsageKnown    bool // false when no metrics source is available
	Unschedulable bool
}

// CPUPercent returns CPU utilisation, or 0 when usage is unknown.
func (n NodeInfo) CPUPercent() int {
	if !n.UsageKnown || n.CPUCapacityMillis == 0 {
		return 0
	}
	return int(n.CPUUsedMillis * 100 / n.CPUCapacityMillis)
}

// MemoryPercent returns memory utilisation, or 0 when usage is unknown.
func (n NodeInfo) MemoryPercent() int {
	if !n.UsageKnown || n.MemCapacityBytes == 0 {
		return 0
	}
	return int(n.MemUsedBytes * 100 / n.MemCapacityBytes)
}

// PodInfo describes one running pod.
type PodInfo struct {
	Name      string
	Namespace string
	Phase     string
	Node      string
	Ready     int32
	Total     int32
	Restarts  int32
	CreatedAt time.Time

	// App is the workload this pod belongs to, empty for pods the engine
	// does not manage.
	App   string
	Owner OwnerID

	// Reason and Message are why a container is not running, taken from the
	// container's own waiting state.
	//
	// Kubernetes explains itself here the way it does in events, and the phase
	// alone does not: "Pending" and "CreateContainerConfigError" are the same
	// picture to somebody watching a deploy, and only one of them says the
	// image runs as root and the cluster refuses it. Empty while the container
	// is running or has never been asked to start.
	Reason  string
	Message string

	// DrainMoves reports whether draining this pod's node would move it.
	//
	// Decided where the drain is implemented rather than by a caller reading
	// fields and re-deriving the rule. A page that counted differently from
	// the drain would call a node empty while the drain still had work, and
	// the two would drift the first time either changed.
	DrainMoves bool
}

// Healthy reports whether every container in the pod is ready.
func (p PodInfo) Healthy() bool { return p.Total > 0 && p.Ready == p.Total }

// ClusterSummary is the headline state of the runtime.
type ClusterSummary struct {
	Nodes      int
	NodesReady int

	Pods        int
	PodCapacity int

	CPUUsedMillis     int64
	CPUCapacityMillis int64
	MemUsedBytes      int64
	MemCapacityBytes  int64

	Volumes    int
	UsageKnown bool
}

// CPUPercent returns cluster CPU utilisation.
func (c ClusterSummary) CPUPercent() int {
	if !c.UsageKnown || c.CPUCapacityMillis == 0 {
		return 0
	}
	return int(c.CPUUsedMillis * 100 / c.CPUCapacityMillis)
}

// MemoryPercent returns cluster memory utilisation.
func (c ClusterSummary) MemoryPercent() int {
	if !c.UsageKnown || c.MemCapacityBytes == 0 {
		return 0
	}
	return int(c.MemUsedBytes * 100 / c.MemCapacityBytes)
}

// PodListOptions narrows a pod query.
type PodListOptions struct {
	// Namespace limits results. Empty means every namespace.
	Namespace string

	// ManagedOnly restricts results to workloads the engine created.
	ManagedOnly bool

	// Node limits results to pods scheduled on one machine. Empty means every
	// node.
	Node string

	// Owner limits results to one principal's workloads.
	//
	// Empty means every owner, which is right for an operator view and wrong
	// for anything a member can reach. These pages were written when the engine
	// had one owner and everything in the cluster belonged to whoever was
	// looking; teams made that false, and a cluster page is the easiest place
	// to forget it.
	Owner OwnerID
}

// EventInfo is one cluster event.
//
// Events are where Kubernetes explains itself. "Pod is Pending" tells an
// operator nothing; "Back-off pulling image … ErrImagePull" or "Insufficient
// cpu" tells them exactly what to fix. Surfacing them is the difference
// between a dashboard that reports a problem and one that helps.
type EventInfo struct {
	Namespace string
	Type      string // Normal | Warning
	Reason    string
	Message   string
	Object    string // e.g. "Pod/web-7d9f4b6c85-2xk9p"
	Count     int32
	LastSeen  time.Time
}

// Warning reports whether the event is an anomaly rather than routine chatter.
func (e EventInfo) Warning() bool { return e.Type == "Warning" }

// VolumeInfo is one persistent volume claim.
type VolumeInfo struct {
	Name          string
	Namespace     string
	Phase         string // Pending | Bound | Lost
	StorageClass  string
	CapacityBytes int64
	RequestBytes  int64
	AccessModes   []string
	App           string
	Owner         OwnerID
	CreatedAt     time.Time
}

// Bound reports whether the claim has been satisfied.
func (v VolumeInfo) Bound() bool { return v.Phase == "Bound" }
