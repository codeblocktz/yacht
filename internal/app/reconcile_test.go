package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/registry"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// stubBuilder answers for a build without running one.
type stubBuilder struct {
	state orchestrator.BuildState
	err   error

	built int
}

func (b *stubBuilder) Build(
	context.Context, orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	b.built++
	return orchestrator.BuildResult{}, errors.New("not run in this test")
}

func (b *stubBuilder) BuildJobName(orchestrator.BuildRequest) string { return "build-test" }

func (b *stubBuilder) BuildState(
	context.Context, string,
) (orchestrator.BuildState, error) {
	return b.state, b.err
}
func (*stubBuilder) CancelBuild(context.Context, string) error { return nil }

// stubImages names an image without a registry.
type stubImages struct{}

func (stubImages) ImageFor(_ context.Context, owner, app, rev string) (string, error) {
	return "registry.test/" + owner + "-" + app + ":" + rev, nil
}
func (stubImages) Configured(context.Context) bool { return true }
func (stubImages) Insecure(context.Context) bool   { return false }
func (stubImages) DockerConfig(context.Context) ([]byte, error) {
	return []byte(`{"auths":{}}`), nil
}

type recoveryManifests struct {
	digest string
	err    error
	refs   []string
}

func (m *recoveryManifests) ResolveDigest(_ context.Context, ref string) (string, error) {
	m.refs = append(m.refs, ref)
	return m.digest, m.err
}

func interruptedOperationBuild(
	t *testing.T, s *Service, ownerID string, a App,
) (Operation, dbgen.Build) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
		UPDATE apps
		SET source = 'git', repo_url = 'https://example.test/x.git', repo_branch = 'main'
		WHERE owner_id = $1 AND id = $2`, ownerID, a.ID); err != nil {
		t.Fatalf("make app buildable: %v", err)
	}
	a, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get Git app: %v", err)
	}
	admitted, err := s.admitDeployment(ctx, ownerID, a, "redeploy", uuid.Nil, true)
	if err != nil {
		t.Fatalf("admit build: %v", err)
	}
	op, err := s.claimOperation(ctx, ownerID, admitted.ID)
	if err != nil {
		t.Fatalf("claim build: %v", err)
	}
	if err := s.transitionOperation(ctx, &op, OperationBuilding); err != nil {
		t.Fatalf("enter building: %v", err)
	}
	build, err := s.q.CreateBuild(ctx, dbgen.CreateBuildParams{
		OwnerID: ownerID, AppID: a.ID, DeploymentID: op.DeploymentID,
		RepoUrl: a.Repo.URL, RepoRef: a.Repo.Ref(),
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	if err := s.q.SetBuildJob(ctx, dbgen.SetBuildJobParams{
		ID: build.ID, JobName: "build-interrupted-" + revisionFor(op.DeploymentID),
	}); err != nil {
		t.Fatalf("SetBuildJob: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE builds SET started_at = now() - interval '1 hour' WHERE id = $1`,
		build.ID); err != nil {
		t.Fatalf("age interrupted build row: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE deployment_operations
		SET lease_expires_at = now() - interval '1 second' WHERE id = $1`,
		op.ID); err != nil {
		t.Fatalf("expire interrupted operation: %v", err)
	}
	return op, build
}

func buildingOperationWithoutRow(
	t *testing.T, s *Service, ownerID string, a App,
) (App, Operation) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
		UPDATE apps
		SET source = 'git', repo_url = 'https://example.test/x.git', repo_branch = 'main'
		WHERE owner_id = $1 AND id = $2`, ownerID, a.ID); err != nil {
		t.Fatalf("make app buildable: %v", err)
	}
	a, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get Git app: %v", err)
	}
	admitted, err := s.admitDeployment(ctx, ownerID, a, "redeploy", uuid.Nil, true)
	if err != nil {
		t.Fatalf("admit build: %v", err)
	}
	op, err := s.claimOperation(ctx, ownerID, admitted.ID)
	if err != nil {
		t.Fatalf("claim build: %v", err)
	}
	if err := s.transitionOperation(ctx, &op, OperationBuilding); err != nil {
		t.Fatalf("enter building: %v", err)
	}
	return a, op
}

