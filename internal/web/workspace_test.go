package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// workspaceCtx is a request context carrying an owner and a workspace, the
// way withWorkspace leaves one.
func workspaceCtx(ws app.Workspace) context.Context {
	ctx := identity.NewContext(context.Background(), identity.Owner{ID: "owner-1", DisplayName: "Eric"})
	return context.WithValue(ctx, workspaceKey{}, workspaceLoader(func() (app.Workspace, bool) {
		return ws, true
	}))
}

func project(slug string, apps ...app.WorkspaceApp) app.WorkspaceProject {
	return app.WorkspaceProject{
		ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(slug)), Slug: slug, Name: strings.ToUpper(slug[:1]) + slug[1:],
		Apps: apps,
	}
}

// ------------------------------------------------------------ search index

// The palette's index is the requesting team's, and nobody else's.
//
// Scoped by the owner identity resolved, never by the request: there is no
// parameter here to name another team with, so this checks that what comes
// back is exactly the resolved owner's projects and apps.
func TestTheSearchIndexIsTheTeamsOwn(t *testing.T) {
	apps := newFakeApps()
	apps.workspace = map[string]app.Workspace{
		"owner-1": {Projects: []app.WorkspaceProject{
			project("shop", app.WorkspaceApp{Name: "web", State: app.AppServing}),
		}},
		"owner-2": {Projects: []app.WorkspaceProject{
			project("secret", app.WorkspaceApp{Name: "vault", State: app.AppServing}),
		}},
	}
	h := testServer(t, Options{Apps: apps})

	rec := get(t, h, "/search/index?owner=owner-2")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /search/index = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	// Not cached anywhere past this response: it lists somebody's apps.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var got searchIndex
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	want := []searchEntry{
		{Name: "Shop", Kind: "project", Href: "/projects/shop", Detail: "1 app"},
		{Name: "web", Kind: "app", Href: "/apps/web", Status: "serving", Detail: "Shop"},
	}
	if fmt.Sprint(got.Items) != fmt.Sprint(want) {
		t.Errorf("items = %+v\nwant %+v", got.Items, want)
	}
	if strings.Contains(rec.Body.String(), "vault") || strings.Contains(rec.Body.String(), "secret") {
		t.Error("another team's apps leaked into the index")
	}
	// No networking surface, so nowhere to send "Add a custom domain".
	if got.Domains {
		t.Error("the index offers domains on an install that has no domains tab")
	}
}

