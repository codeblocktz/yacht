package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

type fakeStacks struct {
	deployed []string
	err      error
}

func (f *fakeStacks) DeployTemplate(
	_ context.Context, _, slug, stack string,
) (app.Project, error) {
	if f.err != nil {
		return app.Project{}, f.err
	}
	f.deployed = append(f.deployed, slug+"/"+stack)
	return app.Project{Slug: stack, Name: stack}, nil
}

func stackServer(t *testing.T, st Stacks) http.Handler {
	t.Helper()
	const team = "stack-team"
	opts := Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            newFakeApps(sampleApp(team, "probe")),
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: "owner"},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		Stacks:          st,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

// The picker used to say templates were not built yet. It must now lead
// somewhere, and to the templates page rather than the create form — that form
// makes one app, and a stack is several.
func TestThePickerOffersTemplatesAndLeadsToThem(t *testing.T) {
	h := stackServer(t, &fakeStacks{})

	body := do(h, signedIn(http.MethodGet, "/apps/new")).Body.String()

	if strings.Contains(body, "no templates are defined yet") {
		t.Error("the picker still says templates are not built")
	}
	if !strings.Contains(body, `href="/templates"`) {
		t.Error("the template entry does not lead to the templates page")
	}
	if strings.Contains(body, `href="/apps/new?source=template"`) {
		t.Error("a template was sent to the form that makes exactly one app")
	}
}

// The page has to say what a stack will be called before it is deployed, and
// which app gets which address — the wiring is the reason to use one.
func TestTheTemplatePageShowsTheStackItWouldMake(t *testing.T) {
	h := stackServer(t, &fakeStacks{})

	body := do(h, signedIn(http.MethodGet, "/templates")).Body.String()

	for _, want := range []string{"&lt;stack&gt;-db", "&lt;stack&gt;-web", "gets db"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show what it would make: missing %q", want)
		}
	}
}

// Deploying opens the canvas, because the wiring is the thing worth seeing and
// the canvas is the only place that shows it.
func TestDeployingAStackOpensItsCanvas(t *testing.T) {
	st := &fakeStacks{}
	h := stackServer(t, st)

	rec := do(h, signedInForm(http.MethodPost, "/templates", "template=postgres-app&stack=shop"))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/projects/shop" {
		t.Errorf("redirected to %q, want the stack's canvas", loc)
	}
	if len(st.deployed) != 1 || st.deployed[0] != "postgres-app/shop" {
		t.Fatalf("deployed = %v, want [postgres-app/shop]", st.deployed)
	}
}

// A refusal keeps what was typed. Retyping a stack name from nothing because
// the first one was taken is the sort of thing that makes a form feel hostile.
func TestARefusedStackKeepsWhatWasTyped(t *testing.T) {
	h := stackServer(t, &fakeStacks{err: errNameTaken{}})

	rec := do(h, signedInForm(http.MethodPost, "/templates", "template=postgres-app&stack=shop"))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="shop"`) {
		t.Error("the stack name was not kept after a refusal")
	}
	if !strings.Contains(body, "already exists") {
		t.Error("the refusal does not say what was wrong")
	}
}

type errNameTaken struct{}

func (errNameTaken) Error() string { return `a project called "shop" already exists` }

// With no key to seal credentials the surface is off rather than failing after
// creating the first app of a stack.
func TestWithoutStacksTheRoutesAreAbsent(t *testing.T) {
	h := stackServer(t, nil)

	if code := do(h, signedIn(http.MethodGet, "/templates")).Code; code != http.StatusNotFound {
		t.Errorf("templates page without the ability = %d, want 404", code)
	}
}

// signedInForm is a signed-in POST carrying a form body.
func signedInForm(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "probe-session"})
	req.Header.Set("Origin", "https://yacht.test")
	return req
}