func waitForOperationStatus(
	t *testing.T, s *Service, ownerID string, id uuid.UUID, want string,
) dbgen.DeploymentOperation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		row, err := s.q.GetDeploymentOperation(context.Background(),
			dbgen.GetDeploymentOperationParams{OwnerID: ownerID, ID: id})
		if err != nil {
			t.Fatalf("read operation: %v", err)
		}
		if row.Status == want {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation status = %q, want %q", row.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// abandonedBuild writes a build that claims to be running and is not.
//
// Aged past the grace period directly in the database, because the alternative
// is a test that sleeps for two minutes to prove a timestamp comparison.
func abandonedBuild(t *testing.T, s *Service, ownerID string, a App) dbgen.Build {
	t.Helper()
	ctx := context.Background()

	deploy := s.beginDeployment(ctx, ownerID, a, "redeploy")
	row, err := s.q.CreateBuild(ctx, dbgen.CreateBuildParams{
		OwnerID: ownerID, AppID: a.ID, DeploymentID: deploy,
		RepoUrl: "https://example.test/x.git", RepoRef: "main",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	if err := s.q.SetBuildJob(ctx, dbgen.SetBuildJobParams{
		ID: row.ID, JobName: "build-gone",
	}); err != nil {
		t.Fatalf("SetBuildJob: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE builds SET started_at = now() - interval '1 hour' WHERE id = $1`,
		row.ID); err != nil {
		t.Fatalf("age the build: %v", err)
	}
	return row
}

// A build whose Job is gone stops claiming to run.
//
// This is the failure the reconciler exists for: the goroutine driving a build
// does not survive a restart, so without something reading the cluster the
// deployment sits on "running" for as long as the row is kept.
func TestABuildWhoseJobIsGoneIsSettled(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{} // Found: false — no such Job.
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile")

	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildFailed {
		t.Errorf("build status = %q, want %q", got.Status, BuildFailed)
	}
	if got.Message == "" {
		t.Error("nothing says why the build ended")
	}

	// And the deployment it was for, which is the row somebody actually looks
	// at. A settled build under a deployment still marked running would have
	// fixed nothing.
	deps, err := s.Deployments(ctx, ownerID, a.ID, 10)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	for _, d := range deps {
		if d.ID == row.DeploymentID && d.Status == DeployRunning {
			t.Error("the deployment is still running after its build was settled")
		}
	}
}

// A build whose Job is still going is left alone.
//
// The reconciler runs every minute against every running build, so a version
// that could not tell "still working" from "gone" would kill each build about
// a minute in.
func TestABuildStillRunningIsNotTouched(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{state: orchestrator.BuildState{Found: true}}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-live")

	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildRunning {
		t.Errorf("a running build was settled: status = %q", got.Status)
	}
}

// A build that has only just started is left alone.
//
// The row is written before the Job is created, so a reconcile landing in that
// window sees no Job. Without the grace period it would fail every build a
// moment after it began — and the reconciler runs every minute, so it would.
func TestABuildThatJustStartedIsNotSettled(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &stubBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-young")

	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deploy := s.beginDeployment(ctx, ownerID, a, "redeploy")
	row, err := s.q.CreateBuild(ctx, dbgen.CreateBuildParams{
		OwnerID: ownerID, AppID: a.ID, DeploymentID: deploy,
		RepoUrl: "https://example.test/x.git", RepoRef: "main",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildRunning {
		t.Errorf("a build seconds old was settled: %q — %s", got.Status, got.Message)
	}
}

func TestBuildingCheckpointBeforeBuildRowIsFencedAndSafelyRetried(t *testing.T) {
	ctx := context.Background()
	builder := &ceilingOperationBuilder{
		started: make(chan struct{}, 2), release: make(chan struct{}, 2),
	}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-recover-before-build-row")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a, stale := buildingOperationWithoutRow(t, s, ownerID, a)
	if _, err := s.pool.Exec(ctx, `
		UPDATE deployment_operations SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1`, stale.ID); err != nil {
		t.Fatalf("expire building lease: %v", err)
	}

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}
	reset, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: stale.ID,
	})
	if err != nil {
		t.Fatalf("read reset operation: %v", err)
	}
	if reset.Status != OperationQueued || reset.Checkpoint != OperationClaimed ||
		reset.ClaimToken.Valid || reset.LeaseExpiresAt.Valid || reset.ClaimedAt.Valid {
		t.Fatalf("missing-row recovery bypassed admission: %#v", reset)
	}
	select {
	case <-builder.started:
		t.Fatal("reconciler launched the recovered build directly")
	case <-time.After(100 * time.Millisecond):
	}

	staleErr := make(chan error, 1)
	go func() {
		_, err := s.buildOperation(ctx, &stale, a)
		staleErr <- err
	}()
	select {
	case err := <-staleErr:
		if !errors.Is(err, ErrOperationClaimLost) {
			t.Fatalf("stale pre-row worker = %v, want ErrOperationClaimLost", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale pre-row worker did not return")
	}
	select {
	case <-builder.started:
		t.Fatal("stale worker launched a duplicate build")
	case <-time.After(100 * time.Millisecond):
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go s.RunOperationAdmission(workerCtx)
	select {
	case <-builder.started:
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary admission did not retry the missing-row operation")
	}
	builder.release <- struct{}{}
	waitForOperationStatus(t, s, ownerID, stale.ID, OperationSucceeded)
	var builds int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM builds WHERE deployment_id = $1`, stale.DeploymentID).Scan(&builds); err != nil {
		t.Fatalf("count builds: %v", err)
	}
	if builds != 1 {
		t.Fatalf("build rows = %d, want exactly one", builds)
	}
	if err := s.Redeploy(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("future admission remained blocked: %v", err)
	}
	future, err := s.liveOperation(ctx, a)
	if err != nil {
		t.Fatalf("read future operation: %v", err)
	}
	if err := s.CancelOperation(ctx, ownerID, future.ID); err != nil {
		t.Fatalf("clean future operation: %v", err)
	}
}

func TestMissingBuildRecoveryReentersAdmissionUnderTheBuildCeiling(t *testing.T) {
	ctx := context.Background()
	builder := &ceilingOperationBuilder{
		started: make(chan struct{}, 4), release: make(chan struct{}, 4),
	}
	s, _, pool := testService(t, Options{
		Builder: builder, Images: stubImages{}, MaxConcurrentBuilds: 10,
	})
	ownerID := owner(t, s, pool, "owner-recover-build-ceiling")

	var stranded []Operation
	var apps []App
	for i := range 4 {
		a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
			Name: fmt.Sprintf("web-%d", i), Image: "nginx:alpine", Replicas: 1, Port: 80,
		})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		a, op := buildingOperationWithoutRow(t, s, ownerID, a)
		apps = append(apps, a)
		stranded = append(stranded, op)
		if _, err := pool.Exec(ctx, `
			UPDATE deployment_operations
			SET lease_expires_at = now() - interval '1 second'
			WHERE owner_id = $1 AND id = $2`, ownerID, op.ID); err != nil {
			t.Fatalf("expire operation %d: %v", i, err)
		}
	}
	// More stranded rows can exist after a configuration decrease or an older
	// release. The recovery pass must obey the current ceiling, not the value
	// under which those rows were originally admitted.
	s.opts.MaxConcurrentBuilds = 2

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}
	var normalized int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM deployment_operations
		WHERE owner_id = $1 AND status = 'queued' AND checkpoint = 'claimed'
		  AND claim_token IS NULL AND lease_expires_at IS NULL AND claimed_at IS NULL`,
		ownerID).Scan(&normalized); err != nil {
		t.Fatalf("count normalized operations: %v", err)
	}
	if normalized != len(stranded) {
		t.Fatalf("normalized operations = %d, want %d", normalized, len(stranded))
	}
	select {
	case <-builder.started:
		t.Fatal("recovery launched a build outside ordinary admission")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := s.buildOperation(ctx, &stranded[0], apps[0]); !errors.Is(err, ErrOperationClaimLost) {
		t.Fatalf("stale recovered worker = %v, want ErrOperationClaimLost", err)
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go s.RunOperationAdmission(workerCtx)
	for i := range 2 {
		select {
		case <-builder.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("build %d did not start", i+1)
		}
	}
	select {
	case <-builder.started:
		t.Fatal("third recovered build started above the configured ceiling")
	case <-time.After(100 * time.Millisecond):
	}
	var waiting int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM deployment_operations
		WHERE owner_id = $1 AND status = 'queued' AND checkpoint = 'claimed'`,
		ownerID).Scan(&waiting); err != nil {
		t.Fatalf("count capacity-waiting operations: %v", err)
	}
	if waiting != 2 {
		t.Fatalf("operations waiting for build capacity = %d, want 2", waiting)
	}

	builder.release <- struct{}{}
	select {
	case <-builder.started:
	case <-time.After(3 * time.Second):
		t.Fatal("third recovered build did not start after capacity returned")
	}
	builder.release <- struct{}{}
	select {
	case <-builder.started:
	case <-time.After(3 * time.Second):
		t.Fatal("fourth recovered build did not start after capacity returned")
	}
	if got := builder.max.Load(); got != 2 {
		t.Fatalf("maximum concurrent recovered builds = %d, want exactly 2", got)
	}
	builder.release <- struct{}{}
	builder.release <- struct{}{}
	for _, op := range stranded {
		waitForOperationStatus(t, s, ownerID, op.ID, OperationSucceeded)
	}
}

func TestMissingBuildRecoveryIsolatesAPoisonedCandidate(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{} // A missing running Job should still be settled.
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-recover-poison-isolation")

	seedMissing := func(name string) Operation {
		t.Helper()
		a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
			Name: name, Image: "nginx:alpine", Replicas: 1, Port: 80,
		})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		_, op := buildingOperationWithoutRow(t, s, ownerID, a)
		if _, err := pool.Exec(ctx, `
			UPDATE deployment_operations
			SET lease_expires_at = now() - interval '1 second'
			WHERE owner_id = $1 AND id = $2`, ownerID, op.ID); err != nil {
			t.Fatalf("expire %s operation: %v", name, err)
		}
		return op
	}
	poisoned := seedMissing("poisoned")
	healthy := seedMissing("healthy")
	if _, err := pool.Exec(ctx, `
		UPDATE deployment_operations
		SET stage_started_at = CASE id
			WHEN $2 THEN now() - interval '2 hours'
			ELSE now() - interval '1 hour'
		END
		WHERE owner_id = $1 AND id IN ($2, $3)`, ownerID, poisoned.ID, healthy.ID); err != nil {
		t.Fatalf("order recovery candidates: %v", err)
	}

	runningApp, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "running", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create running app: %v", err)
	}
	running := abandonedBuild(t, s, ownerID, runningApp)

	const trigger = "yacht_test_poison_missing_build_reset"
	const function = "yacht_test_reject_missing_build_reset"
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION yacht_test_reject_missing_build_reset()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.id::text = TG_ARGV[0] AND NEW.checkpoint = 'claimed' THEN
				RAISE EXCEPTION 'deterministic poisoned recovery candidate';
			END IF;
			RETURN NEW;
		END
		$$`); err != nil {
		t.Fatalf("create poison trigger function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DROP TRIGGER IF EXISTS "+trigger+" ON deployment_operations")
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+function+"()")
	})
	createTrigger := fmt.Sprintf(`
		CREATE TRIGGER yacht_test_poison_missing_build_reset
		BEFORE UPDATE ON deployment_operations
		FOR EACH ROW EXECUTE FUNCTION yacht_test_reject_missing_build_reset('%s')`,
		poisoned.ID.String())
	if _, err := pool.Exec(ctx, createTrigger); err != nil {
		t.Fatalf("create poison trigger: %v", err)
	}

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("poisoned candidate stopped reconciliation: %v", err)
	}
	poisonedRow, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: poisoned.ID,
	})
	if err != nil {
		t.Fatalf("read poisoned operation: %v", err)
	}
	if poisonedRow.Status != OperationQueued || poisonedRow.Checkpoint != OperationBuilding {
		t.Fatalf("poisoned candidate was partially reset: %#v", poisonedRow)
	}
	healthyRow, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: healthy.ID,
	})
	if err != nil {
		t.Fatalf("read healthy operation: %v", err)
	}
	if healthyRow.Status != OperationQueued || healthyRow.Checkpoint != OperationClaimed ||
		healthyRow.ClaimToken.Valid || healthyRow.LeaseExpiresAt.Valid {
		t.Fatalf("healthy candidate did not recover after poison: %#v", healthyRow)
	}
	settled, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: running.ID})
	if err != nil {
		t.Fatalf("read independently running build: %v", err)
	}
	if settled.Status != BuildFailed {
		t.Fatalf("running-build reconciliation stopped after poison: %q", settled.Status)
	}

	if _, err := pool.Exec(ctx, "DROP TRIGGER "+trigger+" ON deployment_operations"); err != nil {
		t.Fatalf("drop poison trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP FUNCTION "+function+"()"); err != nil {
		t.Fatalf("drop poison trigger function: %v", err)
	}
	for _, id := range []uuid.UUID{poisoned.ID, healthy.ID} {
		if err := s.CancelOperation(ctx, ownerID, id); err != nil {
			t.Fatalf("clean queued operation %s: %v", id, err)
		}
	}
}

