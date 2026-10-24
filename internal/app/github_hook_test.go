package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// signedDelivery is a delivery to the App's webhook, signed as GitHub signs.
func signedDelivery(id, event string, body any) GitHubDelivery {
	raw, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, []byte(fakeHookSecret))
	mac.Write(raw)
	return GitHubDelivery{ID: id, Event: event, Body: raw,
		Signature: "sha256=" + hex.EncodeToString(mac.Sum(nil))}
}

func pushTo(repoID int64, fullName, branch string) map[string]any {
	return map[string]any{
		"ref": "refs/heads/" + branch, "after": sha,
		"repository":   map[string]any{"id": repoID, "full_name": fullName},
		"installation": map[string]any{"id": 7},
	}
}

func liveTriggerOf(t *testing.T, s *Service, ownerID, name string) string {
	t.Helper()
	a, err := s.Get(context.Background(), ownerID, name)
	if err != nil {
		t.Fatal(err)
	}
	return liveTrigger(t, s, a)
}

// A push reaches every app built from the repository — by its id, across
// the teams connected to the installation — whose branch it moved, and a
// redelivery of it deploys nothing again.
func TestAPushThroughTheAppDeploysEveryAppOnThatRepositoryAndBranch(t *testing.T) {
	ctx := context.Background()
	s, _ := githubFixture(t, nil)
	teamA := owner(t, s, s.pool, "gh-push-a")
	teamB := owner(t, s, s.pool, "gh-push-b")
	teamGone := owner(t, s, s.pool, "gh-push-gone")
	for _, team := range []string{teamA, teamB, teamGone} {
		connect(t, s, team, 7)
	}
	mk := func(team, name string, repo int64, branch string) {
		t.Helper()
		if _, err := s.Create(ctx, team, CreateInput{Name: name, Source: SourceGit, Replicas: 1, Port: 8080,
			Repo: Repo{Branch: branch, GitHub: GitHubRef{InstallationID: 7, RepoID: repo}}}); err != nil {
			t.Fatalf("Create %s/%s: %v", team, name, err)
		}
		if err := s.CancelLiveDeployment(ctx, team, name); err != nil {
			t.Fatal(err)
		}
	}
	mk(teamA, "web", webRepo.ID, "main")
	mk(teamA, "web-dev", webRepo.ID, "dev")
	mk(teamA, "api", otherRepo.ID, "main")
	mk(teamB, "site", webRepo.ID, "main")
	mk(teamGone, "web", webRepo.ID, "main")
	if err := s.DisconnectGitHub(ctx, teamGone, 7); err != nil {
		t.Fatal(err)
	}

	// Renamed since: routed by id all the same.
	d := signedDelivery("delivery-1", "push", pushTo(webRepo.ID, "acme/website", "main"))
	result, err := s.DeliverGitHub(ctx, d)
	if err != nil {
		t.Fatalf("DeliverGitHub: %v", err)
	}
	for _, want := range []string{"gh-push-a/web: Deploying 0123456", "gh-push-b/site: Deploying 0123456"} {
		if !strings.Contains(result, want) {
			t.Errorf("result %q does not say %q", result, want)
		}
	}
	for _, c := range []struct {
		team, app, want string
	}{
		{teamA, "web", "push:0123456"}, {teamB, "site", "push:0123456"},
		{teamA, "web-dev", ""}, {teamA, "api", ""}, {teamGone, "web", ""},
	} {
		if got := liveTriggerOf(t, s, c.team, c.app); got != c.want {
			t.Errorf("%s/%s: live trigger %q, want %q", c.team, c.app, got, c.want)
		}
	}
	if got, _ := s.Get(ctx, teamA, "web"); got.Repo.URL != "https://github.com/acme/website.git" {
		t.Errorf("a renamed repository's URL = %q, want its new name", got.Repo.URL)
	}

	// The same delivery again: answered, and nothing more admitted. The
	// first deploy is still in flight, so a second admission would show as a
	// queued push.
	again, err := s.DeliverGitHub(ctx, d)
	if err != nil || !strings.Contains(again, "Already handled") {
		t.Fatalf("replayed delivery: %q, %v", again, err)
	}
	hook, _ := s.GitHook(ctx, teamA, "web")
	if hook.Pending || hook.Enabled {
		t.Fatalf("after a replay: %+v — want nothing queued and no webhook of the app's own", hook)
	}

	// A new push while the first deploys waits in the app's push slot, which
	// an app with no webhook of its own has all the same.
	if _, err := s.DeliverGitHub(ctx, signedDelivery("delivery-2", "push",
		pushTo(webRepo.ID, "acme/website", "main"))); err != nil {
		t.Fatal(err)
	}
	if hook, _ = s.GitHook(ctx, teamA, "web"); !hook.Pending || !strings.Contains(hook.LastDelivery, "Queued") {
		t.Fatalf("a push during a deploy: %+v, want it queued", hook)
	}

	// Signed with anything but the App's secret, it is refused unread.
	forged := signedDelivery("delivery-3", "push", pushTo(webRepo.ID, "acme/web", "main"))
	forged.Signature = "sha256=" + strings.Repeat("0", 64)
	if _, err := s.DeliverGitHub(ctx, forged); !errors.Is(err, ErrHookUnauthorized) {
		t.Fatalf("forged delivery: err = %v", err)
	}
	if _, err := s.DeliverGitHub(ctx, signedDelivery("", "push", pushTo(webRepo.ID, "acme/web", "main"))); !errors.Is(err, ErrGitHubDeliveryInvalid) {
		t.Fatalf("a delivery with no id: err = %v", err)
	}
}

