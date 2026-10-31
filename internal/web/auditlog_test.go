package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

// Making an environment and deleting one are recorded against the project,
// by the environment's name and what it copied: deleting one deletes every
// service in it (OBSERVE-13).
func TestEnvironmentsAreRecorded(t *testing.T) {
	log := &fakeAudit{}
	apps := &envApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}
	h := testServer(t, Options{Apps: apps, Audit: log})

	if rec := post(t, h, "/projects/default/environments", url.Values{"name": {"staging"}, "from": {"production"}}); rec.Code != http.StatusOK {
		t.Fatalf("create = %d", rec.Code)
	}
	ev := log.only(t, "environment.create")
	if ev.Target != "default" || ev.TargetKind != "project" || ev.Detail["name"] != "staging" || ev.Detail["from"] != "production" {
		t.Errorf("environment.create = %+v, want project default, staging from production", ev)
	}
	if rec := post(t, h, "/projects/default/environments/staging/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d", rec.Code)
	}
	if ev := log.only(t, "environment.delete"); ev.Target != "default" || ev.Detail["name"] != "staging" {
		t.Errorf("environment.delete = %+v, want project default, staging", ev)
	}
}

// A bucket is named by the server, as a database is; its uploads are
// recorded by the files they stored, an upload that stored nothing not at
// all, and a deletion by the file's key (OBSERVE-13).
func TestABucketAndItsFilesAreRecorded(t *testing.T) {
	log := &fakeAudit{}
	apps, _ := newBucketApps(t)
	h := testServer(t, Options{Apps: apps, Audit: log})

	if rec := post(t, h, "/apps/new/bucket", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("Add › Bucket = %d", rec.Code)
	}
	if ev := log.only(t, "app.create"); ev.Target != "bucket" || ev.TargetKind != "app" || ev.Detail["kind"] != "bucket" {
		t.Errorf("app.create = %+v, want app bucket, a bucket", ev)
	}

	upload := func(prefix, name string) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("prefix", prefix)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = io.WriteString(fw, "meow")
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/apps/media/files", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", "http://example.com")
		do(h, req)
	}
	upload("photos/", "cat.jpg")
	upload("", "huge.bin") // refused by the store: nothing was uploaded
	ev := log.only(t, "bucket.file.upload")
	if ev.Target != "media" || ev.Detail["files"] != "cat.jpg" || ev.Detail["count"] != "1" || ev.Detail["folder"] != "photos/" {
		t.Errorf("bucket.file.upload = %+v, want media, cat.jpg in photos/", ev)
	}

	post(t, h, "/apps/media/files/delete", url.Values{"key": {"photos/cat.jpg"}})
	post(t, h, "/apps/media/files/delete", url.Values{"key": {"../nope"}}) // refused
	if ev := log.only(t, "bucket.file.delete"); ev.Target != "media" || ev.Detail["key"] != "photos/cat.jpg" {
		t.Errorf("bucket.file.delete = %+v, want media, photos/cat.jpg", ev)
	}
}

// Checking a domain is recorded, as adding and removing one are.
func TestADomainCheckIsRecorded(t *testing.T) {
	log := &fakeAudit{}
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web")), Nets: &fakeNets{}, Audit: log})
	id := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	if rec := post(t, h, "/apps/web/domains/"+id+"/verify", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("verify = %d", rec.Code)
	}
	if ev := log.only(t, "domain.verify"); ev.Target != "web" || ev.Detail["domain"] != id {
		t.Errorf("domain.verify = %+v", ev)
	}
}
