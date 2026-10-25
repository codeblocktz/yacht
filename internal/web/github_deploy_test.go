package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
)

func dominionProject() app.Project {
	return app.Project{ID: uuid.MustParse("6f1f3a52-7a4c-4b6e-9a51-2f0d2b7f8a10"), Slug: "dominion", Name: "dominion"}
}

// A deploy started from a project's canvas lands in that project, through
// the picker and the form.
func TestADeployFromAProjectLandsInIt(t *testing.T) {
	apps := newFakeApps()
	apps.projects = []app.Project{dominionProject()}
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps/new?project=dominion").Body.String()
	for _, want := range []string{"Into dominion.", `href="/apps/new?source=image&amp;project=dominion"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the picker does not show %q", want)
		}
	}
	if body := get(t, h, "/apps/new?source=image&project=dominion").Body.String(); !strings.Contains(body, `name="project" value="dominion"`) {
		t.Error("the form does not carry the project")
	}

	rec := post(t, h, "/apps", url.Values{
		"name": {"web"}, "image": {"nginx:alpine"}, "port": {"8080"}, "replicas": {"1"}, "project": {"dominion"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if got := apps.created[0].ProjectID; got != dominionProject().ID {
		t.Errorf("created in project %s, want dominion's", got)
	}

	// No project, or the default one, is the default project.
	post(t, h, "/apps", url.Values{"name": {"api"}, "image": {"nginx:alpine"}, "port": {"8080"}, "replicas": {"1"}})
	if got := apps.created[1].ProjectID; got != uuid.Nil {
		t.Errorf("created without a project in %s, want the default", got)
	}
	// A project that is gone is said, not silently replaced.
	if rec := post(t, h, "/apps", url.Values{
		"name": {"old"}, "image": {"nginx:alpine"}, "port": {"8080"}, "replicas": {"1"}, "project": {"deleted"},
	}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no longer exists") {
		t.Errorf("into a deleted project = %d", rec.Code)
	}
}

// githubDeployTeam is a live team with GitHub connected, and a project of
// its own besides the default.
func githubDeployTeam(t *testing.T, teamID string) (*roleTeam, *fakeGitHubSurface, app.Project) {
	t.Helper()
	rt, gh := githubTeam(t, teamID)
	gh.conns = []app.GitHubConnection{{InstallationID: 7, Account: "acme"}}
	p, err := rt.apps.CreateProject(t.Context(), teamID, "dominion")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return rt, gh, p
}

// The one-click page lists the team's repositories; a click deploys one
// into the project and goes to the app, building. The picker offers it
// first.
func TestOneClickDeploysARepository(t *testing.T) {
	rt, gh, project := githubDeployTeam(t, "web-gh-oneclick")

	picker := rt.getAs(t, "/apps/new?project=dominion", rt.member).Body.String()
	if !strings.Contains(picker, `href="/apps/new/github?project=dominion"`) || !strings.Contains(picker, "GitHub repository") {
		t.Error("the picker does not offer the one-click deploy into the project")
	}
	body := rt.getAs(t, "/apps/new/github?project=dominion", rt.member).Body.String()
	for _, want := range []string{"acme/web", "acme/api", "Into dominion.", `name="github_repo" value="7:501"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
	rows := rt.getAs(t, "/apps/new/github/repositories?q=API&project=dominion", rt.member).Body.String()
	if !strings.Contains(rows, "acme/api") || strings.Contains(rows, "acme/web") {
		t.Errorf("searching for API shows: %s", rows)
	}

	rec := rt.postFormAs(t, "/apps/github", rt.member, url.Values{"github_repo": {"7:502"}, "project": {"dominion"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/web" {
		t.Fatalf("deploy = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(gh.deploys) != 1 || gh.deploys[0].ref != (app.GitHubRef{InstallationID: 7, RepoID: 502}) ||
		gh.deploys[0].project != project.ID || gh.deploys[0].owner != rt.teamID {
		t.Errorf("deploys = %+v", gh.deploys)
	}

	gh.deployErr = errors.New("app: a build needs an image registry")
	if rec := rt.postFormAs(t, "/apps/github", rt.member, url.Values{"github_repo": {"7:502"}}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Could not deploy") {
		t.Errorf("a refused deploy = %d", rec.Code)
	}
	if rec := rt.postFormAs(t, "/apps/github", rt.member, url.Values{"github_repo": {"nonsense"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("no repository = %d", rec.Code)
	}
}

// With no GitHub connection the page says so, and where to make one.
func TestOneClickWithoutGitHubSaysWhere(t *testing.T) {
	rt, _ := githubTeam(t, "web-gh-oneclick-none")
	body := rt.getAs(t, "/apps/new/github", rt.member).Body.String()
	if !strings.Contains(body, "GitHub is not connected") || !strings.Contains(body, `href="/team"`) {
		t.Errorf("the page does not say GitHub is not connected: %s", body)
	}
}

// An app is moved to another project from its settings; with only one
// project there is nowhere to move it, and no section.
func TestAnAppMovesToAnotherProject(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps})
	if body := get(t, h, "/apps/web/settings").Body.String(); strings.Contains(body, `action="/apps/web/project"`) {
		t.Error("a team with one project is offered a move")
	}

	apps.projects = []app.Project{dominionProject()}
	body := get(t, h, "/apps/web/settings").Body.String()
	if !strings.Contains(body, `action="/apps/web/project"`) || !strings.Contains(body, `value="dominion"`) {
		t.Fatal("the settings do not offer the move")
	}
	if nested := nestedForms(t, body); len(nested) > 0 {
		t.Errorf("forms inside forms: %v", nested)
	}
	rec := post(t, h, "/apps/web/project", url.Values{"project": {"dominion"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("move = %d", rec.Code)
	}
	if got := apps.byOwner["owner-1"][0].ProjectID; got != dominionProject().ID {
		t.Errorf("project = %s, want dominion's", got)
	}
	if rec := post(t, h, "/apps/nope/project", url.Values{"project": {"dominion"}}); rec.Code != http.StatusNotFound {
		t.Errorf("moving an app that does not exist = %d", rec.Code)
	}
}
