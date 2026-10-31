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

// A bucket has nothing running, so it is never "Online"; it is up whenever it
// is there, and a healthy project with one is all online, with a green dot.
func TestAProjectWithABucketIsAllOnline(t *testing.T) {
	p := app.Project{ID: uuid.New(), Name: "Media", Slug: "media"}
	apps := []app.App{
		{Name: "web", Service: "web", ProjectID: p.ID, EnvSlug: app.DefaultEnvironment, StatusKnown: true,
			Status: orchestrator.AppStatus{Phase: orchestrator.PhaseRunning}},
		{Name: "bucket", Service: "bucket", ProjectID: p.ID, EnvSlug: app.DefaultEnvironment, Source: app.SourceBucket},
	}
	c := projectCards([]app.Project{p}, apps, "", sortName)[0]
	if got := projectsSummary(c); got != "production · 2/2 services online" {
		t.Errorf("summary = %q", got)
	}
	if got := projectsDot(c); got != "text-success" {
		t.Errorf("dot = %q, want text-success", got)
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
	if remembered.Secure {
		t.Error("the view cookie is Secure over plain HTTP")
	}
	// Behind the proxy the request arrives as HTTP and says it was HTTPS;
	// the cookie is Secure, as the session's is.
	behind := httptest.NewRequest(http.MethodGet, "/?view=list", nil)
	behind.Header.Set("X-Forwarded-Proto", "https")
	for _, c := range do(h, behind).Result().Cookies() {
		if c.Name == viewCookie && !c.Secure {
			t.Error("the view cookie is not Secure behind a TLS-terminating proxy")
		}
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

// summaryMeter is a meter that also totals the month.
type summaryMeter struct {
	stubMeter
	asked *WorkspaceUsageRequest
}

func (m summaryMeter) WorkspaceUsage(_ context.Context, req WorkspaceUsageRequest) (UsageSummary, error) {
	*m.asked = req
	return UsageSummary{
		Period: "1 – 31 Oct", Current: "TZS 2,280", Included: "TZS 12,000", Bill: "TZS 14,160", BillLabel: "Estimated bill",
		Limit:     &UsageLimit{Label: "Spending limit", Used: "TZS 14,160", Of: "TZS 50,000", Percent: 28, Href: "/billing#limit", ActionLabel: "Set limit"},
		Breakdown: []UsageCharge{{Name: "Memory", Quantity: "120 GiB-h", UnitPrice: "TZS 36 / GiB-h", Cost: "TZS 4,320"}},
		Totals:    []UsageTotal{{Label: "Subtotal", Amount: "TZS 4,320"}, {Label: "Bill", Amount: "TZS 14,160", Strong: true}},
		Plan:      &UsagePlan{Name: "Hobby plan", Detail: "Prepaid", Includes: "Includes TZS 12,000 of usage.", Href: "/billing/plans", HrefLabel: "Change plan"},
		Past:      req.Month.Before(monthStart(time.Now())),
	}, nil
}

// A meter that totals the month gets Railway's summary card: the two
// numbers, the limit, the breakdown and the plan, above the projects. An
// earlier month is the summary alone, and the switch goes no further than
// this month.
func TestTheWorkspaceUsageSummary(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	var asked WorkspaceUsageRequest
	h := testServer(t, Options{Apps: apps, Usage: summaryMeter{asked: &asked}})
	body := get(t, h, "/usage").Body.String()
	for _, want := range []string{
		"1 – 31 Oct usage", "TZS 2,280", "Estimated bill", "TZS 14,160", "TZS 14,160 / TZS 50,000",
		`href="/billing#limit"`, "Show breakdown", "TZS 36 / GiB-h", "Change plan", "Usage by project", "TZS 3,000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the usage page is missing %q", want)
		}
	}
	if strings.Contains(body, `aria-label="Next month"`) {
		t.Error("this month offers a next month")
	}
	prev := monthStart(time.Now()).AddDate(0, -1, 0)
	body = get(t, h, "/usage?month="+prev.Format("2006-01")).Body.String()
	if !asked.Month.Equal(prev) {
		t.Errorf("the meter was asked for %v, want %v", asked.Month, prev)
	}
	if strings.Contains(body, "Usage by project") || !strings.Contains(body, `aria-label="Next month"`) {
		t.Error("an earlier month lists projects, or offers no way forward")
	}
	future := monthStart(time.Now()).AddDate(0, 2, 0)
	get(t, h, "/usage?month="+future.Format("2006-01"))
	if !asked.Month.Equal(monthStart(time.Now())) {
		t.Errorf("a month to come was asked for: %v", asked.Month)
	}
}
