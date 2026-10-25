package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The Deploy section's runtime settings reach the workload and the release: a
// schedule, a restart policy and a healthcheck timeout are how the release
// runs, and a rollback runs them as they were.
func TestDeploySettingsReachTheReleaseAndTheWorkload(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.deploy-settings.test"})
	id := owner(t, s, pool, "deploy-settings")

	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "report", Image: "ghcr.io/acme/report:1", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if hosts := orch.lastAppSpec().Hosts; len(hosts) == 0 {
		t.Fatalf("an app that always runs has hosts %v, want its platform hostname", hosts)
	}
	if _, err := s.Update(ctx, id, "report", UpdateInput{
		Image: a.Image, Port: a.Port,
		CronSchedule: " 0 3 * * * ", RestartPolicy: orchestrator.RestartNever,
		RestartMaxRetries: 7, HealthTimeoutSeconds: 45,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "report"); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	spec := orch.lastAppSpec()
	if spec.Schedule != "0 3 * * *" || spec.RestartPolicy != orchestrator.RestartNever ||
		spec.HealthTimeoutSeconds != 45 {
		t.Errorf("applied schedule %q, policy %q, timeout %d",
			spec.Schedule, spec.RestartPolicy, spec.HealthTimeoutSeconds)
	}
	// Retries belong to on-failure; kept for never, they would come back
	// unasked the day somebody switched back.
	if spec.MaxRestarts != 0 {
		t.Errorf("max restarts = %d with the never policy, want none kept", spec.MaxRestarts)
	}
	// Nothing routes to a workload on a schedule.
	if len(spec.Hosts) != 0 {
		t.Errorf("a scheduled app is routed at %v, want no hostname", spec.Hosts)
	}

	got, err := s.Get(ctx, id, "report")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != orchestrator.PhaseScheduled {
		t.Errorf("phase = %q, want scheduled", got.Status.Phase)
	}
	release, err := s.ReleaseByID(ctx, id, got.ID, *got.ActiveReleaseID)
	if err != nil {
		t.Fatalf("ReleaseByID: %v", err)
	}
	if release.CronSchedule != "0 3 * * *" || release.RestartPolicy != orchestrator.RestartNever ||
		release.HealthTimeoutSeconds != 45 {
		t.Errorf("release = %q, %q, %d", release.CronSchedule, release.RestartPolicy, release.HealthTimeoutSeconds)
	}
}

// An expression Kubernetes would refuse is refused where somebody typed it,
// and nothing is saved.
func TestAnInvalidScheduleIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "deploy-settings-invalid")

	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, in := range []UpdateInput{
		{CronSchedule: "every day at 3"},
		{CronSchedule: "TZ=Africa/Dar_es_Salaam 0 3 * * *"},
		{RestartPolicy: "sometimes"},
		{HealthTimeoutSeconds: orchestrator.MaxHealthTimeoutSeconds + 1},
		{DockerfilePath: "../Dockerfile"},
		{WatchPaths: []string{"!docs/**"}},
	} {
		in.Image, in.Port = a.Image, a.Port
		if _, err := s.Update(ctx, id, "web", in); err == nil {
			t.Errorf("Update(%+v) succeeded, want it refused", in)
		}
	}
	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CronSchedule != "" || got.ConfigVersion != a.ConfigVersion {
		t.Errorf("after refusals: schedule %q, version %d (was %d)", got.CronSchedule, got.ConfigVersion, a.ConfigVersion)
	}
}

// neverReadyOrchestrator applies, and never reports the new version ready.
type neverReadyOrchestrator struct{ *recordingOrchestrator }

func (o neverReadyOrchestrator) AppStatus(context.Context, orchestrator.Ref) (orchestrator.AppStatus, error) {
	return orchestrator.AppStatus{Phase: orchestrator.PhasePending, Desired: 1, Message: "0/1 replicas ready"}, nil
}

// A healthcheck timeout is the rollout's budget: a version not ready within it
// fails its deploy then, and says it was the app's own timeout.
func TestTheHealthcheckTimeoutBoundsTheRollout(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{RolloutPollInterval: 50 * time.Millisecond})
	id := owner(t, s, pool, "deploy-settings-timeout")

	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: a.Image, Port: a.Port, HealthTimeoutSeconds: 1,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	s.orch = neverReadyOrchestrator{orch}

	started := time.Now()
	err = executeNamedOperation(s, ctx, id, "web")
	if err == nil || !strings.Contains(err.Error(), "healthcheck timeout of 1s") {
		t.Fatalf("deploy = %v, want it failed on the healthcheck timeout", err)
	}
	// The engine's own budget is ten minutes; a second, and a little, is the
	// app's.
	if waited := time.Since(started); waited > 10*time.Second {
		t.Errorf("waited %s, want about the app's one second", waited)
	}
}

