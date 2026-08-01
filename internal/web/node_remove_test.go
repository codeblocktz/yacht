package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// managingOrchestrator is a Noop that can also take a node out of service.
type managingOrchestrator struct {
	*orchestrator.Noop
	nodes    []orchestrator.NodeInfo
	pods     []orchestrator.PodInfo
	cordoned map[string]bool
	drained  []string
	deleted  []string
}

func newManaging(pods ...orchestrator.PodInfo) *managingOrchestrator {
	return &managingOrchestrator{
		Noop:     orchestrator.NewNoop(),
		nodes:    []orchestrator.NodeInfo{{Name: "agent-0", Ready: true, Version: "v1.35.5"}},
		pods:     pods,
		cordoned: map[string]bool{},
	}
}

func (m *managingOrchestrator) Nodes(context.Context) ([]orchestrator.NodeInfo, error) {
	return m.nodes, nil
}

func (m *managingOrchestrator) Pods(
	_ context.Context, opts orchestrator.PodListOptions,
) ([]orchestrator.PodInfo, error) {
	if opts.Node == "" {
		return m.pods, nil
	}
	var out []orchestrator.PodInfo
	for _, p := range m.pods {
		if p.Node == opts.Node {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *managingOrchestrator) Cordon(_ context.Context, node string, on bool) error {
	m.cordoned[node] = on
	return nil
}

func (m *managingOrchestrator) Drain(_ context.Context, node string) (int, error) {
	m.drained = append(m.drained, node)
	return len(m.pods), nil
}

func (m *managingOrchestrator) DeleteNode(_ context.Context, node string) error {
	m.deleted = append(m.deleted, node)
	return nil
}

func nodeServer(t *testing.T, orch orchestrator.Orchestrator, role account.Role) http.Handler {
	t.Helper()
	const team = "node-team"
	s, err := New(Options{
		Orchestrator:    orch,
		Apps:            newFakeApps(sampleApp(team, "probe")),
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: role},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

func busyPod() orchestrator.PodInfo {
	return orchestrator.PodInfo{
		Name: "api-1", Namespace: "yacht-x", Node: "agent-0", Phase: "Running",
		Ready: 1, Total: 1, DrainMoves: true,
	}
}

func daemonPod() orchestrator.PodInfo {
	return orchestrator.PodInfo{
		Name: "svclb-1", Namespace: "kube-system", Node: "agent-0", Phase: "Running",
		Ready: 1, Total: 1, DrainMoves: false,
	}
}

// Removing a node evicts every team's workloads off it, so it is owner-only for
// the reason adding one is.
func TestOnlyAnOwnerCanTakeANodeOutOfService(t *testing.T) {
	for _, tc := range []struct {
		role account.Role
		want int
	}{
		{account.RoleOwner, http.StatusOK},
		{account.RoleAdmin, http.StatusForbidden},
		{account.RoleMember, http.StatusForbidden},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			h := nodeServer(t, newManaging(), tc.role)
			if code := do(h, signedIn(http.MethodGet, "/cluster/nodes/agent-0")).Code; code != tc.want {
				t.Errorf("GET node as %s = %d, want %d", tc.role, code, tc.want)
			}
			for _, path := range []string{"/drain", "/remove", "/cordon"} {
				code := do(h, signedIn(http.MethodPost, "/cluster/nodes/agent-0"+path)).Code
				if tc.want == http.StatusForbidden && code != http.StatusForbidden {
					t.Errorf("POST %s as %s = %d, want 403", path, tc.role, code)
				}
			}
		})
	}
}

// The check that matters. The button is hidden on a busy node, but a form can
// be submitted from a page that went stale while somebody read it — and a
// hidden button is a rendering decision, not a guard.
func TestANodeStillRunningWorkIsNotRemoved(t *testing.T) {
	m := newManaging(busyPod())
	h := nodeServer(t, m, account.RoleOwner)

	rec := do(h, signedIn(http.MethodPost, "/cluster/nodes/agent-0/remove"))

	if len(m.deleted) != 0 {
		t.Fatalf("a node still running %d pod(s) was removed anyway", len(m.pods))
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "drain it before removing it") {
		t.Error("the refusal does not say what to do about it")
	}
}

// A node holding only pods a drain would not move is empty for this purpose.
// Waiting for a DaemonSet pod to leave means waiting forever.
func TestPodsADrainWouldNotMoveDoNotBlockRemoval(t *testing.T) {
	m := newManaging(daemonPod())
	h := nodeServer(t, m, account.RoleOwner)

	if code := do(h, signedIn(http.MethodPost, "/cluster/nodes/agent-0/remove")).Code; code != http.StatusSeeOther {
		t.Fatalf("removing a node holding only unmovable pods = %d, want 303", code)
	}
	if len(m.deleted) != 1 || m.deleted[0] != "agent-0" {
		t.Fatalf("deleted = %v, want [agent-0]", m.deleted)
	}
}

// Draining cordons first. Otherwise the scheduler can put a pod on the machine
// being emptied while it is being emptied.
func TestDrainingCordonsFirst(t *testing.T) {
	m := newManaging(busyPod())
	h := nodeServer(t, m, account.RoleOwner)

	if code := do(h, signedIn(http.MethodPost, "/cluster/nodes/agent-0/drain")).Code; code != http.StatusSeeOther {
		t.Fatalf("drain = %d, want 303", code)
	}
	if !m.cordoned["agent-0"] {
		t.Error("the node was drained without being cordoned, so the scheduler could refill it")
	}
	if len(m.drained) != 1 {
		t.Fatalf("drained = %v, want one call", m.drained)
	}
}

// An orchestrator that cannot manage nodes offers no node routes at all,
// rather than pages that fail when pressed.
func TestWithoutTheAbilityTheRoutesAreAbsent(t *testing.T) {
	h := nodeServer(t, orchestrator.NewNoop(), account.RoleOwner)

	if code := do(h, signedIn(http.MethodGet, "/cluster/nodes/agent-0")).Code; code != http.StatusNotFound {
		t.Errorf("node page without a node manager = %d, want 404", code)
	}
	if code := do(h, signedIn(http.MethodPost, "/cluster/nodes/agent-0/remove")).Code; code != http.StatusNotFound {
		t.Errorf("remove without a node manager = %d, want 404", code)
	}
}

// The page says which pods will not move, because otherwise somebody waits for
// a count that is never going to reach zero.
func TestThePageSaysWhichPodsStay(t *testing.T) {
	h := nodeServer(t, newManaging(busyPod(), daemonPod()), account.RoleOwner)

	body := do(h, signedIn(http.MethodGet, "/cluster/nodes/agent-0")).Body.String()

	if !strings.Contains(body, "svclb-1") || !strings.Contains(body, "stays") {
		t.Error("the page does not mark the pods a drain will leave behind")
	}
	if strings.Contains(body, "Remove from cluster</button>") &&
		!strings.Contains(body, "disabled") {
		t.Error("removal is offered on a node that still has work to move")
	}
}
