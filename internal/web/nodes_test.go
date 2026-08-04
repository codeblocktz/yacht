package web

import (
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A machine thirty seconds into joining and one that registered an hour ago and
// never came up are the same boolean and entirely different situations. Telling
// them apart is the whole point of the progression.
func TestAJoiningNodeIsNotDrawnLikeAStuckOne(t *testing.T) {
	fresh := orchestrator.NodeInfo{
		Name: "yacht-w1", CreatedAt: time.Now().Add(-20 * time.Second),
		Reason:  "KubeletNotReady",
		Message: "container runtime network not ready: cni plugin not initialized",
	}
	stuck := orchestrator.NodeInfo{
		Name: "yacht-w2", CreatedAt: time.Now().Add(-2 * time.Hour),
		Reason: "KubeletNotReady",
	}

	freshStep := nodeJoinSteps(fresh, 0)[1]
	if freshStep.State != StepActive {
		t.Errorf("a node twenty seconds in = %q, want it drawn as progress", freshStep.State)
	}
	// The cluster's own words, not a sentence written here in advance.
	if !strings.Contains(freshStep.Detail, "cni plugin not initialized") {
		t.Errorf("detail = %q, want what the kubelet actually said", freshStep.Detail)
	}

	stuckStep := nodeJoinSteps(stuck, 0)[1]
	if stuckStep.State != StepErr {
		t.Errorf("a node two hours in = %q, want it to stop reading as progress", stuckStep.State)
	}

	// And the word beside each differs too.
	if label, _ := nodeJoinStatus(fresh); label != "starting" {
		t.Errorf("fresh label = %q, want starting", label)
	}
	if label, _ := nodeJoinStatus(stuck); label != "not ready" {
		t.Errorf("stuck label = %q, want not ready", label)
	}
}

// Ready and refusing work is deliberate, not stalled.
func TestACordonedNodeIsNotDrawnAsAProblem(t *testing.T) {
	cordoned := orchestrator.NodeInfo{
		Name: "yacht-w1", Ready: true, Unschedulable: true,
		CreatedAt: time.Now().Add(-time.Hour),
	}

	label, _ := nodeJoinStatus(cordoned)
	if label != "cordoned" {
		t.Errorf("label = %q, want cordoned", label)
	}
	if got := nodeJoinSteps(cordoned, 0)[2]; got.State == StepErr {
		t.Error("a deliberately cordoned node is drawn as a failure")
	}
}

// "3 of 7 pods have left" is the question somebody draining is asking. The list
// alone made them count.
func TestADrainCountsDownWhatIsLeft(t *testing.T) {
	d := NodeDetailData{
		Node: orchestrator.NodeInfo{Name: "yacht-w1", Unschedulable: true},
		Pods: []orchestrator.PodInfo{
			{Name: "a", DrainMoves: true},
			{Name: "b", DrainMoves: true},
			{Name: "c", DrainMoves: false},
		},
	}

	steps := nodeDrainSteps(d)
	if steps[0].State != StepDone {
		t.Error("a cordoned node does not show scheduling as stopped")
	}
	if steps[1].State != StepActive {
		t.Errorf("eviction step = %q, want it in progress", steps[1].State)
	}
	if !strings.Contains(steps[1].Detail, "2 pods") {
		t.Errorf("detail = %q, want the count still to leave", steps[1].Detail)
	}
	// The page has always promised a refusal "is reported here". Nothing did.
	if !strings.Contains(steps[1].Detail, "1 cannot move") {
		t.Errorf("detail = %q, want the blocked pod named as blocked", steps[1].Detail)
	}
}

// A node holding only pods that cannot move will not empty on its own, and
// saying "waiting" forever is the wrong answer.
func TestADrainThatCannotFinishSaysSo(t *testing.T) {
	d := NodeDetailData{
		Node: orchestrator.NodeInfo{Name: "yacht-w1", Unschedulable: true},
		Pods: []orchestrator.PodInfo{{Name: "db", DrainMoves: false}},
	}

	last := nodeDrainSteps(d)[2]
	if last.State != StepErr {
		t.Errorf("state = %q, want it to say the drain will not finish", last.State)
	}
	if !strings.Contains(last.Detail, "cannot be moved") {
		t.Errorf("detail = %q", last.Detail)
	}
}

// Nothing is claimed about eviction before the node has even been cordoned.
func TestADrainClaimsNothingBeforeItStarts(t *testing.T) {
	d := NodeDetailData{Node: orchestrator.NodeInfo{Name: "yacht-w1"}}

	steps := nodeDrainSteps(d)
	for _, s := range steps {
		if s.State == StepDone || s.State == StepActive {
			t.Errorf("%q is %q on a node nothing has been done to", s.Label, s.State)
		}
	}
}

// A drained node says so, which is what makes removal reachable.
func TestADrainedNodeReportsItIsEmpty(t *testing.T) {
	d := NodeDetailData{
		Node:    orchestrator.NodeInfo{Name: "yacht-w1", Unschedulable: true},
		Drained: true,
	}

	if got := nodeDrainSteps(d)[2]; got.State != StepDone {
		t.Errorf("state = %q, want done", got.State)
	}
}