// An install with no app service still answers, with nothing in it: the
// palette asks on every page and a 500 would be noise in the console.
func TestTheSearchIndexWithNothingBehindIt(t *testing.T) {
	h := testServer(t, Options{})
	rec := get(t, h, "/search/index")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[],"domains":false}` {
		t.Errorf("GET /search/index = %d %s", rec.Code, rec.Body.String())
	}

	failing := newFakeApps()
	failing.workspaceErr = errors.New("database gone")
	h = testServer(t, Options{Apps: failing})
	if rec := get(t, h, "/search/index"); rec.Code != http.StatusOK {
		t.Errorf("a failed read turned the index into %d", rec.Code)
	}
}

// Behind the member gate like every other read: a session with no role in the
// team it is acting as is refused, and a member is let through.
func TestTheSearchIndexIsBehindTheMemberGate(t *testing.T) {
	const team = "web-search-gate"
	ask := func(h http.Handler) int {
		req := httptest.NewRequest(http.MethodGet, "/search/index", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "probe-session"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	roleless := gatedServer(t, &rolelessAccounts{fakeAccounts: &fakeAccounts{}, team: team}, team)
	if code := ask(roleless); code != http.StatusForbidden {
		t.Errorf("a session with no role read the index: %d", code)
	}
	member := gatedServer(t, &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: account.RoleMember}, team)
	if code := ask(member); code != http.StatusOK {
		t.Errorf("a member could not read the index: %d", code)
	}
}

// ------------------------------------------------------- deployments badge

// Deploys in flight are counted on the Deployments link, and nothing is drawn
// when there are none — a "0" badge is a badge that is always there.
func TestTheDeploymentsLinkCountsDeploysInFlight(t *testing.T) {
	deployments := func(ws app.Workspace) NavItem {
		s := DefaultSlots{}.Slots(workspaceCtx(ws), httptest.NewRequest("GET", "/", nil))
		for _, g := range s.Nav {
			for _, item := range g.Items {
				if item.Href == "/deployments" {
					return item
				}
			}
		}
		t.Fatal("no Deployments link")
		return NavItem{}
	}

	if got := deployments(app.Workspace{LiveDeploys: 3}); got.Badge != "3" || !got.Live {
		t.Errorf("three in flight: badge %q live %v", got.Badge, got.Live)
	}
	if got := deployments(app.Workspace{}); got.Badge != "" || got.Live {
		t.Errorf("none in flight: badge %q live %v", got.Badge, got.Live)
	}

	// And drawn: the count, in the in-flight style, and in the link's name,
	// since the rail hides the number.
	apps := newFakeApps()
	apps.workspace = map[string]app.Workspace{"owner-1": {LiveDeploys: 2}}
	body := get(t, testServer(t, Options{Apps: apps}), "/").Body.String()
	for _, want := range []string{`nav-badge rail-hide nav-badge-live`, `aria-label="Deployments, 2 in progress"`, `nav-dot nav-dot-live`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
}

// ---------------------------------------------------------------- checklist

func TestGettingStartedIsComputedFromWhatTheTeamHas(t *testing.T) {
	titles := func(c *Checklist) string {
		if c == nil {
			return "<none>"
		}
		var out []string
		for _, s := range c.Steps {
			mark := " "
			if s.Done {
				mark = "x"
			}
			out = append(out, "["+mark+"] "+s.Title)
		}
		return strings.Join(out, ", ")
	}

	for _, c := range []struct {
		name string
		in   checklistFacts
		want string
	}{
		{
			"a new team, on an install with everything",
			checklistFacts{Accounts: true, Domains: true, Members: 1},
			"[ ] Deploy your first app, [ ] See a deploy succeed, [ ] Add a custom domain, [ ] Invite a teammate",
		},
		{
			"part of the way",
			checklistFacts{Accounts: true, Domains: true, Members: 1,
				Onboarding: app.Onboarding{HasApps: true, HasSucceededDeploy: true}},
			"[x] Deploy your first app, [x] See a deploy succeed, [ ] Add a custom domain, [ ] Invite a teammate",
		},
		{
			// No accounts: nobody to invite, so no step that could never be done.
			"single owner",
			checklistFacts{Domains: true},
			"[ ] Deploy your first app, [ ] See a deploy succeed, [ ] Add a custom domain",
		},
		{
			"no custom domains on this install",
			checklistFacts{Accounts: true, Members: 2},
			"[ ] Deploy your first app, [ ] See a deploy succeed, [x] Invite a teammate",
		},
		{
			"everything done hides it",
			checklistFacts{Accounts: true, Domains: true, Members: 3,
				Onboarding: app.Onboarding{HasApps: true, HasSucceededDeploy: true, HasCustomDomain: true}},
			"<none>",
		},
		{
			"dismissed hides it",
			checklistFacts{Accounts: true, Domains: true, Onboarding: app.Onboarding{Dismissed: true}},
			"<none>",
		},
	} {
		if got := titles(gettingStarted(c.in)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}

	// The domain step goes to an app's domains tab once there is an app.
	c := gettingStarted(checklistFacts{Domains: true, FirstApp: "web"})
	if c.Steps[2].Href != "/apps/web/domains" {
		t.Errorf("domain step goes to %q", c.Steps[2].Href)
	}
	if c.Next() != 0 || c.Done() != 0 {
		t.Errorf("next %d done %d", c.Next(), c.Done())
	}
}

// A new team sees the checklist on the overview; dismissing it is a POST that
// puts it away for the team.
func TestTheChecklistShowsAndCanBePutAway(t *testing.T) {
	apps := newFakeApps()
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/").Body.String()
	for _, want := range []string{"Get started", `action="/onboarding/dismiss"`, `href="/apps/new"`, `role="progressbar"`} {
		if !strings.Contains(body, want) {
			t.Errorf("a new team's overview is missing %q", want)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/onboarding/dismiss", nil)
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /onboarding/dismiss = %d", rec.Code)
	}
	if len(apps.dismissed) != 1 || apps.dismissed[0] != "owner-1" {
		t.Fatalf("dismissed for %v, want the resolved owner", apps.dismissed)
	}
	if strings.Contains(get(t, h, "/").Body.String(), "Get started") {
		t.Error("the checklist is still shown after being dismissed")
	}
}

// ------------------------------------------------------------- sidebar tree

func TestTheProjectTreeShowsWhereYouAre(t *testing.T) {
	ws := app.Workspace{Projects: []app.WorkspaceProject{
		project("a"), project("b"), project("c"), project("d"), project("e"), project("f"),
		project("g", app.WorkspaceApp{Name: "deep", State: app.AppDeploying}),
	}}

	tree := sidebarProjects(ws, "/apps/deep/settings")
	if len(tree.Items) != sidebarProjectLimit || tree.More != 1 {
		t.Fatalf("listed %d, more %d", len(tree.Items), tree.More)
	}
	// The seventh project holds the app being looked at, so it is listed, and
	// open, in place of the sixth.
	last := tree.Items[sidebarProjectLimit-1]
	if last.Name != "G" || !last.Open || !last.Apps[0].Active {
		t.Errorf("the project being looked at is not listed and open: %+v", last)
	}
	if treeParentHref(tree) != "/projects" {
		t.Error("the Projects link is not marked as the tree's parent")
	}

	onProject := sidebarProjects(ws, "/projects/b")
	if !onProject.Items[1].Active || !onProject.Items[1].Open {
		t.Errorf("the project page does not mark its project: %+v", onProject.Items[1])
	}
	// A form is not an app called "new".
	if p, a := activeInTree("/apps/new"); p != "" || a != "" {
		t.Errorf("/apps/new read as %q/%q", p, a)
	}
}

// The tree, the palette and the sections, as markup: the roles a screen reader
// needs, one tab stop, and the Admin group last whatever order it arrived in.
func TestTheSidebarMarkup(t *testing.T) {
	ws := app.Workspace{Projects: []app.WorkspaceProject{
		project("shop", app.WorkspaceApp{Name: "web", State: app.AppServing},
			app.WorkspaceApp{Name: "api", State: app.AppDeployFailed}),
		project("blog"),
	}}
	ctx := workspaceCtx(ws)
	s := DefaultSlots{}.Slots(ctx, httptest.NewRequest("GET", "/apps/web", nil))
	// Admin first, as a wrapper might leave it.
	s.Nav = append([]NavGroup{{Heading: AdminNavHeading, Items: []NavItem{{Label: "Nodes", Href: "/cluster/nodes", Icon: "server"}}}}, s.Nav...)
	page := renderWith(t, ctx, Layout(s, templ.NopComponent))

	for _, want := range []string{
		`role="tree"`, `role="treeitem"`, `role="group"`,
		`aria-expanded="true"`, `aria-current="page"`,
		`data-tree-node="` + ws.Projects[0].ID.String() + `"`,
		`class="tree-dot status-warn"`, // the failed change, in the warning colour
		"yacht-tree",                   // the open projects restored before paint
		`href="/projects?new=1"`,
		// The palette: a modal dialog with a combobox driving a listbox.
		`<dialog id="palette"`, `role="combobox"`, `role="listbox"`,
		`data-index="/search/index"`, `data-palette-action`, `data-palette-open`,
		`aria-keyshortcuts="Meta+K Control+K /"`,
		"/assets/js/palette.js", "/assets/js/sidebar.js",
		// Where you are and who you are.
		`nav class="crumbs" aria-label="Breadcrumb"`,
		`class="sidebar-foot"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the sidebar is missing %q", want)
		}
	}

	// One tab stop in the tree.
	if n := strings.Count(page, `role="treeitem"`); n != 4 {
		t.Errorf("%d tree items, want 4", n)
	}
	stops := regexp.MustCompile(`role="treeitem"[^>]*tabindex="0"`).FindAllString(page, -1)
	if len(stops) != 1 {
		t.Errorf("%d tab stops in the tree, want 1", len(stops))
	}

	// Admin drawn after everything else in the navigation.
	admin := strings.Index(page, `class="nav-admin"`)
	settings := strings.Index(page, `href="/settings"`)
	if admin < 0 || settings < 0 || admin < settings {
		t.Errorf("the Admin section is not last (admin at %d, settings at %d)", admin, settings)
	}

	// The palette is outside the shell, which the phone drawer makes inert.
	if strings.Index(page, `<dialog id="palette"`) < strings.Index(page, "data-app-shell") {
		t.Error("the palette is drawn inside the shell")
	}
}

