package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/secret"
)

// hookFixture is a Git app building main, with no deploy in flight and a
// webhook whose secret is returned.
func hookFixture(t *testing.T, name string) (*Service, string, App, string) {
	t.Helper()
	ctx := context.Background()
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, _, pool := testService(t, Options{Images: fakeImages{}, Builder: fakeBuilder{}, Keeper: keeper})
	ownerID := owner(t, s, pool, name)
	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "api", Source: SourceGit, Replicas: 1, Port: 8080,
		Repo: Repo{URL: "https://github.com/example/api", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The first build is admitted at create. Stopped, so each test starts
	// with nothing in flight.
	if err := s.CancelLiveDeployment(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("cancel first build: %v", err)
	}
	secret, err := s.EnableGitHook(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("EnableGitHook: %v", err)
	}
	return s, ownerID, a, secret
}

func signedPush(secret, event, ref, after string) Push {
	body := []byte(fmt.Sprintf(`{"ref":%q,"after":%q}`, ref, after))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return Push{Event: event, Body: body, Signature: "sha256=" + hex.EncodeToString(mac.Sum(nil))}
}

func liveTrigger(t *testing.T, s *Service, a App) string {
	t.Helper()
	op, err := s.liveOperation(context.Background(), a)
	if err != nil {
		return ""
	}
	var trigger string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trigger FROM deployments WHERE id = $1`, op.DeploymentID).Scan(&trigger); err != nil {
		t.Fatalf("read trigger: %v", err)
	}
	return trigger
}

const sha = "0123456789abcdef0123456789abcdef01234567"

// A signed push to the branch the app builds is a build, admitted like a
// person's redeploy and attributed to the webhook.
func TestAPushToTheBranchDeploys(t *testing.T) {
	ctx := context.Background()
	s, ownerID, a, secret := hookFixture(t, "hook-deploys")

	result, err := s.DeliverPush(ctx, a.ID, signedPush(secret, "push", "refs/heads/main", sha))
	if err != nil {
		t.Fatalf("DeliverPush: %v", err)
	}
	if !strings.Contains(result, "0123456") {
		t.Errorf("result = %q, want the revision", result)
	}
	if got := liveTrigger(t, s, a); got != "push:0123456" {
		t.Fatalf("live deployment trigger = %q, want push:0123456", got)
	}
	var actor string
	if err := s.pool.QueryRow(ctx,
		`SELECT actor_kind FROM deployments WHERE app_id = $1 ORDER BY started_at DESC LIMIT 1`,
		a.ID).Scan(&actor); err != nil {
		t.Fatalf("read actor: %v", err)
	}
	if actor != "webhook" {
		t.Errorf("actor = %q, want webhook", actor)
	}

	hook, err := s.GitHook(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("GitHook: %v", err)
	}
	if !hook.Enabled || hook.LastDelivery != result || hook.LastDeliveryAt.IsZero() {
		t.Errorf("hook = %+v, want the delivery recorded", hook)
	}
}

// Nothing is admitted for a delivery that is not signed with this app's secret
// — and an unknown app looks the same as a wrong signature.
func TestAnUnsignedPushIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _, a, secret := hookFixture(t, "hook-refused")

	for name, p := range map[string]Push{
		"wrong secret": signedPush("not-the-secret", "push", "refs/heads/main", sha),
		"no signature": {Event: "push", Body: []byte(`{"ref":"refs/heads/main","after":"` + sha + `"}`)},
		"wrong token":  {Event: "Push Hook", Token: "guess", Body: []byte(`{}`)},
		// Signed properly, then pointed at another branch in transit.
		"tampered body": func() Push {
			p := signedPush(secret, "push", "refs/heads/other", sha)
			p.Body = bytes.Replace(p.Body, []byte("other"), []byte("main"), 1)
			return p
		}(),
	} {
		if _, err := s.DeliverPush(ctx, a.ID, p); !errors.Is(err, ErrHookUnauthorized) {
			t.Errorf("%s: err = %v, want ErrHookUnauthorized", name, err)
		}
	}
	if got := liveTrigger(t, s, a); got != "" {
		t.Fatalf("a refused push admitted %q", got)
	}
}

// GitLab sends the secret itself rather than a signature.
func TestAGitLabTokenIsAccepted(t *testing.T) {
	ctx := context.Background()
	s, _, a, secret := hookFixture(t, "hook-gitlab")

	p := Push{Event: "Push Hook", Token: secret,
		Body: []byte(`{"ref":"refs/heads/main","after":"` + sha + `"}`)}
	if _, err := s.DeliverPush(ctx, a.ID, p); err != nil {
		t.Fatalf("DeliverPush: %v", err)
	}
	if got := liveTrigger(t, s, a); got != "push:0123456" {
		t.Fatalf("trigger = %q, want push:0123456", got)
	}
}

// What is not a push to the built branch is answered and left alone.
func TestPushesThisAppDoesNotBuildAreIgnored(t *testing.T) {
	ctx := context.Background()
	s, _, a, secret := hookFixture(t, "hook-ignored")

	for _, tc := range []struct {
		name, event, ref, after, want string
	}{
		{"ping", "ping", "", "", "connected"},
		{"another branch", "push", "refs/heads/feature", sha, "builds main"},
		{"a tag", "push", "refs/tags/v1", sha, "only branches"},
		{"a deletion", "push", "refs/heads/main", strings.Repeat("0", 40), "deletion"},
		{"another event", "issues", "", "", "only pushes"},
	} {
		result, err := s.DeliverPush(ctx, a.ID, signedPush(secret, tc.event, tc.ref, tc.after))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !strings.Contains(result, tc.want) {
			t.Errorf("%s: result = %q, want it to mention %q", tc.name, result, tc.want)
		}
	}
	if got := liveTrigger(t, s, a); got != "" {
		t.Fatalf("an ignored delivery admitted %q", got)
	}
}

// A push during a deploy is held, and built once that deploy ends — a second
// push in quick succession is not dropped.
func TestAPushDuringADeployWaitsForIt(t *testing.T) {
	ctx := context.Background()
	s, ownerID, a, secret := hookFixture(t, "hook-waits")

	if _, err := s.DeliverPush(ctx, a.ID, signedPush(secret, "push", "refs/heads/main", sha)); err != nil {
		t.Fatalf("first push: %v", err)
	}
	second := "fedcba9876543210fedcba9876543210fedcba98"
	result, err := s.DeliverPush(ctx, a.ID, signedPush(secret, "push", "refs/heads/main", second))
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if !strings.Contains(result, "Queued fedcba9") {
		t.Fatalf("result = %q, want the push queued", result)
	}
	if hook, _ := s.GitHook(ctx, ownerID, a.Name); !hook.Pending {
		t.Fatal("the second push is not waiting")
	}

	// Still in flight: the waiting push stays waiting.
	if err := s.admitPendingPushes(ctx); err != nil {
		t.Fatalf("admitPendingPushes: %v", err)
	}
	if got := liveTrigger(t, s, a); got != "push:0123456" {
		t.Fatalf("trigger = %q while the first build is live, want push:0123456", got)
	}

	if err := s.CancelLiveDeployment(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("end the first build: %v", err)
	}
	if err := s.admitPendingPushes(ctx); err != nil {
		t.Fatalf("admitPendingPushes: %v", err)
	}
	if got := liveTrigger(t, s, a); got != "push:fedcba9" {
		t.Fatalf("trigger = %q after the first build ended, want push:fedcba9", got)
	}
	if hook, _ := s.GitHook(ctx, ownerID, a.Name); hook.Pending {
		t.Fatal("the push is still waiting after it was admitted")
	}
}

// A new secret replaces the old outright, and turning the hook off refuses
// the one it had.
func TestRegeneratingAndDisablingRevokeTheSecret(t *testing.T) {
	ctx := context.Background()
	s, ownerID, a, old := hookFixture(t, "hook-revoke")

	fresh, err := s.EnableGitHook(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("EnableGitHook: %v", err)
	}
	if fresh == old {
		t.Fatal("regenerating returned the same secret")
	}
	if _, err := s.DeliverPush(ctx, a.ID, signedPush(old, "ping", "", "")); !errors.Is(err, ErrHookUnauthorized) {
		t.Errorf("the replaced secret: err = %v, want ErrHookUnauthorized", err)
	}
	if err := s.DisableGitHook(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("DisableGitHook: %v", err)
	}
	if _, err := s.DeliverPush(ctx, a.ID, signedPush(fresh, "ping", "", "")); !errors.Is(err, ErrHookUnauthorized) {
		t.Errorf("after disabling: err = %v, want ErrHookUnauthorized", err)
	}
	if hook, _ := s.GitHook(ctx, ownerID, a.Name); hook.Enabled {
		t.Error("the hook is still enabled")
	}
}

func TestAnImageAppHasNoWebhook(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "hook-image")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{Name: "web", Image: "nginx:1.27", Replicas: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.EnableGitHook(ctx, ownerID, a.Name); !errors.Is(err, ErrNotGitApp) {
		t.Fatalf("EnableGitHook = %v, want ErrNotGitApp", err)
	}
}
