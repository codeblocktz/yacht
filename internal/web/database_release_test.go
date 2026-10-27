package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/dbrelease"
)

// releaseApps is databaseApps that says where the database's version stands.
type releaseApps struct {
	*databaseApps
	rel app.DatabaseRelease
}

func (r *releaseApps) DatabaseRelease(context.Context, string, string) (app.DatabaseRelease, error) {
	return r.rel, nil
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

func TestADatabaseSaysItsVersionAndWhatItIsWithout(t *testing.T) {
	high := dbrelease.Advisory{
		ID: "CVE-2026-19385", URL: "https://www.postgresql.org/support/security/CVE-2026-19385/",
		Summary: "PostgreSQL pg_dump heap buffer overflow executes arbitrary code", Component: "client",
		Score: 8.8, Fixed: dbrelease.Version{Major: "16", Minor: 15},
	}
	medium := dbrelease.Advisory{ID: "CVE-2026-18024", Component: "core server", Score: 4.3,
		Fixed: dbrelease.Version{Major: "16", Minor: 15}}

	newer := behind(high, medium)
	newer.NewerImage = true
	pinned := behind(medium)
	pinned.Pinned, pinned.Suggested = true, "postgres:16.15-alpine"
	waiting := behind(medium)
	current := app.DatabaseRelease{Engine: "PostgreSQL", Version: "16.15", Known: true, Report: dbrelease.Report{
		Running: dbrelease.Version{Major: "16", Minor: 15}, Latest: dbrelease.Version{Major: "16", Minor: 15}, Supported: true}}
	ended := app.DatabaseRelease{Engine: "PostgreSQL", Version: "12.22", Known: true, Report: dbrelease.Report{
		Running: dbrelease.Version{Major: "12", Minor: 22}, Latest: dbrelease.Version{Major: "12", Minor: 22},
		EOL: time.Date(2024, 11, 21, 0, 0, 0, 0, time.UTC)}}

	cases := []struct {
		name      string
		rel       app.DatabaseRelease
		want, not []string
	}{
		{"a newer image", newer, []string{
			"PostgreSQL 16.4 is missing 2 security fixes", "callout-err", "16.15 fixes them",
			"<code class=\"mono\">16-alpine</code> image has been updated", `action="/apps/db/redeploy"`,
			"CVE-2026-19385", "high 8.8", "fixed in 16.15",
		}, nil},
		{"a pinned tag", pinned, []string{
			"is missing 1 security fix", "callout-warn", "postgres:16.15-alpine", `href="/apps/db/settings#source"`,
		}, []string{`action="/apps/db/redeploy"`}},
		{"an image not yet published", waiting, []string{"does not carry it yet"}, []string{`action="/apps/db/redeploy"`}},
		{"the newest release", current, []string{"PostgreSQL 16.15", "the newest 16 release"}, []string{"callout"}},
		{"an ended major version", ended, []string{"PostgreSQL 12 no longer gets security fixes", "21 November 2024"}, nil},
		{"a version not learned", app.DatabaseRelease{Engine: "PostgreSQL"}, nil, []string{"PostgreSQL"}},
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

func TestWithoutReleasesThereIsNoVersionRoute(t *testing.T) {
	h := testServer(t, Options{Apps: newDatabaseApps()})
	if res := get(t, h, "/apps/db/database/release"); res.Code == http.StatusOK {
		t.Fatalf("a server whose apps cannot say a version answered %d", res.Code)
	}
}