func TestBuildingCheckpointAfterBuildRowFailsHonestlyWithoutDuplicateBuild(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-recover-after-build-row")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a, stale := buildingOperationWithoutRow(t, s, ownerID, a)
	build, err := s.q.CreateBuild(ctx, dbgen.CreateBuildParams{
		OwnerID: ownerID, AppID: a.ID, DeploymentID: stale.DeploymentID,
		RepoUrl: a.Repo.URL, RepoRef: a.Repo.Ref(),
	})
	if err != nil {
		t.Fatalf("insert build row: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE builds SET started_at = now() - interval '1 hour' WHERE id = $1`,
		build.ID); err != nil {
		t.Fatalf("age build row: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE deployment_operations SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1`, stale.ID); err != nil {
		t.Fatalf("seed post-row crash: %v", err)
	}
	if err := s.reclaimOperations(ctx); err != nil {
		t.Fatalf("reclaim post-row operation: %v", err)
	}
	if _, err := s.buildOperation(ctx, &stale, a); !errors.Is(err, ErrOperationClaimLost) {
		t.Fatalf("stale post-row worker = %v, want ErrOperationClaimLost", err)
	}

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}
	failed := waitForOperationStatus(t, s, ownerID, stale.ID, OperationFailed)
	if !strings.Contains(failed.Message, "restarted mid-build") {
		t.Fatalf("post-row failure message = %q", failed.Message)
	}
	if builder.built != 0 {
		t.Fatalf("recovery launched %d duplicate builds", builder.built)
	}
	var builds int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM builds WHERE deployment_id = $1`, stale.DeploymentID).Scan(&builds); err != nil {
		t.Fatalf("count builds: %v", err)
	}
	if builds != 1 {
		t.Fatalf("build rows = %d, want existing row only", builds)
	}
	if err := s.Redeploy(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("future admission remained blocked: %v", err)
	}
	future, err := s.liveOperation(ctx, a)
	if err != nil {
		t.Fatalf("read future operation: %v", err)
	}
	if err := s.CancelOperation(ctx, ownerID, future.ID); err != nil {
		t.Fatalf("clean future operation: %v", err)
	}
}

// A cluster that will not answer is not evidence a build died.
//
// Failing on a read error would turn an unreachable API server into every
// in-flight deployment being marked failed, all at once, a minute later.
func TestAnUnreadableClusterSettlesNothing(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{err: errors.New("connection refused")}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-down")

	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds returned an error rather than skipping: %v", err)
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildRunning {
		t.Errorf("an unreachable cluster settled a build: %q", got.Status)
	}
}

// Settling twice writes the same thing.
//
// Several replicas run this at once and all of them reach the same conclusion,
// so the second one through must not corrupt what the first wrote.
func TestSettlingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &stubBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-twice")

	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)

	for i := range 2 {
		if err := s.ReconcileBuilds(ctx); err != nil {
			t.Fatalf("ReconcileBuilds pass %d: %v", i, err)
		}
	}

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != BuildFailed {
		t.Errorf("status = %q after two passes", got.Status)
	}
}

func TestFinishedBuildRecoversItsDeploymentSpecificDigestAndSucceeds(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{state: orchestrator.BuildState{Found: true, Done: true}}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-recover-present")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	prior := *a.ActiveReleaseID
	op, build := interruptedOperationBuild(t, s, ownerID, a)
	manifests := &recoveryManifests{
		digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	s.manifests = manifests

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}
	finished := waitForOperationStatus(t, s, ownerID, op.ID, OperationSucceeded)
	if !finished.ReleaseID.Valid {
		t.Fatal("recovered operation has no release")
	}
	wantRef := "registry.test/" + ownerID + "-web:" + revisionFor(op.DeploymentID)
	if len(manifests.refs) != 1 || manifests.refs[0] != wantRef {
		t.Fatalf("resolved refs = %v, want %q", manifests.refs, wantRef)
	}
	release, err := s.ReleaseByID(ctx, ownerID, a.ID, uuid.UUID(finished.ReleaseID.Bytes))
	if err != nil {
		t.Fatalf("ReleaseByID: %v", err)
	}
	if release.ImageRef != wantRef || release.ImageDigest != manifests.digest {
		t.Fatalf("recovered release = %#v", release)
	}
	gotBuild, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{
		OwnerID: ownerID, ID: build.ID,
	})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if gotBuild.Status != BuildSucceeded || gotBuild.Image != wantRef {
		t.Fatalf("recovered build = %#v", gotBuild)
	}
	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActiveReleaseID == nil || *got.ActiveReleaseID == prior ||
		*got.ActiveReleaseID != release.ID {
		t.Fatal("recovered release did not become active")
	}
}

func TestFinishedBuildWithAnAbsentManifestFailsWithRedeployGuidance(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{state: orchestrator.BuildState{Found: true, Done: true}}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-recover-absent")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	prior := *a.ActiveReleaseID
	op, build := interruptedOperationBuild(t, s, ownerID, a)
	s.manifests = &recoveryManifests{err: registry.ErrManifestNotFound}

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}
	failed := waitForOperationStatus(t, s, ownerID, op.ID, OperationFailed)
	if !strings.Contains(failed.Message, "deploy again") {
		t.Fatalf("failure message = %q", failed.Message)
	}
	gotBuild, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{
		OwnerID: ownerID, ID: build.ID,
	})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if gotBuild.Status != BuildFailed || !strings.Contains(gotBuild.Message, "deploy again") {
		t.Fatalf("absent build = %#v", gotBuild)
	}
	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActiveReleaseID == nil || *got.ActiveReleaseID != prior {
		t.Fatal("absent recovered image moved the active release")
	}
}

func TestFinishedBuildWithAnUnreachableRegistryStaysRetryable(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{state: orchestrator.BuildState{Found: true, Done: true}}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-recover-unreachable")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	prior := *a.ActiveReleaseID
	op, build := interruptedOperationBuild(t, s, ownerID, a)
	s.manifests = &recoveryManifests{err: registry.ErrUnreachable}

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}
	row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: op.ID,
	})
	if err != nil {
		t.Fatalf("read retryable operation: %v", err)
	}
	if row.Status != OperationQueued || row.Checkpoint != OperationBuilding ||
		row.FinishedAt.Valid {
		t.Fatalf("unreachable operation = %#v", row)
	}
	gotBuild, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{
		OwnerID: ownerID, ID: build.ID,
	})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if gotBuild.Status != BuildRunning {
		t.Fatalf("unreachable build status = %q", gotBuild.Status)
	}
	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActiveReleaseID == nil || *got.ActiveReleaseID != prior {
		t.Fatal("unreachable registry moved the active release")
	}
}

func TestGenuinelyFailedBuildNeverResolvesAStaleManifest(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{state: orchestrator.BuildState{
		Found: true, Done: true, Failed: true, Reason: "build pod failed",
	}}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-recover-real-failure")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	op, _ := interruptedOperationBuild(t, s, ownerID, a)
	manifests := &recoveryManifests{
		digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
	s.manifests = manifests

	if err := s.ReconcileBuilds(ctx); err != nil {
		t.Fatalf("ReconcileBuilds: %v", err)
	}
	failed := waitForOperationStatus(t, s, ownerID, op.ID, OperationFailed)
	if failed.Message != "build pod failed" {
		t.Fatalf("failed operation message = %q", failed.Message)
	}
	if len(manifests.refs) != 0 {
		t.Fatalf("failed build resolved stale refs: %v", manifests.refs)
	}
}