// watchPush is a push to main that changed files, signed for the hook.
func watchPush(t *testing.T, secret string, payload map[string]any) Push {
	t.Helper()
	payload["ref"], payload["after"] = "refs/heads/main", sha
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode push: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return Push{Event: "push", Body: body, Signature: "sha256=" + hex.EncodeToString(mac.Sum(nil))}
}

func commits(files ...string) []map[string]any {
	return []map[string]any{{"modified": files}}
}

// A push that changed nothing the watch paths match is skipped, and shown in
// the history as skipped. One that did deploys; so does one whose changes
// cannot all be seen, rather than skipping a change nobody looked at.
func TestWatchPathsSkipAPushThatChangedNothingWatched(t *testing.T) {
	ctx := context.Background()
	s, ownerID, a, secret := hookFixture(t, "hook-watch-paths")
	if _, err := s.Update(ctx, ownerID, a.Name, UpdateInput{
		Port: a.Port, Repo: a.Repo,
		WatchPaths: []string{"/backend/**\n# the docs deploy elsewhere\n!/backend/**/*.md"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	a, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(a.WatchPaths) != 2 {
		t.Fatalf("watch paths = %q, want two patterns with the comment gone", a.WatchPaths)
	}

	result, err := s.DeliverPush(ctx, a.ID, watchPush(t, secret, map[string]any{
		"commits": commits("frontend/app.js", "backend/README.md"),
	}))
	if err != nil {
		t.Fatalf("DeliverPush: %v", err)
	}
	if !strings.HasPrefix(result, "Skipped") {
		t.Errorf("result = %q, want the push skipped", result)
	}
	if trigger := liveTrigger(t, s, a); trigger != "" {
		t.Errorf("a skipped push admitted %q", trigger)
	}
	deps, err := s.Deployments(ctx, ownerID, a.ID, 1)
	if err != nil || len(deps) == 0 {
		t.Fatalf("Deployments: %v", err)
	}
	if deps[0].Status != DeploySkipped || deps[0].Message != skippedPushMessage {
		t.Errorf("history says %q (%q), want the push shown skipped", deps[0].Status, deps[0].Message)
	}

	// Twenty of forty commits, as GitLab sends: not all of what changed.
	result, err = s.DeliverPush(ctx, a.ID, watchPush(t, secret, map[string]any{
		"commits": commits("frontend/app.js"), "total_commits_count": 40,
	}))
	if err != nil {
		t.Fatalf("DeliverPush: %v", err)
	}
	if !strings.HasPrefix(result, "Deploying") {
		t.Errorf("an incomplete push = %q, want it deployed", result)
	}
	if err := s.CancelLiveDeployment(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	result, err = s.DeliverPush(ctx, a.ID, watchPush(t, secret, map[string]any{
		"commits": commits("backend/main.go"),
	}))
	if err != nil {
		t.Fatalf("DeliverPush: %v", err)
	}
	if !strings.HasPrefix(result, "Deploying") {
		t.Errorf("a watched change = %q, want it deployed", result)
	}
}

func TestWatchedChangeReadsPatternsAsGitignoreDoes(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		files    []string
		want     bool
	}{
		{[]string{"/backend/**"}, []string{"backend/cmd/main.go"}, true},
		{[]string{"/backend/**"}, []string{"frontend/backend/x.go"}, false},
		{[]string{"*.go"}, []string{"deep/in/the/tree.go"}, true},
		{[]string{"**", "!docs/**"}, []string{"docs/guide.md"}, false},
		{[]string{"**", "!docs/**"}, []string{"docs/guide.md", "main.go"}, true},
	} {
		if got := watchedChange(c.patterns, c.files); got != c.want {
			t.Errorf("watchedChange(%q, %q) = %v, want %v", c.patterns, c.files, got, c.want)
		}
	}
}

func TestADockerfilePathStaysInTheRepository(t *testing.T) {
	for p, ok := range map[string]bool{
		"": true, "Dockerfile": true, "docker/web.Dockerfile": true, "./Dockerfile": true,
		"/Dockerfile": false, "../Dockerfile": false, "a/../../Dockerfile": false, "docker/": false,
	} {
		if err := validateDockerfilePath(p); (err == nil) != ok {
			t.Errorf("validateDockerfilePath(%q) = %v, want ok=%v", p, err, ok)
		}
	}
	if got := cleanDockerfilePath(" ./docker/Dockerfile "); got != "docker/Dockerfile" {
		t.Errorf("cleaned = %q", got)
	}
}
