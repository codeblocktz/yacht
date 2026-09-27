package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The chrome's own gallery states: the sidebar with a team's projects in it,
// the collapsed rail, each menu open, the command palette, the checklist, and
// the phone drawer. Every other gallery page is drawn inside the same chrome,
// so this is also what they are reviewed wearing.

// galleryOwner is who the gallery is signed in as.
var galleryOwner = identity.Owner{
	ID: "team-acme", DisplayName: "Ada Lovelace", Email: "ada@acme.dev",
}

// galleryMemberships are the teams the gallery's person belongs to.
var galleryMemberships = []TeamChoice{
	{ID: "team-acme", Name: "Acme Inc", Role: account.RoleOwner, Active: true},
	{ID: "team-lab", Name: "Research Lab", Role: account.RoleAdmin},
	{ID: "team-oss", Name: "Open Source", Role: account.RoleMember},
}

// galleryWorkspace is a team a few weeks in: several projects, apps in every
// state the sidebar draws, and two deploys in flight.
func galleryWorkspace() app.Workspace {
	p := func(slug, name string, apps ...app.WorkspaceApp) app.WorkspaceProject {
		return app.WorkspaceProject{
			ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(slug)), Slug: slug, Name: name, Apps: apps,
		}
	}
	a := func(name string, state app.AppState) app.WorkspaceApp {
		return app.WorkspaceApp{Name: name, State: state}
	}
	return app.Workspace{
		Projects: []app.WorkspaceProject{
			p("analytics", "Analytics",
				a("clickhouse", app.AppServing), a("ingest", app.AppStopped)),
			p("default", "Default"),
			p("internal-tools", "Internal tools", a("admin", app.AppIdle)),
			p("marketing-site", "Marketing site", a("site", app.AppServing)),
			p("storefront", "Storefront",
				a("api", app.AppDeployFailed), a("web", app.AppServing),
				a("worker", app.AppDeploying), a("mailer", app.AppFailed)),
			p("payments", "Payments", a("ledger", app.AppDeploying)),
			p("staging", "Staging", a("web-preview", app.AppServing)),
		},
		LiveDeploys: 2,
		Onboarding:  app.Onboarding{HasApps: true, HasSucceededDeploy: true},
	}
}

// galleryChromeContext puts a person, their teams and their team's workspace
// on the context, the way the server's middleware does.
func galleryChromeContext(ctx context.Context, g galleryPage) context.Context {
	if g.bare {
		return ctx
	}
	ctx = identity.NewContext(ctx, galleryOwner)
	if !g.solo {
		ctx = context.WithValue(ctx, teamsKey{}, teamLister(func() []TeamChoice {
			return galleryMemberships
		}))
	}
	ws := galleryWorkspace()
	return context.WithValue(ctx, workspaceKey{}, workspaceLoader(func() (app.Workspace, bool) {
		return ws, true
	}))
}

