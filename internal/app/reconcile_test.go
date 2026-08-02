package app

import (
	"context"
	"errors"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
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

	a, err := s.Create(ctx, ownerID, CreateInput{
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

	a, err := s.Create(ctx, ownerID, CreateInput{
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

	a, err := s.Create(ctx, ownerID, CreateInput{
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

// A cluster that will not answer is not evidence a build died.
//
// Failing on a read error would turn an unreachable API server into every
// in-flight deployment being marked failed, all at once, a minute later.
func TestAnUnreadableClusterSettlesNothing(t *testing.T) {
	ctx := context.Background()
	builder := &stubBuilder{err: errors.New("connection refused")}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-reconcile-down")

	a, err := s.Create(ctx, ownerID, CreateInput{
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

	a, err := s.Create(ctx, ownerID, CreateInput{
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
