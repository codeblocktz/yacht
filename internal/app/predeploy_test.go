package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// deployWithCommands deploys an app, then saves the given commands, and
// returns it with the seam's record cleared — so what a test reads is what the
// save's own rollout did.
func deployWithCommands(
	t *testing.T, s *Service, orch *recordingOrchestrator, ownerID, preDeploy, start string,
) App {
	t.Helper()
	ctx := context.Background()
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "ghcr.io/acme/web:1", Replicas: 1, Port: 4000,
		Env: map[string]string{"MIX_ENV": "prod"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orch.takeEvents()
	updated, err := s.Update(ctx, ownerID, "web", UpdateInput{
		Image: a.Image, Port: a.Port,
		PreDeployCommand: preDeploy, StartCommand: start,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return updated
}

// The reason this exists: a Phoenix release had its migrations run by hand,
// with kubectl exec, after every deploy. Saved as a pre-deploy command, they
// run in the new image, with the app's variables, before it is applied.
func TestAPreDeployCommandRunsBeforeTheApply(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "predeploy-order")

	a := deployWithCommands(t, s, orch, id, "  bin/migrate  ", "bin/web start")
	if a.PreDeployCommand != "bin/migrate" || a.StartCommand != "bin/web start" {
		t.Fatalf("saved commands = %q, %q; want them trimmed and kept",
			a.PreDeployCommand, a.StartCommand)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	events, reqs := orch.takeEvents()
	if !slices.Equal(events, []string{"pre-deploy", "apply"}) {
		t.Fatalf("seam calls = %v, want the pre-deploy command before the apply", events)
	}
	req := reqs[0]
	if req.Command != "bin/migrate" {
		t.Errorf("command = %q, want the saved one", req.Command)
	}
	applied := orch.lastAppSpec()
	// The image the app is about to run, pinned, and the release it belongs
	// to: not whatever the app ran before.
	if req.App.Image != applied.Image || req.App.ReleaseID != applied.ReleaseID {
		t.Errorf("ran in %q (%s), want the applied %q (%s)",
			req.App.Image, req.App.ReleaseID, applied.Image, applied.ReleaseID)
	}
	if req.App.Env["MIX_ENV"] != "prod" {
		t.Errorf("env = %v, want the app's variables", req.App.Env)
	}
	if req.App.Namespace != a.Namespace {
		t.Errorf("namespace = %q, want the app's %q", req.App.Namespace, a.Namespace)
	}
	if applied.StartCommand != "bin/web start" {
		t.Errorf("applied start command = %q, want the saved one", applied.StartCommand)
	}

	// And recorded on the release, which is what a rollback reads.
	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	release, err := s.ReleaseByID(ctx, id, got.ID, *got.ActiveReleaseID)
	if err != nil {
		t.Fatalf("ReleaseByID: %v", err)
	}
	if release.PreDeployCommand != "bin/migrate" || release.StartCommand != "bin/web start" {
		t.Errorf("release commands = %q, %q", release.PreDeployCommand, release.StartCommand)
	}
}

// A failing command fails the deploy, says why in the command's own words, and
// leaves the running version alone: nothing applied, the active release where
// it was.
func TestAFailingPreDeployFailsTheDeployWithoutApplying(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "predeploy-fails")

	before, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "ghcr.io/acme/web:1", Replicas: 1, Port: 4000,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orch.preDeploy = func(orchestrator.PreDeployRequest) (orchestrator.PreDeployResult, error) {
		return orchestrator.PreDeployResult{
			ExitCode: 1,
			Output: []string{
				"** (Postgrex.Error) ERROR 42P01 (undefined_table)",
				`relation "users" does not exist`,
			},
		}, nil
	}
	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: before.Image, Port: before.Port, PreDeployCommand: "bin/migrate",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	orch.takeEvents()

	err = executeNamedOperation(s, ctx, id, "web")
	var failure *PreDeployFailure
	if !errors.As(err, &failure) {
		t.Fatalf("deploy error = %v, want the command's failure", err)
	}
	if events, _ := orch.takeEvents(); !slices.Equal(events, []string{"pre-deploy"}) {
		t.Errorf("seam calls = %v, want the command and no apply", events)
	}

	after, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if *after.ActiveReleaseID != *before.ActiveReleaseID {
		t.Errorf("active release moved to %s, want it left at %s",
			*after.ActiveReleaseID, *before.ActiveReleaseID)
	}
	deps, err := s.Deployments(ctx, id, after.ID, 1)
	if err != nil || len(deps) == 0 {
		t.Fatalf("Deployments: %v", err)
	}
	if deps[0].Status != DeployFailed {
		t.Errorf("deployment status = %q, want failed", deps[0].Status)
	}
	for _, want := range []string{"pre-deploy command exited 1", `relation "users" does not exist`} {
		if !strings.Contains(deps[0].Message, want) {
			t.Errorf("message = %q, want it to say %q", deps[0].Message, want)
		}
	}
}

// No command, no run: every app that never set one deploys as it did.
func TestAnEmptyPreDeployIsSkipped(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "predeploy-empty")

	deployWithCommands(t, s, orch, id, "   ", "bin/web start")
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if events, _ := orch.takeEvents(); !slices.Equal(events, []string{"apply"}) {
		t.Errorf("seam calls = %v, want the apply alone", events)
	}
}

// A rollback runs the command of the release it returns to. The command saved
// since was written for code the rollback is taking away.
func TestARollbackRunsItsReleasesCommand(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "predeploy-rollback")

	a := deployWithCommands(t, s, orch, id, "bin/migrate --to 1", "")
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	first, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: a.Image, Port: a.Port, PreDeployCommand: "bin/migrate --to 2",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	orch.takeEvents()

	if err := s.Rollback(ctx, id, "web", *first.ActiveReleaseID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("rollback deploy: %v", err)
	}
	_, reqs := orch.takeEvents()
	if len(reqs) != 1 || reqs[0].Command != "bin/migrate --to 1" {
		t.Fatalf("rollback ran %+v, want the first release's command", reqs)
	}
	restored, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if restored.PreDeployCommand != "bin/migrate --to 1" {
		t.Errorf("app's command after rollback = %q, want the restored release's", restored.PreDeployCommand)
	}
}

