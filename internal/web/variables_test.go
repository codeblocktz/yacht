package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// stagingApps is the fake service with everything the Variables tab, the
// changes bar and the shared variables can ask of it.
type stagingApps struct {
	*fakeApps
	changes  []app.StagedChange
	platform []app.Variable
	values   map[string]string
	shared   []app.Variable

	deployed, discarded []string
	raw                 string
	rawErr              error
	sharedSet           []app.VariableInput
}

func newStagingApps(apps ...app.App) *stagingApps {
	return &stagingApps{fakeApps: newFakeApps(apps...), values: map[string]string{}}
}

func (f *stagingApps) StagesChanges() bool { return true }

func (f *stagingApps) StagedChanges(_ context.Context, _, appName string) ([]app.StagedChange, error) {
	var out []app.StagedChange
	for _, c := range f.changes {
		if c.App == appName {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *stagingApps) DeployStaged(_ context.Context, _, appName string) error {
	f.deployed = append(f.deployed, appName)
	return nil
}

func (f *stagingApps) DiscardStaged(_ context.Context, _, appName string) ([]string, error) {
	f.discarded = append(f.discarded, appName)
	return []string{"TOKEN"}, nil
}

func (f *stagingApps) ProjectStagedChanges(context.Context, string, string) ([]app.StagedChange, error) {
	return f.changes, nil
}

func (f *stagingApps) DeployProjectStaged(_ context.Context, _, slug string) ([]string, error) {
	f.deployed = append(f.deployed, "project:"+slug)
	return []string{"api", "web"}, nil
}

func (f *stagingApps) DiscardProjectStaged(_ context.Context, _, slug string) ([]string, error) {
	f.discarded = append(f.discarded, "project:"+slug)
	return nil, nil
}

func (f *stagingApps) RevealVariable(_ context.Context, _, appName, key string) (string, error) {
	v, ok := f.values[appName+"."+key]
	if !ok {
		return "", app.ErrVariableNotFound
	}
	return v, nil
}

func (f *stagingApps) SetRawVariables(_ context.Context, _, _, raw string) error {
	f.raw = raw
	return f.rawErr
}

func (f *stagingApps) PlatformVariables(context.Context, string, string) ([]app.Variable, error) {
	return f.platform, nil
}

func (f *stagingApps) SharedVariables(context.Context, string, string) ([]app.Variable, error) {
	return f.shared, nil
}

func (f *stagingApps) SetSharedVariable(_ context.Context, _, _ string, in app.VariableInput) error {
	f.sharedSet = append(f.sharedSet, in)
	return nil
}

func (f *stagingApps) DeleteSharedVariable(context.Context, string, string, string) error { return nil }

func (f *stagingApps) RevealSharedVariable(_ context.Context, _, slug, key string) (string, error) {
	return f.RevealVariable(context.Background(), "", "shared:"+slug, key)
}

func webWithVariables() app.App {
	web := sampleApp("owner-1", "web")
	web.Variables = []app.Variable{
		{Key: "MODE", Value: "production"},
		{Key: "TOKEN", Secret: true, Staged: app.StagedChanged},
		{Key: "YACHT_SERVICE_NAME", Value: "mine"},
	}
	return web
}

// Every value is drawn masked with an eye; a plain one is in the page,
// hidden, and a secret's is not in the page at all until its eye asks.
func TestEveryValueIsMaskedUntilItsEyeIsPressed(t *testing.T) {
	apps := newStagingApps(webWithVariables())
	apps.values["web.TOKEN"] = "hunter2"
	var logged bytes.Buffer
	h := testServer(t, Options{Apps: apps, Logger: slog.New(slog.NewTextHandler(&logged, nil))})

	body := get(t, h, "/apps/web/variables").Body.String()
	for _, want := range []string{
		`data-var`, `class="var-mask"`, `data-reveal="MODE"`, `id="var-app-MODE">production<`,
		`hx-get="/apps/web/variables/TOKEN/value"`, `hx-trigger="click once"`,
		`>edited<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab is missing %q", want)
		}
	}
	if strings.Contains(body, "hunter2") {
		t.Fatal("a secret's value is in the page before anybody asked for it")
	}
	if strings.Contains(body, "cannot be shown") {
		t.Error("the tab still says a secret cannot be shown again")
	}

	rec := get(t, h, "/apps/web/variables/TOKEN/value")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "hunter2" {
		t.Fatalf("reveal = %d %q", rec.Code, rec.Body.String())
	}
	line := logged.String()
	if !strings.Contains(line, "variable revealed") || !strings.Contains(line, "key=TOKEN") ||
		!strings.Contains(line, "app=web") || !strings.Contains(line, "by=team:owner-1") {
		t.Errorf("the reveal was logged as %q", line)
	}
	if strings.Contains(line, "hunter2") {
		t.Error("the log carries the value")
	}
	if rec := get(t, h, "/apps/web/variables/NOPE/value"); rec.Code != http.StatusNotFound {
		t.Errorf("revealing a key that is not there = %d", rec.Code)
	}
}

// Revealing is reading, so a member may; it is still logged as them.
func TestAMemberMayRevealAValue(t *testing.T) {
	const team = "web-reveal"
	apps := newStagingApps(webWithVariablesFor(team))
	apps.values["web.TOKEN"] = "hunter2"
	s, err := New(Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            apps,
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: account.RoleMember},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/apps/web/variables/TOKEN/value", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "member-session"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("a member's reveal = %d %q", rec.Code, rec.Body.String())
	}
}

func webWithVariablesFor(team string) app.App {
	a := webWithVariables()
	a.OwnerID = team
	return a
}

// The Raw Editor holds every variable but the secrets, names those it left
// out, and hands back the text as typed.
func TestTheRawEditorLeavesSecretsOut(t *testing.T) {
	apps := newStagingApps(webWithVariables())
	h := testServer(t, Options{Apps: apps})
	body := get(t, h, "/apps/web/variables").Body.String()
	if !strings.Contains(body, "Raw Editor") || !strings.Contains(body, "MODE=production\n") {
		t.Fatal("the raw editor does not hold the plain variables")
	}
	if strings.Contains(body, "TOKEN=") {
		t.Error("a secret is in the raw editor")
	}
	if !strings.Contains(body, "1 secret") || !strings.Contains(body, `<span class="mono">TOKEN</span>`) {
		t.Error("the raw editor does not say which secret it left out")
	}
	if strings.Contains(body, "YACHT_SECRET_KEY") {
		t.Error("the Variables tab names the engine's encryption key setting")
	}

	rec := post(t, h, "/apps/web/variables/raw", url.Values{"raw": {"A=1\nB=2\n"}})
	if rec.Code != http.StatusSeeOther || apps.raw != "A=1\nB=2\n" {
		t.Errorf("raw save = %d, stored %q", rec.Code, apps.raw)
	}
}

// The platform's variables are listed apart, folded, under the brand; one the
// app replaces with its own says so instead of showing a value it will not get.
func TestThePlatformsVariablesAreListedApart(t *testing.T) {
	apps := newStagingApps(webWithVariables())
	apps.platform = []app.Variable{
		{Key: "YACHT_PUBLIC_DOMAIN", Value: "web.apps.example.com"},
		{Key: "YACHT_SERVICE_NAME", Value: "web"},
	}
	h := testServer(t, Options{Apps: apps, WakerBrand: "datahuru"})
	body := get(t, h, "/apps/web/variables").Body.String()
	for _, want := range []string{
		"2 variables added by datahuru", `<details class="panel platform-vars`,
		`id="var-platform-YACHT_PUBLIC_DOMAIN">web.apps.example.com<`,
		"Replaced by this app’s own YACHT_SERVICE_NAME",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab is missing %q", want)
		}
	}
}

// The canvas shows a bar while anything is waiting, a marker on each card
// with changes, and the bar's buttons act on the whole project.
func TestTheCanvasBarDeploysEveryChange(t *testing.T) {
	apps := newStagingApps(sampleApp("owner-1", "web"), sampleApp("owner-1", "api"))
	apps.changes = []app.StagedChange{
		{App: "web", Key: "MODE", Kind: app.StagedChanged},
		{App: "web", Key: "Port", Setting: true, Kind: app.StagedChanged, From: "8080", To: "9090"},
		{App: "api", Key: "NEW", Kind: app.StagedAdded},
	}
	h := testServer(t, Options{Apps: apps})
	body := get(t, h, "/projects/default").Body.String()
	for _, want := range []string{
		`class="staged-bar"`, "3 changes", "Details", "Port: 8080 → 9090",
		`action="/projects/default/changes/deploy"`, `action="/projects/default/changes/discard"`,
		`class="staged-marker" title="2 changes not deployed"`,
		`class="staged-marker" title="1 change not deployed"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the canvas is missing %q", want)
		}
	}

	rec := post(t, h, "/projects/default/changes/deploy", url.Values{"next": {"/projects/default"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/default" {
		t.Errorf("deploy = %d → %s", rec.Code, rec.Header().Get("Location"))
	}
	post(t, h, "/projects/default/changes/discard", url.Values{"next": {"//evil.example"}})
	if strings.Join(apps.deployed, ",") != "project:default" || strings.Join(apps.discarded, ",") != "project:default" {
		t.Errorf("deployed %v, discarded %v", apps.deployed, apps.discarded)
	}

	// With nothing waiting there is no bar.
	apps.changes = nil
	if strings.Contains(get(t, h, "/projects/default").Body.String(), `class="staged-bar"`) {
		t.Error("the bar shows with nothing waiting")
	}
}

// A service that deploys every save draws no bar, however its data looks.
func TestNoBarWhereSavesDeploy(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})
	if strings.Contains(get(t, h, "/projects/default").Body.String(), "staged-bar") {
		t.Error("a bar is drawn where saves deploy")
	}
}

// One app's waiting changes are at the top of its tab, with its own Deploy
// and Discard; Discard says what it could not undo.
func TestAnAppsChangesAreOnItsTab(t *testing.T) {
	apps := newStagingApps(webWithVariables())
	apps.changes = []app.StagedChange{{App: "web", Key: "TOKEN", Kind: app.StagedChanged,
		Note: "Saved since the last deploy. Discard keeps this value.", Kept: true}}
	h := testServer(t, Options{Apps: apps})
	body := get(t, h, "/apps/web/variables").Body.String()
	for _, want := range []string{
		"1 change not deployed", `action="/apps/web/changes/deploy"`, `action="/apps/web/changes/discard"`,
		"Discard keeps this value",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab is missing %q", want)
		}
	}
	rec := post(t, h, "/apps/web/changes/discard", nil)
	if rec.Code != http.StatusSeeOther || len(apps.discarded) != 1 {
		t.Fatalf("discard = %d (%v)", rec.Code, apps.discarded)
	}
	post(t, h, "/apps/web/changes/deploy", nil)
	if len(apps.deployed) != 1 || apps.deployed[0] != "web" {
		t.Errorf("deployed %v", apps.deployed)
	}
}

// Project Settings has the shared variables, masked, and a form for them.
func TestSharedVariablesAreInProjectSettings(t *testing.T) {
	apps := newStagingApps(sampleApp("owner-1", "web"))
	apps.shared = []app.Variable{{Key: "REGION", Value: "eu"}, {Key: "SIGNING_KEY", Secret: true}}
	apps.values["shared:default.SIGNING_KEY"] = "k1"
	h := testServer(t, Options{Apps: apps})
	body := get(t, h, "/projects/default/settings").Body.String()
	for _, want := range []string{
		`id="shared-variables"`, "Shared Variables", `id="var-shared-REGION">eu<`,
		`hx-get="/projects/default/variables/SIGNING_KEY/value"`, `action="/projects/default/variables"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project settings are missing %q", want)
		}
	}
	rec := post(t, h, "/projects/default/variables", url.Values{"key": {"PLAN"}, "value": {"pro"}})
	if rec.Code != http.StatusSeeOther || len(apps.sharedSet) != 1 || apps.sharedSet[0].Key != "PLAN" {
		t.Errorf("set = %d %+v", rec.Code, apps.sharedSet)
	}
	if rec := get(t, h, "/projects/default/variables/SIGNING_KEY/value"); strings.TrimSpace(rec.Body.String()) != "k1" {
		t.Errorf("shared reveal = %q", rec.Body.String())
	}
}

