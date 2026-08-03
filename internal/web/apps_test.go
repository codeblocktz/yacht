package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// fakeApps is an in-memory Apps implementation.
//
// Scoped by owner like the real one, so a test that leaks across owners fails
// here rather than passing and failing in production.
type fakeApps struct {
	byOwner  map[string][]app.App
	created  []app.CreateInput
	deleted  []string
	scaled   map[string]int32
	activity app.DeployActivity
	err      error
}

func newFakeApps(apps ...app.App) *fakeApps {
	f := &fakeApps{byOwner: map[string][]app.App{}, scaled: map[string]int32{}}
	for _, a := range apps {
		f.byOwner[a.OwnerID] = append(f.byOwner[a.OwnerID], a)
	}
	return f
}

func (f *fakeApps) List(_ context.Context, ownerID string) ([]app.App, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byOwner[ownerID], nil
}

func (f *fakeApps) Count(_ context.Context, ownerID string) (int64, error) {
	return int64(len(f.byOwner[ownerID])), nil
}

func (f *fakeApps) Get(_ context.Context, ownerID, name string) (app.App, error) {
	for _, a := range f.byOwner[ownerID] {
		if a.Name == name {
			return a, nil
		}
	}
	return app.App{}, app.ErrNotFound
}

func (f *fakeApps) Create(_ context.Context, ownerID string, in app.CreateInput) (app.App, error) {
	if f.err != nil {
		return app.App{}, f.err
	}
	f.created = append(f.created, in)
	a := app.App{
		ID: uuid.New(), OwnerID: ownerID, Name: in.Name, Image: in.Image,
		Replicas: in.Replicas, Port: in.Port, Variables: varsFrom(in.Env),
		Namespace: app.Namespace(ownerID, in.Name),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	f.byOwner[ownerID] = append(f.byOwner[ownerID], a)
	return a, nil
}

func (f *fakeApps) Scale(_ context.Context, _, name string, replicas int32) (app.App, error) {
	f.scaled[name] = replicas
	return app.App{Name: name, Replicas: replicas}, nil
}

func (f *fakeApps) Redeploy(context.Context, string, string) error { return nil }

// DeployActivity backs the overview chart. Empty by default, so the chart is
// absent from pages that are not asserting on it; a test that wants it sets
// activity itself.
func (f *fakeApps) DeployActivity(
	_ context.Context, _ string, _ int,
) (app.DeployActivity, error) {
	return f.activity, nil
}

func (f *fakeApps) Deployments(
	_ context.Context, _ string, appID uuid.UUID, _ int32,
) ([]app.Deployment, error) {
	return []app.Deployment{
		{
			ID: uuid.New(), AppID: appID, Image: "nginx:alpine",
			Revision: "initial", Status: "running",
			StartedAt: time.Now().Add(-12 * time.Minute),
		},
		{
			ID: uuid.New(), AppID: appID, Image: "nginx:1.27",
			Revision: "redeploy", Status: "succeeded",
			StartedAt: time.Now().Add(-6 * 24 * time.Hour),
		},
	}, nil
}

func (f *fakeApps) Delete(_ context.Context, _, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeApps) RecentActivity(
	_ context.Context, ownerID string, _ int32,
) ([]app.Activity, error) {
	var out []app.Activity
	for _, a := range f.byOwner[ownerID] {
		out = append(out, app.Activity{
			Deployment: app.Deployment{
				ID: uuid.New(), AppID: a.ID, Image: a.Image,
				Revision: "initial", Status: "running",
				StartedAt: time.Now().Add(-3 * time.Minute),
			},
			AppName:      a.Name,
			AppNamespace: a.Namespace,
		})
	}
	return out, nil
}

func sampleApp(owner, name string) app.App {
	return app.App{
		ID: uuid.New(), OwnerID: owner, Name: name,
		Namespace: app.Namespace(owner, name),
		Image:     "nginx:alpine", Replicas: 2, Port: 8080,
		Variables: []app.Variable{{Key: "LOG_LEVEL", Value: "info"}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
		StatusKnown: true,
		Status: orchestrator.AppStatus{
			Phase: orchestrator.PhaseRunning, Desired: 2, Ready: 2, Available: 2,
		},
	}
}

func post(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAppListShowsApps(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"), sampleApp("owner-1", "api"))
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps").Body.String()
	for _, want := range []string{"web", "api", "nginx:alpine", "running"} {
		if !strings.Contains(body, want) {
			t.Errorf("apps page missing %q", want)
		}
	}
}

// The dashboard must only ever render the signed-in owner's apps. This is the
// single most important assertion in the package.
func TestAppListIsScopedToOwner(t *testing.T) {
	apps := newFakeApps(
		sampleApp("owner-1", "mine"),
		sampleApp("owner-2", "theirs"),
	)
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps").Body.String()
	if !strings.Contains(body, "mine") {
		t.Error("own app missing from the list")
	}
	if strings.Contains(body, "theirs") {
		t.Error("another owner's app leaked into the list")
	}
}

func TestAppDetailIsScopedToOwner(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-2", "theirs"))
	h := testServer(t, Options{Apps: apps})

	// Signed in as owner-1 (see testServer), asking for owner-2's app by name.
	if code := get(t, h, "/apps/theirs").Code; code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for another owner's app", code)
	}
}

func TestAppDetailRendersScaleAndPods(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	orch := orchestrator.NewNoop()
	_ = orch.ApplyApp(context.Background(), orchestrator.AppSpec{
		Ref: orchestrator.Ref{
			Owner: "owner-1", Namespace: app.Namespace("owner-1", "web"), Name: "web",
		},
		Image: "nginx:alpine", Replicas: 2,
	})

	h := testServer(t, Options{Apps: apps, Orchestrator: orch})
	body := get(t, h, "/apps/web").Body.String()

	// Deployments is the default tab, showing live state and history.
	for _, want := range []string{"web", "Redeploy", "Delete", "Deployments", "History"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
	if !strings.Contains(body, "12 minutes ago") {
		t.Error("expected a relative deploy time")
	}
	if !strings.Contains(body, "web-0") {
		t.Error("expected pods to be listed")
	}
}

// Each tab must be its own URL so it can be linked and bookmarked, and the
// rail must let you move between apps without returning to the index.
func TestAppDetailTabs(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"), sampleApp("owner-1", "api"))
	h := testServer(t, Options{Apps: apps})

	cases := map[string]string{
		"/apps/web/variables": "LOG_LEVEL",
		"/apps/web/metrics":   "Requested resources",
		"/apps/web/settings":  "Danger zone",
	}
	for path, want := range cases {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s missing %q", path, want)
		}
	}

	// The rail lists sibling apps on every tab.
	body := get(t, h, "/apps/web/settings").Body.String()
	if !strings.Contains(body, "/apps/api") {
		t.Error("expected the app rail to link to sibling apps")
	}
}

// A tab belonging to another owner's app must 404 like the base route does.
func TestAppDetailTabsAreScopedToOwner(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-2", "theirs"))
	h := testServer(t, Options{Apps: apps})

	for _, path := range []string{
		"/apps/theirs", "/apps/theirs/variables",
		"/apps/theirs/metrics", "/apps/theirs/settings",
	} {
		if code := get(t, h, path).Code; code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}

func TestAppDetailShowsTheHostnameAsALink(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.Host, a.TLS = "web.apps.example.com", true
	h := testServer(t, Options{Apps: newFakeApps(a)})

	body := get(t, h, "/apps/web").Body.String()

	if !strings.Contains(body, "web.apps.example.com") {
		t.Fatal("app detail does not show the hostname")
	}
	// A URL a user cannot click is a URL they have to retype.
	if !strings.Contains(body, `href="https://web.apps.example.com"`) {
		t.Fatal("the hostname is not rendered as a link")
	}
}

// An install without wildcard TLS must not be offered an https link that
// cannot connect.
func TestAppURLSchemeFollowsPlatformTLS(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.Host = "web.apps.example.com"
	h := testServer(t, Options{Apps: newFakeApps(a)})

	body := get(t, h, "/apps/web").Body.String()

	if !strings.Contains(body, `href="http://web.apps.example.com"`) {
		t.Error("expected an http link when the platform does not serve TLS")
	}
	if strings.Contains(body, `href="https://web.apps.example.com"`) {
		t.Error("https link offered when the platform does not serve TLS")
	}
}

func TestAppWithNoHostnameShowsNoLink(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})

	body := get(t, h, "/apps/web").Body.String()

	for _, empty := range []string{`href="https://"`, `href="http://"`} {
		if strings.Contains(body, empty) {
			t.Errorf("an app with no hostname rendered an empty link: %s", empty)
		}
	}
}

func TestAppListShowsTheHostname(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.Host = "web.apps.example.com"
	h := testServer(t, Options{Apps: newFakeApps(a)})

	if !strings.Contains(get(t, h, "/apps").Body.String(), "web.apps.example.com") {
		t.Error("the app list does not show the hostname")
	}
}

// The settings page is where an operator finds out whether per-app hostnames
// are on at all, and what DNS record makes them work.
func TestSettingsShowsTheAppDomain(t *testing.T) {
	h := testServer(t, Options{AppDomain: "apps.example.com", WildcardTLS: true})
	body := get(t, h, "/settings").Body.String()

	if !strings.Contains(body, "*.apps.example.com") {
		t.Error("settings should show the wildcard app domain")
	}
	if !strings.Contains(body, "Wildcard certificate") {
		t.Error("settings should report the platform TLS posture")
	}

	h = testServer(t, Options{})
	body = get(t, h, "/settings").Body.String()
	if !strings.Contains(body, "YACHT_APP_DOMAIN") {
		t.Error("settings should name the variable that turns per-app hostnames on")
	}
}

// The stylesheet must be reachable without a credential, or every error page
// and future login screen renders unstyled.
func TestAssetsAreServedUnauthenticated(t *testing.T) {
	denied := identity.ProviderFunc(func(context.Context, *http.Request) (identity.Owner, error) {
		return identity.Owner{}, identity.ErrUnauthenticated
	})
	h := testServer(t, Options{Identity: denied})

	rec := get(t, h, "/assets/css/app.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q, want text/css", ct)
	}
	// The compiled theme must actually be in there.
	if !strings.Contains(rec.Body.String(), "--background") {
		t.Error("stylesheet does not contain the theme tokens — was `make css` run?")
	}
}

