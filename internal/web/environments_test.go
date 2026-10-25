package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
)

// envApps is the fake service with environments: production and staging,
// and staging's own copy of web.
type envApps struct {
	*fakeApps
	created, deleted []string
}

func (e *envApps) Environments(context.Context, string, string) ([]app.Environment, error) {
	return []app.Environment{
		{Slug: "production", Name: "production", Production: true, Apps: 1},
		{Slug: "staging", Name: "staging", CopiedFrom: "production", Apps: 1},
	}, nil
}

func (e *envApps) HasEnvironment(_ context.Context, _ string, _ app.Project, env string) (bool, error) {
	return env == "production" || env == "staging", nil
}

func (e *envApps) ListInEnvironment(_ context.Context, ownerID string, _ app.Project, env string) ([]app.App, error) {
	if env == "staging" {
		a := sampleApp(ownerID, "web-staging")
		a.EnvSlug, a.Service = "staging", "web"
		return []app.App{a}, nil
	}
	return e.byOwner[ownerID], nil
}

func (e *envApps) CreateEnvironment(_ context.Context, _, _, name, from string) (app.Environment, []app.EnvironmentCopy, error) {
	e.created = append(e.created, name+"<"+from)
	return app.Environment{Slug: app.Slugify(name), Name: name}, []app.EnvironmentCopy{{Service: "web", App: "web-" + name}}, nil
}

func (e *envApps) DeleteEnvironment(_ context.Context, _, _, slug string) error {
	e.deleted = append(e.deleted, slug)
	return nil
}

// The canvas draws the environment asked for, under its own name, and the
// bar offers the project's others, as Railway's switcher does.
func TestTheCanvasIsOneEnvironment(t *testing.T) {
	apps := &envApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}
	h := testServer(t, Options{Apps: apps})

	staging := get(t, h, "/projects/default?environment=staging").Body.String()
	if !strings.Contains(staging, `data-node="web-staging"`) || strings.Contains(staging, `data-node="web"`) {
		t.Error("staging's canvas does not draw staging's services alone")
	}
	for _, want := range []string{`href="/projects/default?environment=production"`, "New environment"} {
		if !strings.Contains(staging, want) {
			t.Errorf("the switcher is missing %q", want)
		}
	}
	production := get(t, h, "/projects/default?environment=production").Body.String()
	if !strings.Contains(production, `data-node="web"`) {
		t.Error("production's canvas lost production's web")
	}
	// An environment the project does not have is production.
	if body := get(t, h, "/projects/default?environment=nowhere").Body.String(); !strings.Contains(body, `data-node="web"`) {
		t.Error("an unknown environment did not fall back to production")
	}
}

// Project Settings makes and deletes environments.
func TestEnvironmentsAreMadeInProjectSettings(t *testing.T) {
	apps := &envApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/projects/default/settings").Body.String()
	for _, want := range []string{`id="environments"`, "copied from production", `action="/projects/default/environments"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings are missing %q", want)
		}
	}
	rec := post(t, h, "/projects/default/environments", url.Values{"name": {"qa"}, "from": {"production"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "qa is being deployed") {
		t.Errorf("create = %d", rec.Code)
	}
	post(t, h, "/projects/default/environments", url.Values{"name": {"empty"}, "from": {"-"}})
	if len(apps.created) != 2 || apps.created[0] != "qa<production" || apps.created[1] != "empty<" {
		t.Errorf("created = %v", apps.created)
	}
	if rec := post(t, h, "/projects/default/environments/staging/delete", nil); rec.Code != http.StatusSeeOther || len(apps.deleted) != 1 {
		t.Errorf("delete = %d, %v", rec.Code, apps.deleted)
	}
}
