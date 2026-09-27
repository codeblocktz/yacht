package web

import (
	"cmp"
	"strconv"
	"strings"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// nodeRetireSteps is how far a retirement has got, or what one would do.
//
// Drawn before it starts as well as during: what moves gently, what is evicted,
// and what cannot go anywhere are the three things to know before pressing the
// button, not after.
func nodeRetireSteps(d NodeDetailData) []Step {
	return []Step{
		retireCordonStep(d),
		retireAppsStep(d.Retire),
		retireEvictStep(d.Retire),
		retirePinnedStep(d.Retire),
		retireDoneStep(d.Retire),
	}
}

func retireCordonStep(d NodeDetailData) Step {
	st := Step{Label: "Scheduling stopped"}
	if d.Node.Unschedulable {
		st.State = StepDone
		st.Detail = "Nothing new is placed on this node."
	} else {
		st.State = StepWait
		st.Detail = "Retiring stops scheduling here first, so nothing moves back."
	}
	if d.Retire.Retiring() {
		st.At = d.Retire.Since
	}
	return st
}

func retireAppsStep(p orchestrator.RetirePlan) Step {
	st := Step{Label: "Apps moved"}
	total, moved := len(p.Apps), p.Moved()
	switch {
	case total == 0:
		st.State = cmp.Or(doneIf(p.Retiring()), StepWait)
		st.Detail = "No apps are running here."
		return st
	case !p.Retiring():
		st.State = StepWait
		st.Detail = plural(total, "app") + " move by rolling restart, one at a time. " +
			"Each replacement starts on another node and is ready before the original stops."
		return st
	case moved == total:
		st.State = StepDone
		st.Detail = strconv.Itoa(moved) + " of " + strconv.Itoa(total) + " moved."
		return st
	}

	st.State = StepActive
	st.Detail = strconv.Itoa(moved) + " of " + strconv.Itoa(total) + " moved."
	if a, ok := p.Moving(); ok {
		st.Detail += " Moving " + a.Name + " now — " + movingHow(a)
	}
	if stuck := stuckApps(p); len(stuck) > 0 {
		st.Detail += " " + strings.Join(stuck, " ")
		if p.AppsSettled() {
			st.State = StepErr
		}
	}
	return st
}

// movingHow is what a restart looks like from outside, which differs by how
// the app updates.
func movingHow(a orchestrator.RetireApp) string {
	if a.Rolling {
		return "its replacement is starting elsewhere and takes over once it is ready."
	}
	return "it has storage only one pod can hold at a time, so it restarts " +
		"with a short gap while the volume changes hands."
}

// stuckApps names the apps whose rollout gave up. Their originals keep serving
// here — nothing stops before its replacement is ready — so this is a node that
// will not empty, not an outage.
func stuckApps(p orchestrator.RetirePlan) []string {
	var out []string
	for _, a := range p.Apps {
		if a.Stuck != "" && !a.Moved() {
			out = append(out, a.Name+" will not finish moving: "+a.Stuck+
				" Its original keeps serving here.")
		}
	}
	return out
}

func retireEvictStep(p orchestrator.RetirePlan) Step {
	st := Step{Label: "Everything else asked to leave"}
	evict, builds := len(p.Evict), len(p.Finishing)
	switch {
	case evict == 0 && builds == 0:
		st.State = cmp.Or(doneIf(p.Retiring() && p.AppsSettled()), StepWait)
		st.Detail = "Nothing else here needs to move."
		return st
	case evict == 0:
		st.State = StepWait
		if p.Retiring() && p.AppsSettled() {
			st.State = StepActive
		}
	case !p.Retiring() || !p.AppsSettled():
		st.State = StepWait
		st.Detail = "Once the apps have moved, " + plural(evict, "pod") +
			" Yacht does not manage will be evicted, the way a drain does it. " +
			"They restart elsewhere with whatever gap their own setup allows."
	default:
		st.State = StepActive
		st.Detail = plural(evict, "pod") + " Yacht does not manage " + isAre(evict) +
			" being asked to leave. One that stays may be held by its disruption budget; " +
			"the engine log names it."
	}
	if builds > 0 {
		st.Detail = strings.TrimSpace(st.Detail + " " + plural(builds, "build") +
			" running here " + isAre(builds) + " left to finish rather than failed.")
	}
	return st
}

func retirePinnedStep(p orchestrator.RetirePlan) Step {
	st := Step{Label: "Held by storage"}
	n := len(p.Pinned)
	if n == 0 {
		st.State = cmp.Or(doneIf(p.Retiring()), StepWait)
		st.Detail = "Nothing here keeps its data on this machine."
		return st
	}
	// Drawn as a problem because it is one: a known limit, stated plainly.
	// The alternative — evicting — gives a pod that can never start elsewhere.
	st.State = StepErr
	names := make([]string, 0, n)
	for _, pin := range p.Pinned {
		names = append(names, pinnedLabel(pin))
	}
	st.Detail = strings.Join(names, ", ") + " " + pick(n, "stays", "stay") +
		" until " + pick(n, "its", "their") + " volume is moved. " +
		"Yacht does not copy volumes between machines yet, so " +
		pick(n, "it is", "they are") + " left running here, untouched."
	return st
}

func retireDoneStep(p orchestrator.RetirePlan) Step {
	st := Step{Label: "Ready to remove"}
	if !p.Retiring() || !p.Empty() {
		st.State = StepWait
		st.Detail = "Removal is offered once everything that can move has moved."
		return st
	}
	st.State = StepDone
	st.Detail = "Only pods that stay are left. The node can be removed."
	if n := len(p.Pinned); n > 0 {
		st.Detail += " Removing it strands " + pick(n, "the volume", "the volumes") +
			" above, so move " + pick(n, "its", "their") + " data first."
	}
	return st
}

// pinnedLabel is one pinned pod as "db (volume db-data, 10.0 GiB)": the
// size is what moving it would mean copying.
func pinnedLabel(p orchestrator.PinnedPod) string {
	parts := []string{"volume " + p.Claim}
	if p.SizeBytes > 0 {
		parts = append(parts, formatBytes(p.SizeBytes))
	}
	return cmp.Or(p.App, p.Name) + " (" + strings.Join(parts, ", ") + ")"
}

// podRetireTag is what a retirement does with one pod on the node, for the
// list of what is running there. Empty where there is nothing to add.
func podRetireTag(d NodeDetailData, p orchestrator.PodInfo) (label, class string) {
	for _, pin := range d.Retire.Pinned {
		if pin.Namespace == p.Namespace && pin.Name == p.Name {
			return "stays: volume here", "status-warn"
		}
	}
	if !p.DrainMoves {
		return "stays", "status-neutral"
	}
	if !d.CanRetire {
		return "", ""
	}
	for _, a := range d.Retire.Apps {
		if a.Namespace == p.Namespace && a.Name == p.App {
			return "rolling restart", "status-info"
		}
	}
	for _, b := range d.Retire.Finishing {
		if b.Namespace == p.Namespace && b.Name == p.Name {
			return "build, left to finish", "status-neutral"
		}
	}
	return "evicted", "status-neutral"
}

// roomDetail is the room check in words, for before a retirement starts.
func roomDetail(r orchestrator.RetireRoom) string {
	return "Moving asks for " + formatMillicores(r.NeedCPUMillis) + " CPU and " +
		formatBytes(r.NeedMemBytes) + " of memory; the other nodes have " +
		formatMillicores(r.FreeCPUMillis) + " CPU and " + formatBytes(r.FreeMemBytes) + " free."
}

func doneIf(ok bool) StepState {
	if ok {
		return StepDone
	}
	return ""
}

func isAre(n int) string { return pick(n, "is", "are") }

// pick chooses the singular or plural form of a phrase.
func pick(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
