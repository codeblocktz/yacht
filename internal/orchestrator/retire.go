package orchestrator

import (
	"context"
	"fmt"
	"time"
)

// Retiring a node: taking a machine out of service without customers noticing.
//
// Draining evicts everything at once, and an eviction stops a pod before its
// replacement exists — a single-replica app is down until the new one starts,
// and one whose storage lives on the machine never starts anywhere else at all.
// Retiring moves the engine's own workloads the way a deploy does instead: a
// rolling restart onto the rest of the cluster, one app at a time, with the old
// pod serving until the new one is ready.

// NodeRetirer moves work off a machine without a gap in service.
//
// Optional and asserted for, like NodeManager, and for the same reason: it
// reads and restarts workloads in every namespace. Every method is a single
// idempotent step. What drives them — which app next, when to wait — lives with
// the caller, so an implementation stays a set of reads and writes against the
// cluster and a retirement survives the process driving it being restarted.
type NodeRetirer interface {
	// RetirePlan reads what retiring a node involves: which of the engine's
	// apps move by rolling restart, which pods are held by storage on the
	// machine, what else would be evicted, and whether the rest of the cluster
	// has room for all of it. Read-only.
	RetirePlan(ctx context.Context, node string) (RetirePlan, error)

	// MarkRetiring records on the node that it is being retired, and since
	// when. A zero time clears the mark. The mark is the whole of a
	// retirement's stored state: everything else is read back from the
	// cluster, which is what makes a retirement resumable.
	MarkRetiring(ctx context.Context, node string, since time.Time) error

	// RestartApp starts a rolling restart of one of the engine's workloads,
	// the way `kubectl rollout restart` does. The token is written into the
	// pod template, so restarting again with the same token changes nothing
	// and starts no second rollout.
	RestartApp(ctx context.Context, namespace, name, token string) error

	// EvictPod asks one pod to leave, respecting its disruption budget.
	// A pod already gone is not an error.
	EvictPod(ctx context.Context, namespace, name string) error
}

// RetireToken is the marker a retirement writes into each app it restarts.
//
// Derived from the node and the moment retirement began, so every pass of the
// same retirement writes the same value — a resumed retirement recognises the
// apps it already moved — and retiring the node a second time, later, writes a
// different one.
func RetireToken(node string, since time.Time) string {
	return fmt.Sprintf("%s@%d", node, since.Unix())
}

// RetirePlan is what retiring one node involves, as the cluster stands now.
type RetirePlan struct {
	Node string

	// Since is when retirement began. Zero when the node is not being retired.
	Since time.Time

	// Cordoned is whether the node is closed to new work. A retirement never
	// restarts anything onto a node that still accepts it: the replacement
	// could be placed right back where the original was.
	Cordoned bool

	// Apps are the engine's workloads that move by rolling restart: those with
	// a pod on this node, and those this retirement has already restarted, so
	// progress can be counted as "N of M" rather than as what is left.
	Apps []RetireApp

	// Pinned are pods held on this node by storage that lives on it. They are
	// not moved: a restart would stop them, and the replacement could only be
	// scheduled back onto this machine, which no longer accepts work.
	Pinned []PinnedPod

	// Evict are the remaining pods that are not the engine's, and not ones
	// that belong on every node. Nothing is known about how they tolerate a
	// restart, so they are evicted the way a drain always has, and only once
	// the apps have moved.
	Evict []PodRef

	// Finishing are builds running on the node. A build is never retried, so
	// evicting one fails a deploy; with the node closed to new work they are
	// left to finish, which they do within the build timeout.
	Finishing []PodRef

	// Room is whether the rest of the cluster can hold what moves.
	Room RetireRoom
}

// Retiring reports whether retirement has begun.
func (p RetirePlan) Retiring() bool { return !p.Since.IsZero() }

// Token is this retirement's restart marker. Empty when not retiring.
func (p RetirePlan) Token() string {
	if !p.Retiring() {
		return ""
	}
	return RetireToken(p.Node, p.Since)
}

// Moved counts the apps that have left the node.
func (p RetirePlan) Moved() int {
	n := 0
	for _, a := range p.Apps {
		if a.Moved() {
			n++
		}
	}
	return n
}

