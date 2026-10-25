package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
)

// projectAdminApps is the fake service with a project's rename and delete.
type projectAdminApps struct {
	*fakeApps
	renamed, deleted []string
}

func (p *projectAdminApps) RenameProject(_ context.Context, _, slug, name string) (app.Project, error) {
	p.renamed = append(p.renamed, slug+"="+name)
	return app.Project{Slug: slug, Name: name}, nil
}

func (p *projectAdminApps) DeleteProject(_ context.Context, _, slug string) error {
	p.deleted = append(p.deleted, slug)
	return nil
}

// A project's settings rename it, and delete it only once its name has been
// typed back; the default project offers no delete at all.
func TestAProjectIsRenamedAndDeletedFromItsSettings(t *testing.T) {
	apps := &projectAdminApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}
	apps.projects = []app.Project{dominionProject()}
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/projects/dominion/settings").Body.String()
	for _, want := range []string{"Project Settings", `action="/projects/dominion/rename"`, `action="/projects/dominion/delete"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page does not have %q", want)
		}
	}
	if body := get(t, h, "/projects/default/settings").Body.String(); strings.Contains(body, `action="/projects/default/delete"`) {
		t.Error("the default project offers to be deleted")
	}
	if rec := get(t, h, "/projects/nowhere/settings"); rec.Code != http.StatusNotFound {
		t.Errorf("a project that does not exist = %d", rec.Code)
	}

	post(t, h, "/projects/dominion/rename", url.Values{"name": {"Dominion"}})
	if len(apps.renamed) != 1 || apps.renamed[0] != "dominion=Dominion" {
		t.Errorf("renamed = %v", apps.renamed)
	}

	if rec := post(t, h, "/projects/dominion/delete", url.Values{"confirm": {"wrong"}}); rec.Code != http.StatusUnprocessableEntity || len(apps.deleted) != 0 {
		t.Errorf("a wrong confirmation = %d, deleted %v", rec.Code, apps.deleted)
	}
	if rec := post(t, h, "/projects/dominion/delete", url.Values{"confirm": {"dominion"}}); rec.Code != http.StatusSeeOther || len(apps.deleted) != 1 {
		t.Errorf("deleting = %d, deleted %v", rec.Code, apps.deleted)
	}
}