// GitHub taking a repository or an installation away pauses its apps —
// pushes are ignored and builds refused, with the reason — and giving it
// back resumes them. Nothing is deleted.
func TestAccessTakenAwayPausesAppsRatherThanDeletingThem(t *testing.T) {
	ctx := context.Background()
	b := &buildRecorder{}
	s, _ := githubFixture(t, b)
	team := owner(t, s, s.pool, "gh-pause")
	connect(t, s, team, 7)
	if _, err := s.Create(ctx, team, CreateInput{Name: "web", Source: SourceGit, Replicas: 1, Port: 8080,
		Repo: Repo{Branch: "main", GitHub: GitHubRef{InstallationID: 7, RepoID: webRepo.ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelLiveDeployment(ctx, team, "web"); err != nil {
		t.Fatal(err)
	}
	paused := func() string {
		t.Helper()
		a, err := s.Get(ctx, team, "web")
		if err != nil {
			t.Fatalf("the app is gone: %v", err)
		}
		return a.GitHubPaused
	}

	if _, err := s.DeliverGitHub(ctx, signedDelivery("r-1", "installation_repositories", map[string]any{
		"action": "removed", "installation": map[string]any{"id": 7},
		"repositories_removed": []map[string]any{{"id": webRepo.ID, "full_name": "acme/web"}},
	})); err != nil {
		t.Fatal(err)
	}
	if got := paused(); got != GitHubPausedRepoRemoved {
		t.Fatalf("paused = %q, want repo_removed", got)
	}
	result, err := s.DeliverGitHub(ctx, signedDelivery("r-2", "push", pushTo(webRepo.ID, "acme/web", "main")))
	if err != nil || !strings.Contains(result, "paused") || liveTriggerOf(t, s, team, "web") != "" {
		t.Fatalf("a push to a paused app: %q, %v, live %q", result, err, liveTriggerOf(t, s, team, "web"))
	}
	a, _ := s.Get(ctx, team, "web")
	if _, err := s.gitHubCloneToken(ctx, a); !errors.As(err, new(ErrGitHubPaused)) {
		t.Fatalf("a paused app's build: err = %v, want refused as paused", err)
	}

	if _, err := s.DeliverGitHub(ctx, signedDelivery("r-3", "installation_repositories", map[string]any{
		"action": "added", "installation": map[string]any{"id": 7},
		"repositories_added": []map[string]any{{"id": webRepo.ID}},
	})); err != nil {
		t.Fatal(err)
	}
	if got := paused(); got != "" {
		t.Fatalf("after the repository came back, paused = %q", got)
	}

	for _, step := range []struct{ action, want string }{
		{"suspend", GitHubPausedSuspended}, {"unsuspend", ""}, {"deleted", GitHubPausedUninstalled},
	} {
		if _, err := s.DeliverGitHub(ctx, signedDelivery("i-"+step.action, "installation", map[string]any{
			"action": step.action, "installation": map[string]any{"id": 7, "account": map[string]any{"login": "acme"}},
		})); err != nil {
			t.Fatal(err)
		}
		if got := paused(); got != step.want {
			t.Fatalf("after %s, paused = %q, want %q", step.action, got, step.want)
		}
	}
	conns, _ := s.GitHubConnections(ctx, team)
	if len(conns) != 1 || !conns[0].Removed {
		t.Fatalf("connections after uninstall = %+v, want it shown as removed", conns)
	}
}