// The picker offers the project's shared variables by that name, and a
// reference to one is drawn without a link to an app that does not exist.
func TestSharedVariablesArePickedLikeAnAppsOwn(t *testing.T) {
	web := sampleApp("owner-1", "web")
	web.Variables = []app.Variable{{Key: "KEY", Value: "${{shared.SIGNING_KEY}}", Reference: true}}
	apps := referenceApps{fakeApps: newFakeApps(web), targets: []app.ReferenceTarget{{
		App: app.SharedScope, Keys: []app.ReferenceKey{{Key: "SIGNING_KEY", Secret: true}},
	}}}
	h := testServer(t, Options{Apps: apps})
	body := get(t, h, "/apps/web/variables").Body.String()
	if !strings.Contains(body, `value="shared.SIGNING_KEY"`) {
		t.Error("the picker does not offer the shared variable")
	}
	if strings.Contains(body, `href="/apps/shared/variables"`) {
		t.Error("a shared reference links to an app called shared")
	}
	post(t, h, "/apps/web/variables", url.Values{"ref": {"shared.SIGNING_KEY"}})
	if got := apps.setVars; len(got) != 1 || got[0].Value != "${{shared.SIGNING_KEY}}" {
		t.Errorf("set = %+v", got)
	}
}

// A Raw Editor text the service refuses comes back as it was typed, on the
// Variables tab as it always is — the canvas, with the panel open on it — and
// with the line it was refused for quoted beside the reason. From htmx it is
// the tab alone, to swap in where it was; a save from htmx is a page load.
func TestARefusedRawEditKeepsTheTextAndPointsAtTheLine(t *testing.T) {
	apps := newStagingApps(webWithVariables())
	apps.rawErr = &app.EnvLineError{Line: 3, Err: errors.New(`"this is not a valid line" has no = — write it as KEY=value`)}
	h := testServer(t, Options{Apps: apps})
	typed := "A=1\nB=2\nthis is not a valid line\nC=<3>\n"

	rec := post(t, h, "/apps/web/variables/raw", url.Values{"raw": {typed}})
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	for _, want := range []string{
		`class="canvas`,                          // the canvas, not a page of its own
		`id="variables-tab"`,                     // with the Variables tab open
		`<details class="panel raw-editor" open`, // and the Raw Editor open on it
		"A=1\nB=2\nthis is not a valid line\nC=&lt;3&gt;\n</textarea>",
		`data-error-line="3"`, `aria-invalid="true"`,
		"Line 3", "has no = — write it as KEY=value",
		"Nothing was saved.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal is missing %q", want)
		}
	}
	if strings.Contains(body, "line 3: ") {
		t.Error("the line number is said twice, once in the reason")
	}

	req := httptest.NewRequest(http.MethodPost, "/apps/web/variables/raw", strings.NewReader(url.Values{"raw": {typed}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("HX-Request", "true")
	rec = do(h, req)
	body = rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.HasPrefix(strings.TrimSpace(body), `<div id="variables-tab">`) {
		t.Errorf("from htmx = %d, starting %.60q; want 422 and the tab alone", rec.Code, body)
	}
	if strings.Contains(body, "<html") || !strings.Contains(body, "this is not a valid line\nC=&lt;3&gt;") {
		t.Error("the fragment is a whole page, or lost the text")
	}

	apps.rawErr = nil
	req = httptest.NewRequest(http.MethodPost, "/apps/web/variables/raw", strings.NewReader(url.Values{"raw": {"A=1\n"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("HX-Request", "true")
	rec = do(h, req)
	if rec.Header().Get("HX-Redirect") != "/apps/web/variables" {
		t.Errorf("a save from htmx = %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}

// A variable the service refuses keeps what was typed in the Set form, with
// the reason beside that form rather than at the top of the tab.
func TestARefusedVariableKeepsTheForm(t *testing.T) {
	apps := newStagingApps(webWithVariables())
	apps.setVarErr = errors.New("${{api.NOPE}} refers to NOPE, which api does not have")
	h := testServer(t, Options{Apps: apps})
	rec := post(t, h, "/apps/web/variables", url.Values{"key": {"MISSING_REF"}, "value": {"${{api.NOPE}}"}})
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", rec.Code)
	}
	for _, want := range []string{
		`class="canvas`, `value="MISSING_REF"`, `value="${{api.NOPE}}"`,
		"refers to NOPE, which api does not have",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal is missing %q", want)
		}
	}
	at := strings.Index(body, `id="variables-tab"`)
	if at < 0 {
		t.Fatal("the refusal is not drawn on the Variables tab")
	}
	tab := body[at:]
	if reason, form := strings.Index(tab, "refers to NOPE"), strings.Index(tab, `id="var-key"`); reason < 0 || form < 0 || reason > form ||
		reason < strings.Index(tab, "Set a variable") {
		t.Error("the reason is not beside the Set form")
	}
}