// A worker that stopped in its applying stage cannot say whether its command
// ran. The workload says the apply did not, so recovery runs the command again
// before applying, rather than starting the new version on an unmigrated
// database.
func TestRecoveryRunsThePreDeployCommandBeforeApplying(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "predeploy-recovery")

	deployWithCommands(t, s, orch, id, "bin/migrate", "")
	a, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	op, err := s.liveOperation(ctx, a)
	if err != nil {
		t.Fatalf("liveOperation: %v", err)
	}
	if op, err = s.claimOperation(ctx, id, op.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.transitionOperation(ctx, &op, OperationApplying); err != nil {
		t.Fatalf("enter applying: %v", err)
	}
	// The worker goes away here, with its lease.
	if _, err := pool.Exec(ctx, `
		UPDATE deployment_operations SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1`, op.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := s.reclaimOperations(ctx); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	queued, err := s.liveOperation(ctx, a)
	if err != nil {
		t.Fatalf("liveOperation: %v", err)
	}
	recovered, err := s.claimRecoverableOperation(ctx, queued)
	if err != nil {
		t.Fatalf("claimRecoverableOperation: %v", err)
	}
	if err := s.executeRecoveredAppOperation(ctx, recovered); err != nil {
		t.Fatalf("recovered deploy: %v", err)
	}
	if events, _ := orch.takeEvents(); !slices.Equal(events, []string{"pre-deploy", "apply"}) {
		t.Errorf("seam calls = %v, want the command again before the apply", events)
	}
}

// plainOrchestrator is an orchestrator that cannot run a command: the
// interface alone, without the optional one beside it.
type plainOrchestrator struct{ orchestrator.Orchestrator }

// An install whose orchestrator cannot run the command refuses the deploy
// rather than skipping a migration the new version needs.
func TestAPreDeployCommandWithNowhereToRunFailsTheDeploy(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "predeploy-unsupported")

	deployWithCommands(t, s, orch, id, "bin/migrate", "")
	plain := NewService(pool, plainOrchestrator{orch},
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Manifests: staticManifests{}, RolloutSettle: -1})
	if err := executeNamedOperation(plain, ctx, id, "web"); !errors.Is(err, ErrNoPreDeployer) {
		t.Fatalf("deploy = %v, want ErrNoPreDeployer", err)
	}
	if events, _ := orch.takeEvents(); len(events) != 0 {
		t.Errorf("seam calls = %v, want nothing applied", events)
	}
	row, err := s.q.GetApp(ctx, dbgen.GetAppParams{OwnerID: id, Name: "web"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if row.PreDeployCommand != "bin/migrate" {
		t.Errorf("command = %q, want it kept for the next deploy", row.PreDeployCommand)
	}
}

// namespaceOrchestrator records each namespace it is asked to make among the
// seam's other events, so a test can say it came before the command that
// runs in it.
type namespaceOrchestrator struct{ *recordingOrchestrator }

func (o namespaceOrchestrator) EnsureNamespace(ctx context.Context, spec orchestrator.NamespaceSpec) error {
	o.mu.Lock()
	o.events = append(o.events, "namespace")
	o.mu.Unlock()
	return o.recordingOrchestrator.EnsureNamespace(ctx, spec)
}

// firstDeployService is a team with a database to reference, and a service
// that builds from source with the database's URL and a secret of its own,
// its pre-deploy command saved before it has ever been deployed — the order
// somebody sets up an app with migrations in. The seam reports namespaces
// too, and nothing has been deployed of the service when it returns.
func firstDeployService(t *testing.T, team string) (*Service, *recordingOrchestrator, string) {
	t.Helper()
	ctx := context.Background()
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("p"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	base, orch, pool := testService(t, Options{})
	s := NewService(pool, namespaceOrchestrator{orch},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{
			Keeper: keeper, Manifests: staticManifests{}, Images: fakeImages{}, RolloutSettle: -1,
			Builder: &uploadBuilder{}, SourceFetchURL: "https://yacht.example.test",
		})
	id := owner(t, base, pool, team)

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "postgres", Image: "postgres:16-alpine", Replicas: 1, Port: 5432,
	}); err != nil {
		t.Fatalf("Create postgres: %v", err)
	}
	if err := s.SetVariable(ctx, id, "postgres", VariableInput{
		Key: "DATABASE_URL", Value: "postgres://app:hunter2@postgres/app", Secret: true,
	}); err != nil {
		t.Fatalf("set the database's URL: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "postgres"); err != nil &&
		!errors.Is(err, ErrNotFound) {
		t.Fatalf("deploy postgres: %v", err)
	}

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Empty: true, Replicas: 1, Port: 4000,
		Env: map[string]string{"MIX_ENV": "prod"},
		Variables: []VariableInput{
			{Key: "DATABASE_URL", Value: "${{postgres.DATABASE_URL}}"},
			{Key: "SECRET_KEY_BASE", Value: "s3cr3t", Secret: true},
		},
	}); err != nil {
		t.Fatalf("Create web: %v", err)
	}
	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Port: 4000, PreDeployCommand: "/app/bin/migrate",
	}); err != nil {
		t.Fatalf("save the pre-deploy command: %v", err)
	}
	orch.takeEvents()
	return s, orch, id
}

