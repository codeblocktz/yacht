package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// retiringOrchestrator can also move work off a node gently. Its plan is
// whatever the test sets; the orchestration itself is tested in package
// retire, and this is about what the page says and offers.
type retiringOrchestrator struct {
	*managingOrchestrator
	plan     orchestrator.RetirePlan
	restarts []string
}

func newRetiring(plan orchestrator.RetirePlan, pods ...orchestrator.PodInfo) *retiringOrchestrator {
	plan.Node = "agent-0"
	return &retiringOrchestrator{managingOrchestrator: newManaging(pods...), plan: plan}
}

func (m *retiringOrchestrator) RetirePlan(context.Context, string) (orchestrator.RetirePlan, error) {
	p := m.plan
	p.Cordoned = m.cordoned["agent-0"]
	return p, nil
}

func (m *retiringOrchestrator) MarkRetiring(_ context.Context, _ string, since time.Time) error {
	m.plan.Since = since
	return nil
}

func (m *retiringOrchestrator) RestartApp(_ context.Context, _, name, _ string) error {
	m.restarts = append(m.restarts, name)
	return nil
}

func (m *retiringOrchestrator) EvictPod(context.Context, string, string) error { return nil }

func roomFor() orchestrator.RetireRoom {
	return orchestrator.RetireRoom{NeedCPUMillis: 200, FreeCPUMillis: 2000, NeedMemBytes: 256 << 20, FreeMemBytes: 4 << 30}
}

func appOnNode() orchestrator.PodInfo {
	return orchestrator.PodInfo{
		Name: "web-7d9f-x2k", Namespace: "yacht-x", Node: "agent-0", Phase: "Running",
		Ready: 1, Total: 1, App: "web", DrainMoves: true,
	}
}

func pinnedPodInfo() orchestrator.PodInfo {
	return orchestrator.PodInfo{
		Name: "db-5c8b-q9z", Namespace: "yacht-x", Node: "agent-0", Phase: "Running",
		Ready: 1, Total: 1, App: "db", DrainMoves: true,
	}
}

func pinnedDB() orchestrator.PinnedPod {
	return orchestrator.PinnedPod{Namespace: "yacht-x", Name: "db-5c8b-q9z", App: "db", Claim: "db-data", SizeBytes: 10 << 30}
}

// Retiring is offered as the way out, and the drain beside it says it is the
// forceful one.
func TestTheNodePageOffersRetiringAndCallsDrainForceful(t *testing.T) {
	m := newRetiring(orchestrator.RetirePlan{
		Apps: []orchestrator.RetireApp{{Namespace: "yacht-x", Name: "web", PodsHere: 1, Rolling: true}},
		Room: roomFor(),
	}, appOnNode())
	body := do(nodeServer(t, m, account.RoleOwner), signedIn(http.MethodGet, "/cluster/nodes/agent-0")).Body.String()

	for _, want := range []string{"Retire this node", "/cluster/nodes/agent-0/retire", "Force drain", "rolling restart"} {
		if !strings.Contains(body, want) {
			t.Errorf("node page is missing %q", want)
		}
	}
}

// Refused before anything is touched when the rest of the cluster cannot hold
// the node's work, with the shortfall and what to do about it.
func TestRetiringWithoutRoomIsRefusedWithTheShortfall(t *testing.T) {
	m := newRetiring(orchestrator.RetirePlan{
		Apps: []orchestrator.RetireApp{{Namespace: "yacht-x", Name: "web", PodsHere: 1, Rolling: true}},
		Room: orchestrator.RetireRoom{NeedCPUMillis: 2500, FreeCPUMillis: 1000, NeedMemBytes: 1 << 30, FreeMemBytes: 8 << 30},
	}, appOnNode())
	rec := do(nodeServer(t, m, account.RoleOwner), signedIn(http.MethodPost, "/cluster/nodes/agent-0/retire"))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "short of 1.5 CPU") || !strings.Contains(body, "Add a node first") {
		t.Errorf("the refusal does not give the shortfall and the fix")
	}
	if m.cordoned["agent-0"] || m.plan.Retiring() || len(m.restarts) != 0 {
		t.Error("a refused retirement still changed the node")
	}
}