// Cache headers must distinguish fingerprinted from bare requests, or a deploy
// leaves operators on a stale stylesheet.
func TestAssetCaching(t *testing.T) {
	h := testServer(t, Options{})

	bare := get(t, h, "/assets/css/app.css").Header().Get("Cache-Control")
	if strings.Contains(bare, "immutable") {
		t.Errorf("unfingerprinted asset should not be immutable, got %q", bare)
	}

	req := httptest.NewRequest(http.MethodGet, "/assets/css/app.css?v=abc123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("fingerprinted asset should be immutable, got %q", got)
	}
}

// The layout must reference the fingerprinted URL, not the bare path.
func TestStylesheetIsFingerprinted(t *testing.T) {
	h := testServer(t, Options{})
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, "/assets/css/app.css?v=") {
		t.Error("layout should link the fingerprinted stylesheet")
	}
}

// Theme resolution must happen before paint, or dark-mode users get a white
// flash on every navigation.
func TestThemeIsResolvedInline(t *testing.T) {
	h := testServer(t, Options{})
	body := get(t, h, "/").Body.String()

	head := body
	if i := strings.Index(body, "</head>"); i > 0 {
		head = body[:i]
	}
	if !strings.Contains(head, "yacht-theme") {
		t.Error("theme script must be inline in <head>, before first paint")
	}
	if !strings.Contains(head, "prefers-color-scheme") {
		t.Error("theme script should fall back to the OS preference")
	}
}

