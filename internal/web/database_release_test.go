package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/dbrelease"
)

// releaseApps is databaseApps that says where the database's version stands,
// and takes the update it offers.
type releaseApps struct {
	*databaseApps
	rel     app.DatabaseRelease
	updated []string
	err     error
}

func (r *releaseApps) DatabaseRelease(context.Context, string, string) (app.DatabaseRelease, error) {
	return r.rel, nil
}

func (r *releaseApps) UpdateDatabase(_ context.Context, _, name, _ string) (app.DatabaseUpdate, error) {
	if r.err != nil {
		return app.DatabaseUpdate{}, r.err
	}
	if r.rel.Update == nil {
		return app.DatabaseUpdate{}, app.ErrNoDatabaseUpdate
	}
	r.updated = append(r.updated, name)
	return *r.rel.Update, nil
}

func behind(fixes ...dbrelease.Advisory) app.DatabaseRelease {
	return app.DatabaseRelease{
		Engine: "PostgreSQL", Version: "16.4", Known: true,
		Report: dbrelease.Report{
			Running: dbrelease.Version{Major: "16", Minor: 4}, Latest: dbrelease.Version{Major: "16", Minor: 15},
			Supported: true, Fixes: fixes,
		},
	}
}

func pgUpdate(image string) *app.DatabaseUpdate {
	return &app.DatabaseUpdate{Version: "16.15", Label: "PostgreSQL 16.15", Image: image, Backup: true}
}

const updateAction = `action="/apps/db/database/update"`

func TestADatabaseSaysItsVersionAndWhatItIsWithout(t *testing.T) {
	high := dbrelease.Advisory{
		ID: "CVE-2026-19385", URL: "https://www.postgresql.org/support/security/CVE-2026-19385/",
		Summary: "PostgreSQL pg_dump heap buffer overflow executes arbitrary code", Component: "client",
		Score: 8.8, Fixed: dbrelease.Version{Major: "16", Minor: 15},
	}
	medium := dbrelease.Advisory{ID: "CVE-2026-18024", Component: "core server", Score: 4.3,
		Fixed: dbrelease.Version{Major: "16", Minor: 15}}

	newer := behind(high, medium)
	newer.Update = pgUpdate("postgres:16-alpine")
	pinned := behind(medium)
	pinned.Update = pgUpdate("postgres:16.15-alpine")
	waiting := behind(medium)
	current := app.DatabaseRelease{Engine: "PostgreSQL", Version: "16.15", Known: true, Report: dbrelease.Report{
		Running: dbrelease.Version{Major: "16", Minor: 15}, Latest: dbrelease.Version{Major: "16", Minor: 15}, Supported: true}}
	nextMajor := current
	nextMajor.NextMajor = "17"
	quiet := app.DatabaseRelease{Engine: "PostgreSQL", Version: "16.15", Known: true, Report: dbrelease.Report{
		Running: dbrelease.Version{Major: "16", Minor: 15}, Latest: dbrelease.Version{Major: "16", Minor: 16}, Supported: true},
		Update: &app.DatabaseUpdate{Version: "16.16", Label: "PostgreSQL 16.16", Image: "postgres:16-alpine", Backup: true}}
	rebuilt := current
	rebuilt.Update = &app.DatabaseUpdate{Version: "16.15", Label: "a newer build of PostgreSQL 16.15", Image: "postgres:16-alpine", Backup: true}
	ended := app.DatabaseRelease{Engine: "PostgreSQL", Version: "12.22", Known: true, Report: dbrelease.Report{
		Running: dbrelease.Version{Major: "12", Minor: 22}, Latest: dbrelease.Version{Major: "12", Minor: 22},
		EOL: time.Date(2024, 11, 21, 0, 0, 0, 0, time.UTC)}}
	redis := app.DatabaseRelease{Engine: "Redis", Version: "7.4.1", Series: "7", Newest: "7.4.2", NextMajor: "8",
		Update: &app.DatabaseUpdate{Version: "7.4.2", Label: "Redis 7.4.2", Image: "redis:7.4.2-alpine"}}
	redisCurrent := app.DatabaseRelease{Engine: "Redis", Version: "7.4.2", Series: "7", Newest: "7.4.2"}

	cases := []struct {
		name      string
		rel       app.DatabaseRelease
		want, not []string
	}{
		{"a newer image", newer, []string{
			"PostgreSQL 16.4 is missing 2 security fixes", "callout-err", "16.15 fixes them",
			"Update takes it", "the newer postgres:16-alpine image deploys", updateAction,
			"CVE-2026-19385", "high 8.8", "fixed in 16.15",
		}, []string{"data-database-update"}},
		{"a pinned tag", pinned, []string{
			"is missing 1 security fix", "callout-warn",
			"the image changes from postgres:16-alpine to postgres:16.15-alpine", updateAction,
		}, []string{"/settings#source"}},
		{"an image not yet published", waiting, []string{"does not carry it yet"}, []string{updateAction}},
		{"the newest release", current, []string{"PostgreSQL 16.15", "the newest 16 release"},
			[]string{"callout", updateAction, "is out."}},
		{"an update with nothing to fix", quiet, []string{
			"PostgreSQL 16.16 is available", updateAction, "data-database-update",
			`data-confirm-title="Update to PostgreSQL 16.16?"`,
			"A backup is taken first", "unavailable for a few seconds", "the current version is put back",
		}, []string{"callout", "is out, with no security fixes"}},
		{"a rebuilt image", rebuilt, []string{"A newer build of PostgreSQL 16.15 is available", updateAction}, nil},
		{"a new major version", nextMajor, []string{
			"PostgreSQL 17 is out.", "needs a data migration", "create a new PostgreSQL 17 service",
			"pg_dump and pg_restore",
		}, []string{updateAction}},
		{"an ended major version", ended, []string{"PostgreSQL 12 no longer gets security fixes", "21 November 2024"},
			[]string{updateAction}},
		{"a version not learned", app.DatabaseRelease{Engine: "PostgreSQL"}, nil, []string{"PostgreSQL"}},
		{"another engine", redis, []string{
			"Redis 7.4.1", "Redis 7.4.2 is available", updateAction, "No backup is taken first",
			"the image changes from postgres:16-alpine to redis:7.4.2-alpine",
			"Redis 8 is out.", "copy the data across",
		}, []string{"pg_dump"}},
		{"another engine up to date", redisCurrent, []string{"Redis 7.4.2", "the newest 7 release"}, []string{updateAction}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			apps := &releaseApps{databaseApps: newDatabaseApps(), rel: c.rel}
			apps.byOwner["owner-1"][0].Image = "postgres:16-alpine"
			h := testServer(t, Options{Apps: apps})
			res := get(t, h, "/apps/db/database/release")
			if res.Code != http.StatusOK {
				t.Fatalf("status %d", res.Code)
			}
			body := res.Body.String()
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q in\n%s", w, body)
				}
			}
			for _, n := range c.not {
				if strings.Contains(body, n) {
					t.Errorf("has %q in\n%s", n, body)
				}
			}
		})
	}
}