// writeGallerySearchIndex writes what /search/index would answer, at that
// path, so the palette in the served gallery has projects and apps to find.
func writeGallerySearchIndex(t *testing.T, out string) {
	t.Helper()
	dir := filepath.Join(out, "search")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, err := json.Marshal(searchIndex{Items: searchEntries(galleryWorkspace()), Domains: true})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index"), b, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

func chromeGalleryPages() []galleryPage {
	now := time.Now()
	mk := func(name string, phase orchestrator.Phase, replicas, ready int32) app.App {
		return app.App{
			ID: uuid.New(), OwnerID: galleryOwner.ID, Name: name,
			Image: "ghcr.io/acme/" + name + ":v1", Replicas: replicas, Port: 8080,
			CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-time.Hour),
			StatusKnown: true,
			Status:      orchestrator.AppStatus{Phase: phase, Desired: replicas, Ready: ready},
		}
	}
	apps := []app.App{
		mk("api", orchestrator.PhaseRunning, 2, 2),
		mk("web", orchestrator.PhaseRunning, 3, 3),
		mk("worker", orchestrator.PhasePending, 1, 0),
		mk("mailer", orchestrator.PhaseDegraded, 2, 1),
	}
	apps[1].Host, apps[1].TLS = "shop.acme.dev", true

	overview := func(c *Checklist) OverviewData {
		return OverviewData{
			OwnerName: galleryOwner.DisplayName, ClusterOK: true, AppCount: int64(len(apps)),
			Apps: apps, Activity: activityBusy(), Checklist: c,
		}
	}
	partly := gettingStarted(checklistFacts{
		Onboarding: galleryWorkspace().Onboarding,
		Members:    1, Accounts: true, Domains: true, FirstApp: "api",
	})

	storefront := []Crumb{{Label: "Projects", Href: "/projects"}, {Label: "Storefront", Href: "/projects/storefront"}, {Label: "api"}}
	openMenu := func(n int) string {
		return `var m = document.querySelectorAll("details[data-menu]")[` + string(rune('0'+n)) + `];` +
			` m.open = true; var s = m.querySelector("summary"); s.blur();`
	}

	return []galleryPage{
		{
			// The project the viewer is in, open, with the app they are on
			// highlighted; another project left open by hand; a badge on
			// Deployments for the two deploys in flight.
			file: "chrome-sidebar.html", path: "/apps/api", crumbs: storefront,
			page: AppList(apps),
			boot: `var n = document.querySelector('[data-tree-node="` +
				uuid.NewSHA1(uuid.NameSpaceURL, []byte("analytics")).String() + `"]');` +
				` n.querySelector("[data-tree-toggle]").click();`,
		},
		{
			file: "chrome-sidebar-light.html", path: "/apps/api", crumbs: storefront,
			page: AppList(apps),
			// Chosen the way a person chooses it — stored, then applied before
			// paint on the next load — so nothing is caught mid-transition.
			boot: `if (!sessionStorage.getItem("gallery")) { sessionStorage.setItem("gallery", "1");` +
				` window.yachtSetTheme("light"); location.reload(); }`,
		},
		{
			// Collapsed to icons, with a tooltip showing for the focused link.
			file: "chrome-rail.html", path: "/", crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(overview(nil)),
			boot: `document.documentElement.setAttribute("data-sidebar", "collapsed");` +
				` document.querySelector('.nav-item[href="/deployments"]').focus();`,
		},
		{
			file: "chrome-rail-menu.html", path: "/", crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(overview(nil)),
			boot: `document.documentElement.setAttribute("data-sidebar", "collapsed"); ` + openMenu(0),
		},
		{
			file: "chrome-palette.html", path: "/", crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(overview(nil)),
			boot: `window.yachtPalette.open();`,
		},
		{
			file: "chrome-palette-search.html", path: "/", crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(overview(nil)),
			boot: `window.yachtPalette.open(); setTimeout(function () {` +
				` var i = document.querySelector("[data-palette-input]"); i.value = "st";` +
				` i.dispatchEvent(new Event("input")); }, 200);`,
		},
		{
			file: "chrome-switcher.html", path: "/", crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(overview(nil)),
			boot: openMenu(0),
		},
		{
			file: "chrome-user-menu.html", path: "/", crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(overview(nil)),
			boot: openMenu(1),
		},
		{
			// An install with no accounts: the owner, named, and nothing to
			// switch to.
			file: "chrome-solo.html", path: "/", crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(overview(nil)), solo: true,
		},
		{
			file: "chrome-getting-started.html", path: "/", crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(overview(partly)),
		},
		{
			// Shot at phone width: the same sidebar, as a drawer over the page.
			file: "chrome-drawer.html", path: "/apps/api", crumbs: storefront,
			page: AppList(apps),
			boot: `window.yachtSidebarToggle();`,
		},
	}
}

// The gallery's chrome has to be a real one: signed in, in a team, with
// projects. A gallery drawn against an empty context reviews a sidebar with
// no switcher, no tree and no person in it — which no install ships.
func TestTheGalleryDrawsAFullChrome(t *testing.T) {
	g := galleryPage{path: "/apps/api"}
	ctx := galleryChromeContext(context.Background(), g)
	s := DefaultSlots{}.Slots(ctx, httptest.NewRequest("GET", "/apps/api", nil))
	if s.SidebarTop == nil || s.SidebarFooter == nil || s.Projects == nil {
		t.Fatalf("gallery chrome is missing parts: top %v, footer %v, projects %v",
			s.SidebarTop != nil, s.SidebarFooter != nil, s.Projects != nil)
	}
	if len(chromeGalleryPages()) < 8 {
		t.Error("the chrome's gallery states are missing")
	}
}