func TestCreateApp(t *testing.T) {
	apps := newFakeApps()
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps", url.Values{
		"name":     {"web"},
		"image":    {"nginx:alpine"},
		"port":     {"8080"},
		"replicas": {"2"},
		"env":      {"LOG_LEVEL=info\nAPP_ENV=prod"},
	})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/apps/web" {
		t.Errorf("redirect = %q, want /apps/web", loc)
	}
	if len(apps.created) != 1 {
		t.Fatalf("created %d apps, want 1", len(apps.created))
	}
	in := apps.created[0]
	if in.Name != "web" || in.Image != "nginx:alpine" || in.Port != 8080 || in.Replicas != 2 {
		t.Errorf("unexpected input: %+v", in)
	}
	if in.Env["LOG_LEVEL"] != "info" || in.Env["APP_ENV"] != "prod" {
		t.Errorf("env not parsed: %v", in.Env)
	}
}

// A rejected submission must come back with the fields still filled in.
// Discarding a user's typing on a validation error is a small cruelty that
// makes a form feel broken.
func TestCreateAppPreservesInputOnError(t *testing.T) {
	apps := newFakeApps()
	apps.err = errors.New("image pull is not permitted")
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps", url.Values{
		"name": {"web"}, "image": {"nginx:alpine"}, "port": {"8080"}, "replicas": {"3"},
	})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "image pull is not permitted") {
		t.Error("error message not shown")
	}
	for _, want := range []string{`value="web"`, `value="nginx:alpine"`, `value="3"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form did not preserve %s", want)
		}
	}
}

func TestCreateAppRejectsMalformedEnv(t *testing.T) {
	apps := newFakeApps()
	h := testServer(t, Options{Apps: apps})

	rec := post(t, h, "/apps", url.Values{
		"name": {"web"}, "image": {"nginx:alpine"}, "env": {"NOT_A_PAIR"},
	})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	if len(apps.created) != 0 {
		t.Error("app must not be created when the env block is malformed")
	}
}

func TestScaleAndDelete(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	if rec := post(t, h, "/apps/web/scale", url.Values{"replicas": {"5"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("scale status = %d, want 303", rec.Code)
	}
	if apps.scaled["web"] != 5 {
		t.Errorf("scaled to %d, want 5", apps.scaled["web"])
	}

	if rec := post(t, h, "/apps/web/delete", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("delete status = %d, want 303", rec.Code)
	}
	if len(apps.deleted) != 1 || apps.deleted[0] != "web" {
		t.Errorf("deleted = %v, want [web]", apps.deleted)
	}
}

// Mutations must be POST. A GET that changes state can be triggered by a
// prefetch or a crawler.
func TestMutationsRejectGET(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})

	for _, path := range []string{"/apps/web/scale", "/apps/web/delete", "/apps/web/redeploy"} {
		if code := get(t, h, path).Code; code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, code)
		}
	}
	if len(apps.deleted) != 0 {
		t.Error("a GET must not have deleted anything")
	}
}

func TestClusterPageRendersEmptyState(t *testing.T) {
	h := testServer(t, Options{})

	body := get(t, h, "/cluster/nodes").Body.String()
	if !strings.Contains(body, "No nodes") {
		t.Error("expected an explanatory empty state when no cluster is connected")
	}
	if !strings.Contains(body, "YACHT_KUBECONFIG") {
		t.Error("empty state should tell the operator what to check")
	}
}

func TestClusterPodsTab(t *testing.T) {
	orch := orchestrator.NewNoop()
	_ = orch.ApplyApp(context.Background(), orchestrator.AppSpec{
		Ref:   orchestrator.Ref{Owner: "owner-1", Namespace: "yacht-demo", Name: "web"},
		Image: "nginx:alpine", Replicas: 2,
	})
	h := testServer(t, Options{Orchestrator: orch})

	body := get(t, h, "/cluster/pods").Body.String()
	for _, want := range []string{"web-0", "web-1", "yacht-demo"} {
		if !strings.Contains(body, want) {
			t.Errorf("pods tab missing %q", want)
		}
	}
}

func TestSettingsWarnsWhenUnauthenticated(t *testing.T) {
	h := testServer(t, Options{Authenticated: false})
	body := get(t, h, "/settings").Body.String()
	if !strings.Contains(body, "YACHT_AUTH_TOKEN") {
		t.Error("settings should warn when no credential is required")
	}

	h = testServer(t, Options{Authenticated: true})
	body = get(t, h, "/settings").Body.String()
	if strings.Contains(body, "No authentication configured") {
		t.Error("warning should disappear once a token is configured")
	}
}

// The dashboard must stay usable with no database wired up, because that is
// the state a first-time operator hits before finishing configuration.
func TestPagesRenderWithoutAnAppService(t *testing.T) {
	h := testServer(t, Options{})
	for _, path := range []string{"/", "/apps", "/apps/new", "/cluster/nodes", "/settings"} {
		if code := get(t, h, path).Code; code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, code)
		}
	}
}

// Storage is exercised against the real service and a real database in
// storage_test.go — what it claims is facts about rows and a Deployment, which
// a fake can only echo back. These exist so fakeApps satisfies the interface.

func (f *fakeApps) AttachVolume(
	context.Context, string, string, app.VolumeInput,
) (app.Volume, error) {
	return app.Volume{}, errors.New("fakeApps has no storage")
}

func (f *fakeApps) ResizeVolume(context.Context, string, string, string, int64) error {
	return errors.New("fakeApps has no storage")
}

func (f *fakeApps) DeleteVolume(context.Context, string, string, string, bool) error {
	return errors.New("fakeApps has no storage")
}

// varsFrom turns a fixture map into the variable rows an app now carries.
func varsFrom(env map[string]string) []app.Variable {
	out := make([]app.Variable, 0, len(env))
	for k, v := range env {
		out = append(out, app.Variable{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (f *fakeApps) SetVariable(context.Context, string, string, app.VariableInput) error {
	return errors.New("fakeApps has no variables")
}

func (f *fakeApps) DeleteVariable(context.Context, string, string, string) error {
	return errors.New("fakeApps has no variables")
}

func (f *fakeApps) SetHealth(context.Context, string, string, string, bool) error {
	return errors.New("fakeApps has no health probe")
}

func (f *fakeApps) Links(context.Context, string) ([]app.Link, error) { return nil, nil }

// ---- projects and canvas arrangement
//
// One project holding every app the fake knows about. The canvas tests are
// about what the page draws, not about which project an app is filed under, and
// a fake that made them file one first would test the fake.

var fakeProject = app.Project{
	ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	Slug: app.DefaultProjectSlug,
	Name: app.DefaultProjectName,
}

func (f *fakeApps) Projects(_ context.Context, ownerID string) ([]app.Project, error) {
	p := fakeProject
	p.OwnerID = ownerID
	p.Apps = int64(len(f.byOwner[ownerID]))
	return []app.Project{p}, nil
}

func (f *fakeApps) Project(_ context.Context, ownerID, slug string) (app.Project, error) {
	if slug != "" && slug != app.DefaultProjectSlug {
		return app.Project{}, app.ErrProjectNotFound
	}
	p := fakeProject
	p.OwnerID = ownerID
	return p, nil
}

func (f *fakeApps) ProjectByID(_ context.Context, ownerID string, id uuid.UUID) (app.Project, error) {
	if id != fakeProject.ID {
		return app.Project{}, app.ErrProjectNotFound
	}
	p := fakeProject
	p.OwnerID = ownerID
	return p, nil
}

func (f *fakeApps) CreateProject(_ context.Context, ownerID, name string) (app.Project, error) {
	if f.err != nil {
		return app.Project{}, f.err
	}
	return app.Project{OwnerID: ownerID, Slug: app.Slugify(name), Name: name}, nil
}

func (f *fakeApps) ListInProject(_ context.Context, ownerID string, _ uuid.UUID) ([]app.App, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byOwner[ownerID], nil
}

func (f *fakeApps) SetPosition(_ context.Context, ownerID, name string, x, y int32) error {
	if f.err != nil {
		return f.err
	}
	for i, a := range f.byOwner[ownerID] {
		if a.Name == name {
			f.byOwner[ownerID][i].X, f.byOwner[ownerID][i].Y = &x, &y
			return nil
		}
	}
	return app.ErrNotFound
}

func (f *fakeApps) ClearPositions(_ context.Context, ownerID string, _ uuid.UUID) error {
	for i := range f.byOwner[ownerID] {
		f.byOwner[ownerID][i].X, f.byOwner[ownerID][i].Y = nil, nil
	}
	return nil
}

// An app's URL renders its canvas with its panel open, not a page of its own.
//
// The relationships are the point: what a service reads and what reads it stay
// on screen while somebody changes a variable. A detail page that replaced the
// canvas would hide every one of them at exactly that moment.
func TestAnAppURLRendersItsCanvasWithThePanelOpen(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"), sampleApp("owner-1", "db"))
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps/web").Body.String()

	// The canvas is behind the panel, with every sibling still drawn.
	for _, want := range []string{`data-canvas`, `data-node="web"`, `data-node="db"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the app URL did not render the canvas: missing %q", want)
		}
	}
	// Open on arrival. Fetching the panel afterwards would show an empty sheet
	// to anybody who navigated straight here.
	if !strings.Contains(body, `data-tui-dialog-open="true"`) {
		t.Error("the panel is not open on a URL that names an app")
	}
	// Everything the app page used to be able to do, it can still do.
	for _, want := range []string{"Redeploy", "Delete", "Variables", "Metrics", "Storage", "Settings"} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel lost the app page's %q", want)
		}
	}
}

// The canvas is the project's, so it must not draw another team's apps.
func TestTheCanvasIsScopedToOwner(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "mine"), sampleApp("owner-2", "theirs"))
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/projects/default").Body.String()
	if !strings.Contains(body, `data-node="mine"`) {
		t.Error("own app missing from the canvas")
	}
	if strings.Contains(body, "theirs") {
		t.Error("another owner's app leaked onto the canvas")
	}
}

// A project a team does not have is a 404, not somebody else's canvas.
func TestAnUnknownProjectIsNotFound(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})

	if code := get(t, h, "/projects/someone-elses").Code; code != http.StatusNotFound {
		t.Fatalf("unknown project returned %d, want 404", code)
	}
}