// deployFirstUpload deploys the service's first source, as `up` does.
func deployFirstUpload(t *testing.T, s *Service, id string) error {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.DeployUpload(ctx, id, "web",
		sourceArchive(t, map[string]string{"Dockerfile": "FROM busybox\n"})); err != nil {
		t.Fatalf("DeployUpload: %v", err)
	}
	return executeNamedOperation(s, ctx, id, "web")
}

// The usual setup for an app with migrations: its pre-deploy command is
// saved before it is first deployed, so the command runs on that very first
// deploy — before anything else has made the app's namespace. The namespace
// is made first, the command runs in it with everything the app will have,
// and only then is the app itself applied.
func TestAFirstDeployRunsItsPreDeployCommandInAReadyNamespace(t *testing.T) {
	ctx := context.Background()
	s, orch, id := firstDeployService(t, "predeploy-first")

	if err := deployFirstUpload(t, s, id); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	events, reqs := orch.takeEvents()
	if !slices.Equal(events, []string{"namespace", "pre-deploy", "namespace", "apply"}) {
		t.Fatalf("seam calls = %v, want the namespace, then the command, then the apply", events)
	}
	a, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	req := reqs[0]
	if req.Command != "/app/bin/migrate" || req.App.Namespace != a.Namespace {
		t.Errorf("ran %q in %q, want the saved command in %q", req.Command, req.App.Namespace, a.Namespace)
	}
	applied := orch.lastAppSpec()
	if req.App.Image != applied.Image || len(req.App.RegistryAuth) == 0 {
		t.Errorf("ran in %q with a pull credential %t, want the built %q with one",
			req.App.Image, len(req.App.RegistryAuth) > 0, applied.Image)
	}
	// The command's environment is the app's: the reference resolved, the
	// secret opened, both as secrets, and the platform's own variables.
	for _, key := range []string{"DATABASE_URL", "SECRET_KEY_BASE"} {
		if req.App.Secrets[key] == "" || req.App.Secrets[key] != applied.Secrets[key] {
			t.Errorf("the command's %s is not the app's", key)
		}
	}
	if req.App.Secrets["DATABASE_URL"] != "postgres://app:hunter2@postgres/app" {
		t.Error("the command's DATABASE_URL is not the database's")
	}
	if !maps.Equal(req.App.Env, applied.Env) {
		t.Errorf("command env = %v, want the app's %v", req.App.Env, applied.Env)
	}
	if req.App.Env["MIX_ENV"] != "prod" || req.App.Env[DefaultPlatformVariablePrefix+PlatformServiceName] != "web" {
		t.Errorf("command env = %v, want the app's variables and the platform's", req.App.Env)
	}
	if a.ActiveReleaseID == nil {
		t.Error("the first deploy left no active release")
	}
}

