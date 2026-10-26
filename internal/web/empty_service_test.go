package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// buildingApps is an install that builds from source.
type buildingApps struct{ *fakeApps }

func (buildingApps) Sources(context.Context) []app.Blueprint {
	return app.BlueprintsFor(app.Capabilities{CanBuild: true})
}

func (buildingApps) Capabilities(context.Context) app.Capabilities {
	return app.Capabilities{CanBuild: true}
}

func buildingServer(t *testing.T, apps *fakeApps) http.Handler {
	t.Helper()
	const team = "owner-1"
	s, err := New(Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            buildingApps{apps},
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: "owner"},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		CLIName:         "datahuru",
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

// Railway's Empty Service: on the canvas's Add and the deploy page, a form
// that asks for a name, and a create the engine is told is empty.
func TestAnEmptyServiceIsOfferedAndCreated(t *testing.T) {
	var buf bytes.Buffer
	if err := canvasAdd("shop").Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Empty Service") ||
		!strings.Contains(buf.String(), "/apps/new?source=empty&amp;project=shop") {
		t.Error("the canvas's Add has no Empty Service into the project")
	}

	apps := newFakeApps()
	h := buildingServer(t, apps)
	if body := do(h, signedIn(http.MethodGet, "/apps/new")).Body.String(); !strings.Contains(body, "Empty service") {
		t.Error("the deploy page does not offer an empty service")
	}
	body := do(h, signedIn(http.MethodGet, "/apps/new?source=empty")).Body.String()
	for _, want := range []string{`name="source" value="empty"`, `name="name"`, "datahuru up"} {
		if !strings.Contains(body, want) {
			t.Errorf("the empty service's form is missing %q", want)
		}
	}
	if strings.Contains(body, `name="image"`) || strings.Contains(body, `name="repo_url"`) {
		t.Error("the empty service's form asks for a source")
	}
	// An install that cannot build has nothing to build an upload with.
	if body := do(testServer(t, Options{Apps: newFakeApps()}), signedIn(http.MethodGet, "/apps/new")).Body.String(); strings.Contains(body, "Empty service") {
		t.Error("an install that cannot build offers an empty service")
	}

	form := url.Values{"source": {"empty"}, "name": {"api"}, "port": {"3000"}}
	if rec := do(h, signedInForm(http.MethodPost, "/apps", form.Encode())); rec.Code != http.StatusSeeOther {
		t.Fatalf("create = %d, want a redirect", rec.Code)
	}
	if len(apps.created) != 1 || !apps.created[0].Empty || apps.created[0].Port != 3000 {
		t.Fatalf("created %+v, want api, empty, on 3000", apps.created)
	}
}

// Until something is given to it, an empty service says how to give it
// something, rather than "building…" or a redeploy of nothing.
func TestAnEmptyServiceSaysHowToDeployIt(t *testing.T) {
	a := app.App{Name: "api", Source: app.SourceGit, Image: app.PendingImage}
	var buf bytes.Buffer
	if err := AppDeploymentsBody(AppDetailData{App: a, CLI: "datahuru"}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "datahuru up --app api") || strings.Contains(buf.String(), "Redeploy") {
		t.Errorf("deployments tab:\n%s", buf.String())
	}
	if label, _ := cardStatus(a); label != "No deploys" {
		t.Errorf("card status = %q, want No deploys", label)
	}
	if got := appImageLabel(a); got != "no source yet" {
		t.Errorf("image label = %q", got)
	}

	// A repository being built for the first time is not waiting for a source.
	a.Repo = app.Repo{URL: "https://github.com/acme/api.git"}
	if label, _ := cardStatus(a); label == "No deploys" || appImageLabel(a) != "building…" {
		t.Errorf("a first build reads as %q / %q", label, appImageLabel(a))
	}
}