// Moving is the app whose restart is in flight, if one is.
func (p RetirePlan) Moving() (RetireApp, bool) {
	for _, a := range p.Apps {
		if a.InFlight() {
			return a, true
		}
	}
	return RetireApp{}, false
}

// Next is the app to restart next, if one is waiting.
func (p RetirePlan) Next() (RetireApp, bool) {
	for _, a := range p.Apps {
		if a.PodsHere > 0 && !a.Restarted {
			return a, true
		}
	}
	return RetireApp{}, false
}

// AppsSettled reports whether every app has either moved or stopped trying,
// which is when the rest of the node is evicted.
func (p RetirePlan) AppsSettled() bool {
	for _, a := range p.Apps {
		if !a.Moved() && a.Stuck == "" {
			return false
		}
	}
	return true
}

// Empty reports whether nothing is left on the node but pods that stay: those
// that belong on every node, and those held by storage on this one. That is
// the point at which a retired node can be removed.
func (p RetirePlan) Empty() bool {
	for _, a := range p.Apps {
		if a.PodsHere > 0 {
			return false
		}
	}
	return len(p.Evict) == 0 && len(p.Finishing) == 0
}

// RetireApp is one of the engine's workloads with work on a retiring node.
type RetireApp struct {
	Namespace string
	Name      string
	Owner     OwnerID

	// PodsHere is how many of its pods are still on the node.
	PodsHere int

	// Rolling is true when the workload updates by starting the replacement
	// before stopping the original. False for a workload that recreates —
	// one with storage elsewhere that only one pod can hold at once — which
	// moves with a short gap, because the new pod cannot start until the old
	// one has let go of the volume.
	Rolling bool

	// Restarted is true once it carries this retirement's restart marker.
	Restarted bool

	// RolledOut is true when its most recent rollout has finished: every
	// replica is on the current template and available, and none of the old
	// ones remain.
	RolledOut bool

	// Stuck is why its rollout will not finish on its own, in Kubernetes'
	// words. Empty while it is progressing.
	Stuck string
}

// Moved reports whether this app has left the node.
func (a RetireApp) Moved() bool { return a.Restarted && a.RolledOut && a.PodsHere == 0 }

// InFlight reports whether its restart has started and not finished.
func (a RetireApp) InFlight() bool { return a.Restarted && !a.Moved() && a.Stuck == "" }

// PinnedPod is a pod held on a node by storage that lives on that node.
type PinnedPod struct {
	Namespace string
	Name      string

	// App is the engine workload it belongs to, empty for anything else.
	App string

	// Claim and SizeBytes describe the volume that holds it there. The size is
	// what moving it would mean copying.
	Claim     string
	SizeBytes int64
}

// PodRef names one pod.
type PodRef struct {
	Namespace string
	Name      string
}

// RetireRoom is whether the rest of the cluster can hold what a node is
// running.
//
// Counted in requests — what the scheduler reserves — rather than live usage:
// a pod is placed on what it asks for, and a node that is idle now can still be
// full to the scheduler.
type RetireRoom struct {
	// NeedCPUMillis and NeedMemBytes are what the moving pods request.
	NeedCPUMillis int64
	NeedMemBytes  int64

	// FreeCPUMillis and FreeMemBytes are what the other schedulable, ready
	// nodes have unreserved.
	FreeCPUMillis int64
	FreeMemBytes  int64

	// Unplaced is a pod that fits on no single remaining node even where the
	// totals would. Empty when everything has somewhere to go.
	Unplaced string
}

// Fits reports whether everything that moves has somewhere to go.
func (r RetireRoom) Fits() bool {
	return r.Unplaced == "" &&
		r.NeedCPUMillis <= r.FreeCPUMillis && r.NeedMemBytes <= r.FreeMemBytes
}

// ShortCPUMillis is how much more CPU the rest of the cluster would need.
func (r RetireRoom) ShortCPUMillis() int64 { return max(0, r.NeedCPUMillis-r.FreeCPUMillis) }

// ShortMemBytes is how much more memory the rest of the cluster would need.
func (r RetireRoom) ShortMemBytes() int64 { return max(0, r.NeedMemBytes-r.FreeMemBytes) }
