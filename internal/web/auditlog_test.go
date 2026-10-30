package web

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/audit"
)

// fakeAudit keeps what the trail records, without a database.
type fakeAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (f *fakeAudit) Record(_ context.Context, _ string, _ audit.Actor, ev audit.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeAudit) List(context.Context, string, audit.Filter) ([]audit.Entry, error) {
	return nil, nil
}

func (f *fakeAudit) Actors(context.Context, string) ([]string, error) { return nil, nil }

func (f *fakeAudit) only(t *testing.T, action string) audit.Event {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []audit.Event
	for _, ev := range f.events {
		if ev.Action == action {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s recorded %d times: %+v", action, len(found), f.events)
	}
	return found[0]
}

// A database is named by the server, not the form; the log names it all
// the same, and says it is a database, so a search for it finds its
// creation as well as its deletion.
func TestAnAddedDatabaseIsRecordedByName(t *testing.T) {
	log := &fakeAudit{}
	h := testServer(t, Options{Apps: newDatabaseApps(), Audit: log})
	if rec := post(t, h, "/apps/new/database", url.Values{"engine": {"mongo"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deploy = %d", rec.Code)
	}
	ev := log.only(t, "app.create")
	if ev.Target != "mongo" || ev.TargetKind != "app" || ev.Detail["kind"] != "database" || ev.Detail["engine"] != "mongo" {
		t.Errorf("app.create = %+v, want app mongo, a database", ev)
	}
}

// A one-click GitHub deploy is named by the app it made.
func TestAGitHubDeployIsRecordedByName(t *testing.T) {
	log := &fakeAudit{}
	gh := &fakeGitHubSurface{conns: []app.GitHubConnection{{InstallationID: 7, Account: "acme"}}}
	rt := newRoleTeamWith(t, "web-audit-gh", "web-audit-gh-app", nil, func(o *Options) { o.GitHub, o.Audit = gh, log })
	if rec := rt.postFormAs(t, "/apps/github", rt.member, url.Values{"github_repo": {"7:502"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deploy = %d", rec.Code)
	}
	ev := log.only(t, "app.create")
	if ev.Target != "web" || ev.Detail["kind"] != "github" {
		t.Errorf("app.create = %+v, want app web from GitHub", ev)
	}
}

// Sleep now stops a running service, and Wake now starts one: who did
// either, and when, is the log's to say.
func TestSleepNowAndWakeNowAreRecorded(t *testing.T) {
	log := &fakeAudit{}
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "api")), Sleep: &fakeSleeper{}, Audit: log})
	if rec := post(t, h, "/apps/api/sleep", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("Sleep now = %d", rec.Code)
	}
	if rec := post(t, h, "/apps/api/wake", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("Wake now = %d", rec.Code)
	}
	if ev := log.only(t, "app.sleep"); ev.Target != "api" || ev.Detail["how"] != "now" {
		t.Errorf("app.sleep = %+v", ev)
	}
	if ev := log.only(t, "app.wake"); ev.Target != "api" || ev.Detail["how"] != "now" {
		t.Errorf("app.wake = %+v", ev)
	}
}