// Update asks the service for the update, says what it did, and is in the
// audit log with what it updated to.
func TestUpdateTakesTheUpdateAndRecordsIt(t *testing.T) {
	rel := behind()
	rel.Update = pgUpdate("postgres:16.15-alpine")
	apps := &releaseApps{databaseApps: newDatabaseApps(), rel: rel}
	log := &fakeAudit{}
	h := testServer(t, Options{Apps: apps, Audit: log})

	rec := post(t, h, "/apps/db/database/update", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/db" {
		t.Fatalf("update = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(apps.updated) != 1 || apps.updated[0] != "db" {
		t.Fatalf("updated %v, want db once", apps.updated)
	}
	ev := log.only(t, "deploy.update")
	if ev.Target != "db" || ev.Detail["to"] != "PostgreSQL 16.15" || ev.Detail["image"] != "postgres:16.15-alpine" {
		t.Errorf("deploy.update = %+v", ev)
	}
}

// Nothing to update is said, and is not recorded as an update.
func TestUpdateWithNothingNewerIsRefused(t *testing.T) {
	apps := &releaseApps{databaseApps: newDatabaseApps(), rel: behind()}
	log := &fakeAudit{}
	h := testServer(t, Options{Apps: apps, Audit: log})
	if rec := post(t, h, "/apps/db/database/update", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("update = %d", rec.Code)
	}
	if len(apps.updated) != 0 {
		t.Fatalf("updated %v with nothing to update to", apps.updated)
	}
	for _, ev := range log.events {
		if ev.Action == "deploy.update" {
			t.Fatalf("a refused update was recorded: %+v", ev)
		}
	}
}

// A deployment the button made is called what it did.
func TestAnUpdateDeploymentIsTitledByItsVersion(t *testing.T) {
	dep := app.Deployment{Revision: app.DatabaseUpdateTriggerPrefix + "PostgreSQL 16.16"}
	if got := deploymentTitle(dep); got != "Update to PostgreSQL 16.16" {
		t.Errorf("title = %q", got)
	}
}

func TestWithoutReleasesThereIsNoVersionRoute(t *testing.T) {
	h := testServer(t, Options{Apps: newDatabaseApps()})
	if res := get(t, h, "/apps/db/database/release"); res.Code == http.StatusOK {
		t.Fatalf("a server whose apps cannot say a version answered %d", res.Code)
	}
	if res := post(t, h, "/apps/db/database/update", url.Values{}); res.Code == http.StatusSeeOther {
		t.Fatalf("a server whose apps cannot update a database answered %d", res.Code)
	}
}