// Starting cordons, marks, and moves the first app by restart — never by the
// drain the old button used.
func TestRetiringCordonsAndRestartsRatherThanDraining(t *testing.T) {
	m := newRetiring(orchestrator.RetirePlan{
		Apps: []orchestrator.RetireApp{{Namespace: "yacht-x", Name: "web", PodsHere: 1, Rolling: true}},
		Room: roomFor(),
	}, appOnNode())
	rec := do(nodeServer(t, m, account.RoleOwner), signedIn(http.MethodPost, "/cluster/nodes/agent-0/retire"))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if !m.cordoned["agent-0"] || !m.plan.Retiring() {
		t.Error("retiring did not cordon and mark the node")
	}
	if len(m.restarts) != 1 || m.restarts[0] != "web" {
		t.Errorf("restarts = %v, want web", m.restarts)
	}
	if len(m.drained) != 0 {
		t.Error("retiring drained the node")
	}
}

// A pod held by its volume is listed as staying, with the size of what would
// have to move. Once nothing else is left, removal is offered — with the
// warning that the volume goes with the machine.
func TestAPinnedVolumeIsListedAndDoesNotBlockRemoval(t *testing.T) {
	m := newRetiring(orchestrator.RetirePlan{
		Since:  time.Now().Add(-10 * time.Minute),
		Pinned: []orchestrator.PinnedPod{pinnedDB()},
		Room:   roomFor(),
	}, pinnedPodInfo(), daemonPod())
	m.cordoned["agent-0"] = true
	m.nodes[0].Unschedulable = true
	h := nodeServer(t, m, account.RoleOwner)

	body := do(h, signedIn(http.MethodGet, "/cluster/nodes/agent-0")).Body.String()
	for _, want := range []string{"stays: volume here", "db (volume db-data, 10.0 GiB)", "until its volume is moved", "Storage on this machine goes with it"} {
		if !strings.Contains(body, want) {
			t.Errorf("node page is missing %q", want)
		}
	}

	if code := do(h, signedIn(http.MethodPost, "/cluster/nodes/agent-0/remove")).Code; code != http.StatusSeeOther {
		t.Fatalf("removing a node retired down to a pinned pod = %d, want 303", code)
	}
}

// Mid-way, the page counts what has moved and names what is moving now.
func TestARetirementShowsHowFarItHasGot(t *testing.T) {
	m := newRetiring(orchestrator.RetirePlan{
		Since: time.Now().Add(-2 * time.Minute),
		Apps: []orchestrator.RetireApp{
			{Namespace: "yacht-x", Name: "api", Restarted: true, RolledOut: true, Rolling: true},
			{Namespace: "yacht-x", Name: "web", PodsHere: 1, Restarted: true, Rolling: true},
			{Namespace: "yacht-x", Name: "worker", PodsHere: 1, Rolling: true},
		},
		Room: roomFor(),
	}, appOnNode())
	m.cordoned["agent-0"] = true
	m.nodes[0].Unschedulable = true

	h := nodeServer(t, m, account.RoleOwner)

	// The polled fragment, which is what a reload mid-way redraws from.
	body := do(h, signedIn(http.MethodGet, "/cluster/nodes/agent-0/status")).Body.String()
	for _, want := range []string{"1 of 3 moved", "Moving web now"} {
		if !strings.Contains(body, want) {
			t.Errorf("progress is missing %q", want)
		}
	}
	if code := do(h, signedIn(http.MethodPost, "/cluster/nodes/agent-0/remove")).Code; code != http.StatusUnprocessableEntity {
		t.Errorf("removing mid-retirement = %d, want 422", code)
	}
}

// Reopening a retiring node is stopping the retirement; otherwise the next
// pass would close it again and the button would seem to do nothing.
func TestAllowingSchedulingAgainStopsARetirement(t *testing.T) {
	m := newRetiring(orchestrator.RetirePlan{Since: time.Now(), Room: roomFor()})
	m.cordoned["agent-0"] = true

	req := signedInForm(http.MethodPost, "/cluster/nodes/agent-0/cordon", "unschedulable=false")
	if code := do(nodeServer(t, m, account.RoleOwner), req).Code; code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", code)
	}
	if m.plan.Retiring() || m.cordoned["agent-0"] {
		t.Error("reopening the node left it retiring or cordoned")
	}
}

// Before it starts, the steps say what would happen and claim nothing has.
func TestARetirementClaimsNothingBeforeItStarts(t *testing.T) {
	d := NodeDetailData{
		Node: orchestrator.NodeInfo{Name: "agent-0"},
		Retire: orchestrator.RetirePlan{
			Apps: []orchestrator.RetireApp{{Name: "web", PodsHere: 1, Rolling: true}},
			Room: roomFor(),
		},
		CanRetire: true,
	}
	for _, s := range nodeRetireSteps(d) {
		if s.State == StepDone || s.State == StepActive {
			t.Errorf("%q is %q on a node nothing has been done to", s.Label, s.State)
		}
	}
}