// A first deploy whose command fails starts nothing, and leaves the app
// where a Redeploy, once the command is fixed, deploys it as a first deploy
// would have.
func TestAFailedFirstPreDeployCanBeRedeployed(t *testing.T) {
	ctx := context.Background()
	s, orch, id := firstDeployService(t, "predeploy-first-fails")

	orch.mu.Lock()
	orch.preDeploy = func(orchestrator.PreDeployRequest) (orchestrator.PreDeployResult, error) {
		return orchestrator.PreDeployResult{ExitCode: 1, Output: []string{"connection refused"}}, nil
	}
	orch.mu.Unlock()
	var failure *PreDeployFailure
	if err := deployFirstUpload(t, s, id); !errors.As(err, &failure) {
		t.Fatalf("first deploy = %v, want the command's failure", err)
	}
	if events, _ := orch.takeEvents(); !slices.Equal(events, []string{"namespace", "pre-deploy"}) {
		t.Fatalf("seam calls = %v, want the namespace and the command, and nothing applied", events)
	}
	failed, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if failed.ActiveReleaseID != nil {
		t.Errorf("a failed first deploy made release %s active", *failed.ActiveReleaseID)
	}

	orch.mu.Lock()
	orch.preDeploy = nil
	orch.mu.Unlock()
	if err := s.Redeploy(ctx, id, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	events, reqs := orch.takeEvents()
	if !slices.Equal(events, []string{"namespace", "pre-deploy", "namespace", "apply"}) {
		t.Fatalf("seam calls = %v, want the command again before the apply", events)
	}
	if reqs[0].App.Secrets["DATABASE_URL"] != "postgres://app:hunter2@postgres/app" {
		t.Error("the second run's DATABASE_URL is not the database's")
	}
	deployed, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if deployed.ActiveReleaseID == nil {
		t.Fatal("the redeploy left no active release")
	}
	deps, err := s.Deployments(ctx, id, deployed.ID, 1)
	if err != nil || len(deps) == 0 {
		t.Fatalf("Deployments: %v", err)
	}
	if deps[0].Status == DeployFailed {
		t.Errorf("redeploy failed: %s", deps[0].Message)
	}
}

// What the command printed is the deployment's, in its Deploy logs, for a
// deploy that built nothing too, and after a later deploy has replaced it:
// the Job that ran it is deleted with its pod's log once it finishes
// (DEPLOY-14).
func TestAPreDeployCommandsOutputIsKeptWithItsDeployment(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "predeploy-output")

	orch.preDeploy = func(orchestrator.PreDeployRequest) (orchestrator.PreDeployResult, error) {
		return orchestrator.PreDeployResult{
			Succeeded: true, Output: []string{"PREDEPLOY-RAN", "PREDEPLOY-DONE"},
		}, nil
	}
	a := deployWithCommands(t, s, orch, id, "bin/migrate", "")
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	deps, err := s.Deployments(ctx, id, a.ID, 1)
	if err != nil || len(deps) == 0 {
		t.Fatalf("Deployments: %v", err)
	}
	ran := deps[0].ID

	want := []string{
		"Running the pre-deploy command: bin/migrate",
		"PREDEPLOY-RAN", "PREDEPLOY-DONE",
		"The pre-deploy command finished.",
	}
	logs, err := s.DeploymentLogs(ctx, id, "web", ran, LogRequest{})
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if !slices.Equal(logs.PreDeploy, want) {
		t.Fatalf("pre-deploy output = %q, want %q", logs.PreDeploy, want)
	}

	// Replaced, it still has the output, though its containers are gone.
	if err := s.Redeploy(ctx, id, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if logs, err = s.DeploymentLogs(ctx, id, "web", ran, LogRequest{}); err != nil {
		t.Fatalf("DeploymentLogs after a redeploy: %v", err)
	}
	if logs.Live || !slices.Equal(logs.PreDeploy, want) {
		t.Errorf("replaced deployment: live %v, pre-deploy output %q", logs.Live, logs.PreDeploy)
	}
}
