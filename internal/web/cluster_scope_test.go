package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// clusterOfTeams is a cluster running one pod and one claim for each of two
// teams and for the platform itself, and answering the owner filter the way
// the API server's label selector does. The Noop has no claims and no
// platform, so it cannot tell a page that lists everything from one that lists
// a team's.
type clusterOfTeams struct {
	*orchestrator.Noop
	pods    []orchestrator.PodInfo
	volumes []orchestrator.VolumeInfo
}

func newClusterOfTeams(mine, theirs orchestrator.OwnerID) clusterOfTeams {
	return clusterOfTeams{
		Noop: orchestrator.NewNoop(),
		pods: []orchestrator.PodInfo{
			{Name: "mine-pod-0", Namespace: "yacht-mine", Phase: "Running", Node: "n1", Owner: mine},
			{Name: "theirs-pod-0", Namespace: "yacht-theirs", Phase: "Running", Node: "n1", Owner: theirs},
			{Name: "garage-0", Namespace: "datahuru-buckets", Phase: "Running", Node: "n1"},
		},
		volumes: []orchestrator.VolumeInfo{
			{Name: "mine-data", Namespace: "yacht-mine", Phase: "Bound", Owner: mine},
			{Name: "theirs-data", Namespace: "yacht-theirs", Phase: "Bound", Owner: theirs},
			{Name: "garage-data", Namespace: "datahuru-buckets", Phase: "Bound"},
		},
	}
}

func (c clusterOfTeams) ClusterSummary(context.Context) (orchestrator.ClusterSummary, error) {
	return orchestrator.ClusterSummary{
		Nodes: 1, NodesReady: 1, Pods: len(c.pods), PodCapacity: 110, Volumes: len(c.volumes),
	}, nil
}

func (c clusterOfTeams) Pods(_ context.Context, opts orchestrator.PodListOptions) ([]orchestrator.PodInfo, error) {
	var out []orchestrator.PodInfo
	for _, p := range c.pods {
		if opts.Owner == "" || p.Owner == opts.Owner {
			out = append(out, p)
		}
	}
	return out, nil
}

func (c clusterOfTeams) Volumes(_ context.Context, owner orchestrator.OwnerID) ([]orchestrator.VolumeInfo, error) {
	var out []orchestrator.VolumeInfo
	for _, v := range c.volumes {
		if owner == "" || v.Owner == owner {
			out = append(out, v)
		}
	}
	return out, nil
}

// The Admin console's pods and volumes are the whole cluster's for a named
// operator: every team's and the platform's own, each saying whose it is.
// They listed only the operator's own team's under a header counting the
// whole cluster — five rows under "Pods 54" — which hid the machine from the
// person running it. Nobody else reaches them.
func TestClusterPagesListEveryTeamForTheOperator(t *testing.T) {
	const theirs = "web-cluster-theirs"
	var orch clusterOfTeams
	rt := newRoleTeamWith(t, "web-cluster-all", "cluster-app", []string{"admin@web.test"},
		func(o *Options) {
			orch = newClusterOfTeams("web-cluster-all", theirs)
			o.Orchestrator = orch
		})
	if err := rt.apps.EnsureOwner(context.Background(), theirs, "Their Co", ""); err != nil {
		t.Fatalf("EnsureOwner: %v", err)
	}

	pods := rt.getAs(t, "/cluster/pods", rt.admin)
	if pods.Code != http.StatusOK {
		t.Fatalf("GET /cluster/pods as the named operator = %d, want 200", pods.Code)
	}
	body := pods.Body.String()
	for _, want := range []string{
		"mine-pod-0", "theirs-pod-0", "garage-0",
		// Whose each is: the other team by name, the install's own as such.
		"Their Co", "Platform", "<th>Team</th>",
		// The rows agree with the header's figure, and say so.
		"3 pods",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the operator's pods page is missing %q", want)
		}
	}
	if !strings.Contains(body, `<div class="metric-v">3</div>`) {
		t.Error("the operator's header does not count the three pods listed")
	}

	vols := rt.getAs(t, "/cluster/volumes", rt.admin).Body.String()
	for _, want := range []string{"mine-data", "theirs-data", "garage-data", "Their Co", "Platform", "3 claims"} {
		if !strings.Contains(vols, want) {
			t.Errorf("the operator's volumes page is missing %q", want)
		}
	}

	// Everybody else is refused outright, rather than shown a list: a team's
	// owner is not the install's operator once operators are named.
	for _, path := range []string{"/cluster/pods", "/cluster/volumes"} {
		for who, c := range map[string]*http.Cookie{"owner": rt.owner, "member": rt.member} {
			rec := rt.getAs(t, path, c)
			if rec.Code != http.StatusForbidden {
				t.Errorf("GET %s as the team's %s = %d, want 403", path, who, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "theirs-") {
				t.Errorf("GET %s as the team's %s showed another team's workload", path, who)
			}
		}
	}
}

// With no operators named, the operator is whichever team's owner is looking,
// and on an install with several teams that is a customer. They are listed
// their own team's pods and claims and nothing else, and the header counts
// those rather than the cluster's.
func TestClusterPagesAreOneTeamsWithoutNamedOperators(t *testing.T) {
	const theirs = "web-cluster-theirs-2"
	rt := newRoleTeamWith(t, "web-cluster-own", "cluster-own-app", nil,
		func(o *Options) { o.Orchestrator = newClusterOfTeams("web-cluster-own", theirs) })
	if err := rt.apps.EnsureOwner(context.Background(), theirs, "Their Co", ""); err != nil {
		t.Fatalf("EnsureOwner: %v", err)
	}

	pods := rt.getAs(t, "/cluster/pods", rt.owner)
	if pods.Code != http.StatusOK {
		t.Fatalf("GET /cluster/pods as the owner = %d, want 200", pods.Code)
	}
	body := pods.Body.String()
	if !strings.Contains(body, "mine-pod-0") {
		t.Error("the owner is not shown their own team's pod")
	}
	for _, leak := range []string{"theirs-pod-0", "garage-0", "Their Co", "<th>Team</th>"} {
		if strings.Contains(body, leak) {
			t.Errorf("a team's owner who is not a named operator was shown %q", leak)
		}
	}
	// One pod and one claim of theirs, not the cluster's three of each.
	if strings.Count(body, `<div class="metric-v">1</div>`) < 3 {
		t.Errorf("the header counts more than the team's own pods and claims:\n%s", metricsOf(body))
	}

	vols := rt.getAs(t, "/cluster/volumes", rt.owner).Body.String()
	if !strings.Contains(vols, "mine-data") {
		t.Error("the owner is not shown their own team's claim")
	}
	for _, leak := range []string{"theirs-data", "garage-data"} {
		if strings.Contains(vols, leak) {
			t.Errorf("a team's owner who is not a named operator was shown the claim %q", leak)
		}
	}

	if code := rt.getAs(t, "/cluster/pods", rt.member).Code; code != http.StatusForbidden {
		t.Errorf("GET /cluster/pods as a member = %d, want 403", code)
	}
}

// metricsOf is the header's figures, for a failure message.
func metricsOf(body string) string {
	start := strings.Index(body, `<div class="metrics`)
	if start < 0 {
		return "(no metrics)"
	}
	end := min(start+1200, len(body))
	return body[start:end]
}
