package web

import (
	"strconv"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Turning a node's condition into a progression.
//
// Joining a cluster and draining one are both things a person starts and then
// watches, and both were drawn as a flat list that could not express progress.
// A machine that has registered and is starting its container runtime rendered
// identically to one that registered an hour ago and never came up, which is
// the exact pair somebody watching a join needs to tell apart.

// stillJoining is how long a not-Ready node is given before its state stops
// reading as progress and starts reading as a problem.
//
// K3s on a small VPS is usually Ready inside a minute; the CNI is the slow part.
// Well past that and "starting" is no longer the honest word — nothing says the
// machine is broken, but nobody should be told to keep waiting either.
const stillJoining = 5 * time.Minute

// nodeJoinSteps is how far one machine has got toward taking work.
func nodeJoinSteps(n orchestrator.NodeInfo, pods int) []Step {
	registered := Step{
		Label:  "Registered",
		Detail: "The machine has joined the cluster and appears here.",
		At:     n.CreatedAt,
		State:  StepDone,
	}

	starting := Step{Label: "Started"}
	switch {
	case n.Ready:
		starting.State = StepDone
		starting.Detail = "The kubelet is reporting healthy."
	case time.Since(n.CreatedAt) > stillJoining:
		// Past the point where waiting is the answer. Not called a failure —
		// nothing here knows the machine is broken — but it stops being drawn
		// as progress, because it is not making any.
		starting.State = StepErr
		starting.Detail = nodeReasonDetail(n,
			"It has not become ready. Check the agent's logs on the machine itself.")
	default:
		starting.State = StepActive
		starting.Detail = nodeReasonDetail(n,
			"Waiting for the kubelet to report healthy. This usually takes under a minute.")
	}

	taking := Step{Label: "Taking work"}
	switch {
	case !n.Ready:
		taking.State = StepWait
		taking.Detail = "Nothing is scheduled here until the node is ready."
	case n.Unschedulable:
		// Ready and refusing work is a deliberate state, not a stalled one.
		taking.State = StepWait
		taking.Detail = "Scheduling is stopped on this node, so nothing new lands here."
	case pods == 0:
		taking.State = StepActive
		taking.Detail = "Ready and empty. The scheduler places work here as it needs to."
	default:
		taking.State = StepDone
		taking.Detail = plural(pods, "pod") + " running here."
	}

	return []Step{registered, starting, taking}
}

// nodeReasonDetail prefers what the cluster said over what we would guess.
//
// The kubelet fills these in with something usable — "container runtime network
// not ready: cni plugin not initialized" is the ordinary state of a machine
// thirty seconds into joining — and it is more use than any sentence written
// here in advance.
func nodeReasonDetail(n orchestrator.NodeInfo, fallback string) string {
	switch {
	case n.Message != "":
		return n.Message
	case n.Reason != "":
		return n.Reason
	}
	return fallback
}

// nodeDrainSteps is how far a drain has got.
//
// Counting down rather than listing what is left. "3 of 7 pods have left" is
// the shape of the question somebody draining a node is actually asking, and
// the list alone made them count.
func nodeDrainSteps(d NodeDetailData) []Step {
	movable, blocked := 0, 0
	for _, p := range d.Pods {
		if p.DrainMoves {
			movable++
		} else {
			blocked++
		}
	}

	cordoned := Step{Label: "Scheduling stopped"}
	if d.Node.Unschedulable {
		cordoned.State = StepDone
		cordoned.Detail = "Nothing new is placed on this node."
	} else {
		cordoned.State = StepWait
		cordoned.Detail = "This node still accepts new work. Draining stops that first."
	}

	evicting := Step{Label: "Pods moved off"}
	switch {
	case !d.Node.Unschedulable:
		evicting.State = StepWait
		evicting.Detail = "Nothing has been asked to leave yet."
	case movable == 0:
		evicting.State = StepDone
		evicting.Detail = "Everything that can move has moved."
	default:
		evicting.State = StepActive
		evicting.Detail = plural(movable, "pod") +
			" still to leave. Each restarts elsewhere before this finishes."
	}

	// Named rather than merely counted. The page has always promised that a
	// refused eviction "is reported here" and nothing reported it.
	if blocked > 0 {
		evicting.Detail += " " + strconv.Itoa(blocked) +
			" cannot move — see the list below for which, and why."
	}

	drained := Step{Label: "Empty"}
	switch {
	case d.Drained:
		drained.State = StepDone
		drained.Detail = "Nothing a drain would move is left. The node can be removed."
	case blocked > 0 && movable == 0:
		// Stuck, and stuck for a reason that will not resolve on its own: a pod
		// with storage on this machine has nowhere to go.
		drained.State = StepErr
		drained.Detail = "What remains cannot be moved, so this node will not empty on its own."
	default:
		drained.State = StepWait
		drained.Detail = "Removal is offered once nothing is left."
	}

	return []Step{cordoned, evicting, drained}
}

// nodeJoinStatus is the word beside a node in the join list.
func nodeJoinStatus(n orchestrator.NodeInfo) (label, class string) {
	switch {
	case n.Ready && n.Unschedulable:
		return "cordoned", "status-warn"
	case n.Ready:
		return "ready", "status-ok"
	case time.Since(n.CreatedAt) > stillJoining:
		return "not ready", "status-err"
	}
	return "starting", "status-info status-live"
}
