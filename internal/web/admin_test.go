package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// fakeQuotas is the Admin area's data without a database: one or more teams,
// with whatever usage a test gives them.
type fakeQuotas struct {
	mu    sync.Mutex
	teams map[string]*app.TeamUsage
	order []string
}

func newFakeQuotas(ids ...string) *fakeQuotas {
	q := &fakeQuotas{teams: map[string]*app.TeamUsage{}}
	for _, id := range ids {
		q.add(app.TeamUsage{TeamID: id, TeamName: "Team " + id})
	}
	return q
}

func (q *fakeQuotas) add(t app.TeamUsage) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.teams[t.TeamID] = &t
	q.order = append(q.order, t.TeamID)
}

func (q *fakeQuotas) TeamUsages(context.Context) ([]app.TeamUsage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]app.TeamUsage, 0, len(q.order))
	for _, id := range q.order {
		out = append(out, *q.teams[id])
	}
	return out, nil
}

func (q *fakeQuotas) TeamUsage(_ context.Context, id string) (app.TeamUsage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.teams[id]
	if !ok {
		return app.TeamUsage{}, app.ErrNoSuchTeam
	}
	return *t, nil
}

func (q *fakeQuotas) SetQuota(_ context.Context, id string, quota app.Quota) error {
	if err := quota.Validate(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.teams[id]
	if !ok {
		return app.ErrNoSuchTeam
	}
	t.Quota = quota
	return nil
}

// The form is in the units a person thinks in and the quota is in the units
// the cluster is given. The conversion is the part that can quietly be off by
// a factor of a thousand, so it is pinned both ways.
func TestAQuotaFormRoundTripsItsUnits(t *testing.T) {
	q, err := QuotaForm{Apps: "12", CPU: "1.5", Memory: "0.5", Storage: "20"}.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := app.Quota{Apps: 12, CPUMillis: 1500, MemoryBytes: 512 << 20, StorageBytes: 20 << 30}
	if q != want {
		t.Fatalf("parsed %+v, want %+v", q, want)
	}
	if back := quotaForm(q); back != (QuotaForm{Apps: "12", CPU: "1.5", Memory: "0.5", Storage: "20"}) {
		t.Fatalf("shown back as %+v", back)
	}
	// Empty is no limit, and no limit is shown as empty rather than as 0,
	// which would read as "none allowed".
	if q, err := (QuotaForm{}).Parse(); err != nil || !q.Unlimited() {
		t.Fatalf("an empty form = %+v, %v; want unlimited", q, err)
	}
	if f := quotaForm(app.Quota{}); f != (QuotaForm{}) {
		t.Fatalf("an unlimited quota is shown as %+v, want empty fields", f)
	}

	for _, bad := range []QuotaForm{
		{CPU: "two"}, {Memory: "-1"}, {Apps: "1.5"}, {Storage: "NaN"}, {CPU: "1e12"},
	} {
		if _, err := bad.Parse(); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

// An install with no account service has one team and one principal, who runs
// it. The Admin area works there too — it is how a single self-hoster caps
// what they deploy on a small box.
func TestTheAdminAreaWorksWithoutAccounts(t *testing.T) {
	quotas := newFakeQuotas("owner-1")
	h := testServer(t, Options{Apps: newFakeApps(), Quotas: quotas})

	rec := get(t, h, "/admin/teams")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/teams = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Team owner-1") || !strings.Contains(body, `href="/admin/teams/owner-1"`) {
		t.Error("the one team is not listed with a way into its quota")
	}
	if !strings.Contains(body, `href="/admin/capacity"`) || !strings.Contains(body, ">Admin<") {
		t.Error("the Admin group is not in the sidebar of its own page")
	}
	if code := get(t, h, "/admin/teams/owner-1").Code; code != http.StatusOK {
		t.Errorf("GET /admin/teams/owner-1 = %d, want 200", code)
	}
	if code := get(t, h, "/admin/teams/no-such-team").Code; code != http.StatusNotFound {
		t.Errorf("GET a team that does not exist = %d, want 404", code)
	}
	if code := get(t, h, "/admin/capacity").Code; code != http.StatusOK {
		t.Errorf("GET /admin/capacity = %d, want 200", code)
	}
}

// Nothing in the Admin group is offered to somebody who does not run the
// install. On an install hosting customers, the cluster under them is not
// theirs to be shown at all.
func TestTheAdminGroupIsOnlyTheOperators(t *testing.T) {
	for _, operator := range []bool{true, false} {
		ctx := context.WithValue(context.Background(), surfacesKey{},
			Surfaces{Operator: operator, Quotas: true, DNS: true, Registry: true})
		slots := DefaultSlots{}.Slots(ctx, httptest.NewRequest(http.MethodGet, "/", nil))

		var admin *NavGroup
		for i, g := range slots.Nav {
			if g.Heading == "Infrastructure" {
				t.Errorf("an Infrastructure group is still drawn (operator=%v)", operator)
			}
			if g.Heading == AdminNavHeading {
				admin = &slots.Nav[i]
			}
			for _, item := range g.Items {
				if !operator && (strings.HasPrefix(item.Href, "/cluster") || strings.HasPrefix(item.Href, "/admin")) {
					t.Errorf("a non-operator is offered %s", item.Href)
				}
			}
		}
		if !operator {
			if admin != nil {
				t.Error("a non-operator is offered the Admin group")
			}
			continue
		}
		if admin == nil {
			t.Fatal("the operator is not offered the Admin group")
		}
		var got []string
		for _, item := range admin.Items {
			got = append(got, item.Href)
		}
		want := []string{
			"/admin/teams", "/admin/capacity", "/cluster/nodes", "/cluster/pods",
			"/cluster/volumes", "/cluster/events", "/cluster/registry", "/cluster/dns",
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("Admin group = %v, want %v", got, want)
		}
	}
}

func TestCapacitySaysWhenToAddANode(t *testing.T) {
	d := CapacityData{}
	d.addNodes([]orchestrator.NodeInfo{
		{Name: "a", Ready: true, CPUCapacityMillis: 4000, CPUAllocatableMillis: 3800,
			MemCapacityBytes: 8 << 30, MemAllocatableBytes: 7 << 30},
		{Name: "b", Ready: true, CPUCapacityMillis: 2000, MemCapacityBytes: 4 << 30},
		// Cordoned and not ready: real machines, and no room for anything new.
		{Name: "c", Ready: true, Unschedulable: true, CPUCapacityMillis: 8000, MemCapacityBytes: 16 << 30},
		{Name: "d", Ready: false, CPUCapacityMillis: 8000, MemCapacityBytes: 16 << 30},
	})
	if d.Nodes != 4 || d.Schedulable != 2 {
		t.Fatalf("nodes = %d, schedulable = %d; want 4 and 2", d.Nodes, d.Schedulable)
	}
	// Allocatable where the node says, capacity where it does not.
	if d.CPUAllocatable != 5800 || d.MemAllocatable != 11<<30 {
		t.Fatalf("allocatable = %dm, %d bytes; want 5800m and 11 GiB", d.CPUAllocatable, d.MemAllocatable)
	}

	d.addTeams([]app.TeamUsage{
		{TeamID: "small", TeamName: "Small", Usage: app.Usage{CPUMillis: 500, MemoryBytes: 1 << 30}},
		{TeamID: "big", TeamName: "Big", Usage: app.Usage{CPUMillis: 4500, MemoryBytes: 2 << 30}},
	})
	if d.Teams[0].TeamID != "big" {
		t.Errorf("teams ordered %s first, want the largest consumer", d.Teams[0].TeamID)
	}
	p := d.Pressures()
	if len(p) != 1 || p[0].Resource != "CPU" || p[0].Percent != 86 {
		t.Fatalf("pressures = %+v, want CPU at 86%% and memory, at 27%%, left alone", p)
	}

	d.CanAddNode = true
	page := renderToString(t, AdminCapacity(d))
	if !strings.Contains(page, "Customers have committed 86% of the cluster's CPU") ||
		!strings.Contains(page, `href="/cluster/nodes/add"`) {
		t.Error("the page does not say to add a node, with a way to do it")
	}
}

// ------------------------------------------------ against a real database

// The Admin area belongs to whoever runs the install: with no operators named,
// that is the team's owner, and a member gets neither the pages nor the links.
func TestTheAdminPagesAreTheOperators(t *testing.T) {
	rt := newRoleTeam(t, "web-role-admin-area", "admin-area-app")

	for _, path := range []string{
		"/admin/teams", "/admin/teams/" + rt.teamID, "/admin/capacity",
	} {
		if code := rt.getAs(t, path, rt.member).Code; code != http.StatusForbidden {
			t.Errorf("GET %s as a member = %d, want 403", path, code)
		}
		if code := rt.getAs(t, path, rt.owner).Code; code != http.StatusOK {
			t.Errorf("GET %s as the operator = %d, want 200", path, code)
		}
	}
	quota := "/admin/teams/" + rt.teamID + "/quota"
	if code := rt.postFormAs(t, quota, rt.admin, url.Values{"apps": {"1"}}).Code; code != http.StatusForbidden {
		t.Errorf("POST %s as a team admin = %d, want 403", quota, code)
	}
	if q, _ := rt.apps.Quota(context.Background(), rt.teamID); !q.Unlimited() {
		t.Fatalf("a refused request set a quota: %+v", q)
	}

	home := rt.getAs(t, "/", rt.member).Body.String()
	if strings.Contains(home, `href="/admin/teams"`) || strings.Contains(home, ">Admin<") {
		t.Error("a member is offered the Admin area")
	}
	home = rt.getAs(t, "/", rt.owner).Body.String()
	if !strings.Contains(home, `href="/admin/teams"`) || !strings.Contains(home, `href="/admin/capacity"`) {
		t.Error("the operator is not offered the Admin area")
	}
}

// Saved, flashed, shown back — and refused with the typed values kept.
func TestTheQuotaFormRoundTrips(t *testing.T) {
	rt := newRoleTeam(t, "web-role-quota-form", "quota-app")
	ctx := context.Background()
	page := "/admin/teams/" + rt.teamID

	post := rt.postFormAs(t, page+"/quota", rt.owner, url.Values{
		"apps": {"3"}, "cpu": {"1.5"}, "memory": {"2"}, "storage": {"10"},
	})
	if post.Code != http.StatusSeeOther || post.Header().Get("Location") != page {
		t.Fatalf("POST quota = %d to %q, want 303 back to %s", post.Code, post.Header().Get("Location"), page)
	}
	got, err := rt.apps.Quota(ctx, rt.teamID)
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if want := (app.Quota{Apps: 3, CPUMillis: 1500, MemoryBytes: 2 << 30, StorageBytes: 10 << 30}); got != want {
		t.Fatalf("stored %+v, want %+v", got, want)
	}

	req := httptest.NewRequest(http.MethodGet, page, nil)
	req.AddCookie(rt.owner)
	for _, c := range post.Result().Cookies() {
		if c.Name == FlashCookie {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "Quota saved") {
		t.Error("saving did not say so")
	}
	for _, v := range []string{`value="3"`, `value="1.5"`, `value="2"`, `value="10"`} {
		if !strings.Contains(body, v) {
			t.Errorf("the saved quota is not shown back: missing %s", v)
		}
	}

	bad := rt.postFormAs(t, page+"/quota", rt.owner, url.Values{
		"apps": {"3"}, "cpu": {"lots"}, "memory": {"2"}, "storage": {"10"},
	})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST an unreadable CPU = %d, want 422", bad.Code)
	}
	if b := bad.Body.String(); !strings.Contains(b, "CPU must be a number") || !strings.Contains(b, `value="lots"`) {
		t.Error("the refusal does not say which field, or lost what was typed")
	}
	if again, _ := rt.apps.Quota(ctx, rt.teamID); again != got {
		t.Errorf("a refused form changed the quota to %+v", again)
	}
}
