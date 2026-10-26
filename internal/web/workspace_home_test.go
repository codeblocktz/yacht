package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A card is production's services and how many are up; another
// environment's copies are not counted twice. Recent activity is the
// default order, by name the other, and the search narrows by name.
func TestProjectCardsDrawProduction(t *testing.T) {
	shop := app.Project{ID: uuid.New(), Name: "Shop", Slug: "shop"}
	blog := app.Project{ID: uuid.New(), Name: "Blog", Slug: "blog"}
	empty := app.Project{ID: uuid.New(), Name: "Attic", Slug: "attic"}
	now := time.Now()
	svc := func(p app.Project, name, env string, up bool, at time.Time) app.App {
		a := app.App{Name: name, Service: name, ProjectID: p.ID, EnvSlug: env, UpdatedAt: at, StatusKnown: true,
			Status: orchestrator.AppStatus{Phase: orchestrator.PhaseFailing}}
		if up {
			a.Status.Phase = orchestrator.PhaseRunning
		}
		return a
	}
	apps := []app.App{
		svc(shop, "web", "", true, now.Add(-time.Hour)),
		svc(shop, "api", "", false, now.Add(-time.Hour)),
		svc(shop, "web-staging", "staging", true, now.Add(-time.Hour)),
		svc(blog, "site", "", true, now),
	}
	for i := 0; i < 9; i++ {
		apps = append(apps, svc(blog, "worker-"+string(rune('a'+i)), "", true, now))
	}

	cards := projectCards([]app.Project{shop, blog, empty}, apps, "", sortRecent)
	if len(cards) != 3 || cards[0].Project.Slug != "blog" || cards[2].Project.Slug != "attic" {
		t.Fatalf("recent order: %v", slugsOf(cards))
	}
	if c := cards[1]; c.Count != 2 || c.Online != 1 || projectsSummary(c) != "production · 1/2 services online" {
		t.Errorf("shop: %d of %d online, %q", c.Online, c.Count, projectsSummary(c))
	}
	if c := cards[0]; len(c.Services) != projectCardServices-1 || c.More != 10-(projectCardServices-1) {
		t.Errorf("blog draws %d and %d more, want %d and the rest", len(c.Services), c.More, projectCardServices-1)
	}
	if projectsSummary(cards[2]) != "No services" {
		t.Errorf("an empty project: %q", projectsSummary(cards[2]))
	}
	if got := slugsOf(projectCards([]app.Project{shop, blog, empty}, apps, "", sortName)); got != "attic blog shop" {
		t.Errorf("by name: %s", got)
	}
	if got := slugsOf(projectCards([]app.Project{shop, blog, empty}, apps, "SHO", sortRecent)); got != "shop" {
		t.Errorf("searching SHO: %s", got)
	}
}

func slugsOf(cards []ProjectCard) string {
	var out []string
	for _, c := range cards {
		out = append(out, c.Project.Slug)
	}
	return strings.Join(out, " ")
}

// The home page is the workspace's projects, and the list view, once
// chosen, is remembered.
func TestTheHomeIsTheProjects(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/").Body.String()
	for _, want := range []string{`class="project-grid"`, `class="project-card"`, "Search projects", `href="/projects/default"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the home is missing %q", want)
		}
	}

	rec := get(t, h, "/?view=list")
	if !strings.Contains(rec.Body.String(), `class="project-list"`) {
		t.Fatal("?view=list does not draw the list")
	}
	var remembered *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == viewCookie {
			remembered = c
		}
	}
	if remembered == nil || remembered.Value != viewList {
		t.Fatal("the list view is not remembered")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(remembered)
	if !strings.Contains(do(h, req).Body.String(), `class="project-list"`) {
		t.Error("the remembered view is not used")
	}
}

// stubMeter prices every project the same.
type stubMeter struct{}

func (stubMeter) ProjectUsage(_ context.Context, req UsageRequest) (ProjectUsage, error) {
	return ProjectUsage{Period: "1–26 Oct", Current: "TZS 1,000", Estimated: "TZS 3,000", Note: "Egress is included."}, nil
}

// The workspace's Usage lists what each project costs, and is offered only
// where something prices it.
func TestTheWorkspaceUsagePage(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps, Usage: stubMeter{}})
	body := get(t, h, "/usage").Body.String()
	for _, want := range []string{"TZS 1,000", "TZS 3,000", "1–26 Oct", "Egress is included.", `href="/usage"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the usage page is missing %q", want)
		}
	}
	unpriced := testServer(t, Options{Apps: newFakeApps()})
	if code := get(t, unpriced, "/usage").Code; code != http.StatusNotFound {
		t.Errorf("an install with no meter answers /usage with %d", code)
	}
	if strings.Contains(get(t, unpriced, "/").Body.String(), `href="/usage"`) {
		t.Error("an install with no meter offers Usage")
	}
}

// Every page of the Admin console leads back to its Overview, not to a
// page that used to stand in for it.
func TestTheConsolesCrumbsLeadToItsOverview(t *testing.T) {
	for _, path := range []string{"/cluster/nodes", "/cluster/dns", "/admin/teams", "/admin/capacity", "/admin/regions", "/settings", "/admin/impersonations"} {
		crumbs := breadcrumbFor(path)
		if len(crumbs) == 0 || crumbs[0].Label != AdminNavHeading || crumbs[0].Href != "/admin" {
			t.Errorf("%s: crumbs %+v, want the console's Overview first", path, crumbs)
		}
	}
}
