package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
)

type fakeHooks struct {
	hook      app.Hook
	secret    string
	delivered []app.Push
	deliverTo []uuid.UUID
	result    string
	err       error
	enabled   int
	disabled  int
}

func (f *fakeHooks) GitHook(context.Context, string, string) (app.Hook, error) {
	return f.hook, nil
}

func (f *fakeHooks) EnableGitHook(context.Context, string, string) (string, error) {
	f.enabled++
	f.hook.Enabled = true
	return f.secret, nil
}

func (f *fakeHooks) DisableGitHook(context.Context, string, string) error {
	f.disabled++
	return nil
}

func (f *fakeHooks) DeliverPush(_ context.Context, id uuid.UUID, p app.Push) (string, error) {
	f.deliverTo = append(f.deliverTo, id)
	f.delivered = append(f.delivered, p)
	return f.result, f.err
}

func gitApp(owner, name string) app.App {
	a := sampleApp(owner, name)
	a.Source = app.SourceGit
	a.Repo = app.Repo{URL: "https://github.com/example/api", Branch: "main"}
	return a
}

// A delivery is a server calling a server: no Origin, no cookie. It has to get
// past csrfProtect to the engine, with the headers each host signs in.
func TestADeliveryReachesTheEngineWithoutAnOrigin(t *testing.T) {
	hooks := &fakeHooks{result: "Deploying 0123456."}
	h := testServer(t, Options{Apps: newFakeApps(gitApp("owner-1", "api")), Hooks: hooks})
	id := uuid.New()

	req := httptest.NewRequest(http.MethodPost, "/hooks/apps/"+id.String()+"/push",
		strings.NewReader(`{"ref":"refs/heads/main"}`))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("delivery = %d %q, want 202", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Deploying 0123456.") {
		t.Errorf("body = %q, want what the engine said, for the host to show", rec.Body.String())
	}
	if len(hooks.delivered) != 1 || hooks.deliverTo[0] != id {
		t.Fatalf("delivered = %v to %v, want one to %s", hooks.delivered, hooks.deliverTo, id)
	}
	p := hooks.delivered[0]
	if p.Event != "push" || p.Signature != "sha256=abc" || string(p.Body) != `{"ref":"refs/heads/main"}` {
		t.Errorf("push = %+v, want the event, signature and body passed through", p)
	}
}

func TestAGitLabDeliveryPassesItsToken(t *testing.T) {
	hooks := &fakeHooks{result: "ok"}
	h := testServer(t, Options{Hooks: hooks})

	req := httptest.NewRequest(http.MethodPost, "/hooks/apps/"+uuid.NewString()+"/push", strings.NewReader(`{}`))
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", "tok")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(hooks.delivered) != 1 || hooks.delivered[0].Token != "tok" || hooks.delivered[0].Event != "Push Hook" {
		t.Fatalf("delivered = %+v, want GitLab's event and token", hooks.delivered)
	}
}

// A refused delivery says so with a status the host shows as a failure.
func TestARefusedDeliveryIsUnauthorized(t *testing.T) {
	hooks := &fakeHooks{err: app.ErrHookUnauthorized}
	h := testServer(t, Options{Hooks: hooks})

	req := httptest.NewRequest(http.MethodPost, "/hooks/apps/"+uuid.NewString()+"/push", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refused delivery = %d, want 401", rec.Code)
	}

	bad := httptest.NewRequest(http.MethodPost, "/hooks/apps/not-an-id/push", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bad)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id = %d, want 404", rec.Code)
	}
	if len(hooks.delivered) != 1 {
		t.Fatalf("a malformed id reached the engine: %d deliveries", len(hooks.delivered))
	}

	hooks.err = errors.New("database unavailable")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/apps/"+uuid.NewString()+"/push", strings.NewReader(`{}`)))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database") {
		t.Errorf("internal failure = %d %q, want 500 without the cause", rec.Code, rec.Body.String())
	}
}

// The exemption is the prefix and nothing else: an origin-less post anywhere
// outside it is still refused.
func TestTheWebhookExemptionIsOnlyThePrefix(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(gitApp("owner-1", "api")), Hooks: &fakeHooks{}})
	for _, path := range []string{"/apps/api/hook", "/apps/api/redeploy", "/hooksx/apps/1/push"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("origin-less POST %s = %d, want 403", path, rec.Code)
		}
	}
}

// Without the surface there is no endpoint to call.
func TestNoHooksMountsNoEndpoint(t *testing.T) {
	h := testServer(t, Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/apps/"+uuid.NewString()+"/push", nil))
	if rec.Code == http.StatusAccepted {
		t.Fatal("a delivery was accepted with no hooks surface")
	}
}

// The secret is shown on the response that issued it, and on no other.
func TestTheSecretIsShownOnceAndNotReadBack(t *testing.T) {
	const secret = "5ec2e7-5ec2e7-5ec2e7-5ec2e7"
	hooks := &fakeHooks{secret: secret}
	h := testServer(t, Options{Apps: newFakeApps(gitApp("owner-1", "api")), Hooks: hooks})

	before := get(t, h, "/apps/api/settings").Body.String()
	if !strings.Contains(before, "Deploy on push") || !strings.Contains(before, "Turn on") {
		t.Fatal("a Git app's settings do not offer deploy on push")
	}

	rec := post(t, h, "/apps/api/hook", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST hook = %d, want the page rendered with the secret", rec.Code)
	}
	body := rec.Body.String()
	if hooks.enabled != 1 || !strings.Contains(body, secret) {
		t.Fatal("the issued secret is not on the page that issued it")
	}
	if !strings.Contains(body, "/hooks/apps/") {
		t.Error("the page does not give the webhook URL")
	}

	after := get(t, h, "/apps/api/settings").Body.String()
	if strings.Contains(after, secret) {
		t.Fatal("the secret was shown again on a later visit")
	}
	if !strings.Contains(after, "Issue a new one to see it again") {
		t.Error("a later visit does not say how to get a secret again")
	}
}

func TestAnImageAppIsNotOfferedDeployOnPush(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web")), Hooks: &fakeHooks{}})
	if strings.Contains(get(t, h, "/apps/web/settings").Body.String(), "Deploy on push") {
		t.Fatal("an app with no repository is offered deploy on push")
	}
}

func TestTurningTheHookOffReachesTheEngine(t *testing.T) {
	hooks := &fakeHooks{hook: app.Hook{Enabled: true}}
	h := testServer(t, Options{Apps: newFakeApps(gitApp("owner-1", "api")), Hooks: hooks})
	if code := post(t, h, "/apps/api/hook/delete", nil).Code; code != http.StatusSeeOther {
		t.Fatalf("POST hook/delete = %d, want 303", code)
	}
	if hooks.disabled != 1 {
		t.Fatal("turning the hook off did not reach the engine")
	}
}