// Every palette action that goes somewhere goes somewhere that exists. The
// hrefs are data attributes rather than links, so the sidebar-link walk in
// registry_test.go does not see them.
func TestEveryPaletteActionResolves(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})
	page := get(t, h, "/").Body.String()
	urls := regexp.MustCompile(`data-url="([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(urls) < 3 {
		t.Fatalf("found %d palette actions", len(urls))
	}
	for _, m := range urls {
		href := strings.ReplaceAll(m[1], "{name}", "web")
		if strings.Contains(href, "/domains") {
			continue // needs the networking surface; offered only with it
		}
		if code := get(t, h, href).Code; code == http.StatusNotFound {
			t.Errorf("palette action goes to %s, which nothing serves", href)
		}
	}
}

// The top of the sidebar says where you are on every kind of install.
func TestTheTopOfTheSidebar(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	top := func(ctx context.Context) string {
		s := DefaultSlots{}.Slots(ctx, req)
		if s.SidebarTop == nil {
			return ""
		}
		return renderWith(t, ctx, s.SidebarTop)
	}
	owner := identity.NewContext(context.Background(), identity.Owner{ID: "o", DisplayName: "Local"})

	// No accounts: the owner, named, with nothing to open.
	solo := top(owner)
	if !strings.Contains(solo, "Local") || strings.Contains(solo, "<details") {
		t.Errorf("single owner top = %s", solo)
	}

	// Teams: a menu of them, the current one checked, and the team's settings.
	withTeams := context.WithValue(owner, teamsKey{}, teamLister(func() []TeamChoice {
		return []TeamChoice{
			{ID: "t1", Name: "Acme", Role: account.RoleAdmin, Active: true},
			{ID: "t2", Name: "Other", Role: account.RoleMember},
		}
	}))
	menu := top(withTeams)
	for _, want := range []string{
		`aria-haspopup="menu"`, `role="menu"`, `role="menuitemradio" aria-checked="true"`,
		`form="team-switch"`, `action="/teams/switch"`, `value="t2"`, `href="/team"`,
		">Admin<", // the viewer's role, under the team's name
	} {
		if !strings.Contains(menu, want) {
			t.Errorf("switcher is missing %q", want)
		}
	}
	// The team you are in is not a switch to post.
	if strings.Contains(menu, `value="t1"`) {
		t.Error("the current team is offered as a switch")
	}

	// Acting as a team: that team, said plainly, and no switcher.
	acting := context.WithValue(withTeams, surfacesKey{}, Surfaces{ActingAs: "Customer Co"})
	got := top(acting)
	if !strings.Contains(got, "Customer Co") || !strings.Contains(got, "Support access") ||
		strings.Contains(got, "/teams/switch") {
		t.Errorf("acting top = %s", got)
	}
}

func TestNavSectionsKeepAdminApart(t *testing.T) {
	lead, rest, admin := navSections([]NavGroup{
		{Items: []NavItem{{Label: "Overview"}}},
		{Heading: "Account"},
		{Heading: AdminNavHeading},
		{Items: []NavItem{{Label: "Late"}}}, // unheaded, but not leading
		{Heading: "System"},
	})
	if len(lead) != 1 || len(admin) != 1 || len(rest) != 3 || rest[1].Items[0].Label != "Late" {
		t.Errorf("lead %v rest %v admin %v", lead, rest, admin)
	}
}

func renderWith(t *testing.T, ctx context.Context, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(ctx, &b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

var _ io.Writer = (*strings.Builder)(nil)
