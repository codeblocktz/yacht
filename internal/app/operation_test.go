package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

type blockingOperationBuilder struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type ceilingOperationBuilder struct {
	started chan struct{}
	release chan struct{}
	active  atomic.Int32
	max     atomic.Int32
}

func (b *ceilingOperationBuilder) Build(
	ctx context.Context, _ orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	active := b.active.Add(1)
	for old := b.max.Load(); active > old && !b.max.CompareAndSwap(old, active); old = b.max.Load() {
	}
	b.started <- struct{}{}
	defer b.active.Add(-1)
	select {
	case <-ctx.Done():
		return orchestrator.BuildResult{}, ctx.Err()
	case <-b.release:
		return orchestrator.BuildResult{CommitSHA: "deadbeef", RunAsUser: 1000}, nil
	}
}
func (*ceilingOperationBuilder) BuildJobName(orchestrator.BuildRequest) string {
	return "build-ceiling"
}
func (*ceilingOperationBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{Found: true}, nil
}
func (*ceilingOperationBuilder) CancelBuild(context.Context, string) error { return nil }

func (b *blockingOperationBuilder) Build(
	ctx context.Context, _ orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
		return orchestrator.BuildResult{}, ctx.Err()
	case <-b.release:
		return orchestrator.BuildResult{CommitSHA: "deadbeef", RunAsUser: 1000}, nil
	}
}
func (*blockingOperationBuilder) BuildJobName(orchestrator.BuildRequest) string {
	return "build-blocked"
}
func (*blockingOperationBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{Found: true}, nil
}
func (*blockingOperationBuilder) CancelBuild(context.Context, string) error { return nil }

type cancellingOperationBuilder struct{ cancelled chan string }

func (*cancellingOperationBuilder) Build(
	context.Context, orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	return orchestrator.BuildResult{}, errors.New("not used")
}

type liveCancelledBuilder struct {
	started   chan struct{}
	cancelled chan struct{}
	onceStart sync.Once
	onceStop  sync.Once
}

type rolloutScriptOrchestrator struct {
	*recordingOrchestrator
	mu       sync.Mutex
	statuses []orchestrator.AppStatus
	observed chan struct{}
	hold     chan struct{}
	once     sync.Once
}

func (o *rolloutScriptOrchestrator) AppStatus(
	ctx context.Context, _ orchestrator.Ref,
) (orchestrator.AppStatus, error) {
	if o.observed != nil {
		o.once.Do(func() { close(o.observed) })
	}
	if o.hold != nil {
		select {
		case <-ctx.Done():
			return orchestrator.AppStatus{}, ctx.Err()
		case <-o.hold:
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.statuses) == 0 {
		return orchestrator.AppStatus{}, orchestrator.ErrNotFound
	}
	status := o.statuses[0]
	if len(o.statuses) > 1 {
		o.statuses = o.statuses[1:]
	}
	// Most rollout tests script controller health only. Convergence identity is
	// workload metadata written by ApplyApp, so inherit it from the recorded
	// spec unless a test deliberately supplies a conflicting key.
	last := o.recordingOrchestrator.lastAppSpec()
	if status.ReleaseID == "" {
		status.ReleaseID = last.ReleaseID
	}
	if status.ConfigVersion == 0 {
		status.ConfigVersion = last.ConfigVersion
	}
	return status, nil
}

func (b *liveCancelledBuilder) Build(
	ctx context.Context, _ orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	b.onceStart.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
		return orchestrator.BuildResult{}, ctx.Err()
	case <-b.cancelled:
		return orchestrator.BuildResult{}, errors.New("build cancelled")
	}
}
func (*liveCancelledBuilder) BuildJobName(orchestrator.BuildRequest) string {
	return "build-live-cancel"
}
func (*liveCancelledBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{Found: true}, nil
}
func (b *liveCancelledBuilder) CancelBuild(context.Context, string) error {
	b.onceStop.Do(func() { close(b.cancelled) })
	return nil
}
func (*cancellingOperationBuilder) BuildJobName(orchestrator.BuildRequest) string {
	return "build-cancelled"
}
func (*cancellingOperationBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{}, nil
}
func (b *cancellingOperationBuilder) CancelBuild(_ context.Context, name string) error {
	b.cancelled <- name
	return nil
}

func seedOperationAttempt(
	t *testing.T, s *Service, ownerID string, appID uuid.UUID, suffix string,
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `
		INSERT INTO deployments (owner_id, app_id, image, revision, status)
		VALUES ($1, $2, 'nginx:1.27', $3, 'running') RETURNING id`,
		ownerID, appID, suffix).Scan(&id); err != nil {
		t.Fatalf("seed deployment %s: %v", suffix, err)
	}
	return id
}

func claimedCandidate(
	t *testing.T, s *Service, ownerID, name string,
) (App, Release, Operation) {
	t.Helper()
	ctx := context.Background()
	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: name, Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create candidate app: %v", err)
	}
	candidate := a
	candidate.Image = "nginx:1.28"
	release, err := s.createRelease(ctx, s.q, candidate, "")
	if err != nil {
		t.Fatalf("create candidate release: %v", err)
	}
	op, err := s.admitDeployment(ctx, ownerID, candidate, "candidate", release.ID, false)
	if err != nil {
		t.Fatalf("admit candidate: %v", err)
	}
	claimed, err := s.claimOperation(ctx, ownerID, op.ID)
	if err != nil {
		t.Fatalf("claim candidate: %v", err)
	}
	return a, release, claimed
}

func TestConcurrentAdmissionIsEnforcedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "operation-unique")
	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	deployments := []uuid.UUID{
		seedOperationAttempt(t, s, ownerID, a.ID, "race-one"),
		seedOperationAttempt(t, s, ownerID, a.ID, "race-two"),
	}
	connections := make([]*dbgen.Queries, 2)
	for i := range connections {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire connection %d: %v", i, err)
		}
		defer conn.Release()
		connections[i] = dbgen.New(conn)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.enqueueOperation(ctx, connections[i], ownerID, a.ID,
				deployments[i], nil, true)
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	var admitted, refused int
	for err := range errs {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrOperationInFlight):
			refused++
		default:
			t.Fatalf("unexpected admission error: %v", err)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted/refused = %d/%d, want 1/1", admitted, refused)
	}
}

func TestBuildClaimCeilingLeavesTheThirdAppQueued(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{MaxConcurrentBuilds: 2})
	ownerID := owner(t, s, pool, "operation-ceiling")
	for i := range 3 {
		a, err := s.Create(ctx, ownerID, CreateInput{
			Name: fmt.Sprintf("web-%d", i), Image: "nginx:1.27", Replicas: 1, Port: 8080,
		})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		deploymentID := seedOperationAttempt(t, s, ownerID, a.ID, fmt.Sprintf("queued-%d", i))
		_, err = s.enqueueOperation(ctx, s.q, ownerID, a.ID, deploymentID, nil, true)
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	var claimed []Operation
	for i := range 2 {
		op, err := s.ClaimOperation(ctx)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		claimed = append(claimed, op)
	}
	if _, err := s.ClaimOperation(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("third build claim = %v, want no admissible operation", err)
	}
	if err := s.CancelOperation(ctx, claimed[0].OwnerID, claimed[0].ID); err != nil {
		t.Fatalf("cancel first: %v", err)
	}
	if _, err := s.ClaimOperation(ctx); err != nil {
		t.Fatalf("claim after capacity returned: %v", err)
	}
}

func TestQueuedOperationSurvivesAServiceRestart(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "operation-restart")
	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	deploymentID := seedOperationAttempt(t, s, ownerID, a.ID, "survives")
	want, err := s.enqueueOperation(ctx, s.q, ownerID, a.ID, deploymentID, nil, true)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	restarted := NewService(pool, s.orch, s.log, Options{MaxConcurrentBuilds: 2})
	got, err := restarted.ClaimOperation(ctx)
	if err != nil {
		t.Fatalf("claim after restart: %v", err)
	}
	if got.ID != want.ID || got.Status != OperationClaimed {
		t.Fatalf("claimed after restart = %#v, want operation %s", got, want.ID)
	}
}

func TestConcurrentRedeploysAdmitOneDurableBuild(t *testing.T) {
	ctx := context.Background()
	builder := &blockingOperationBuilder{started: make(chan struct{}), release: make(chan struct{})}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "operation-redeploy-race")
	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (
			owner_id, name, namespace, image, replicas, port, source,
			repo_url, repo_branch
		) VALUES ($1, 'web', 'operation-redeploy-web',
			'registry.test/old:web', 1, 8080, 'git',
			'https://example.test/repo.git', 'main')`, ownerID); err != nil {
		t.Fatalf("seed Git app: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- s.Redeploy(ctx, ownerID, "web")
		}()
	}
	close(start)
	var admitted, refused int
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrOperationInFlight):
			refused++
		default:
			t.Fatalf("Redeploy: %v", err)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted/refused = %d/%d, want 1/1", admitted, refused)
	}
	var live int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM deployment_operations
		WHERE owner_id = $1
		  AND status IN ('queued','claimed','building','applying','verifying')`, ownerID).Scan(&live); err != nil {
		t.Fatalf("count live operations: %v", err)
	}
	if live != 1 {
		t.Fatalf("non-terminal operations = %d, want 1", live)
	}

	select {
	case <-builder.started:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted build did not start")
	}
	close(builder.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM deployment_operations
			WHERE owner_id = $1
			  AND status IN ('queued','claimed','building','applying','verifying')`, ownerID).Scan(&live); err != nil {
			t.Fatalf("wait for terminal operation: %v", err)
		}
		if live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admitted operation did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAdmissionDispatcherStartsTwoBuildsAndLeavesTheThirdQueued(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builder := &ceilingOperationBuilder{
		started: make(chan struct{}, 3), release: make(chan struct{}, 3),
	}
	s, _, pool := testService(t, Options{
		Builder: builder, Images: stubImages{}, MaxConcurrentBuilds: 2,
	})
	ownerID := owner(t, s, pool, "operation-dispatch-ceiling")
	for i := range 3 {
		name := fmt.Sprintf("web-%d", i)
		if _, err := pool.Exec(ctx, `
			INSERT INTO apps (
				owner_id, name, namespace, image, replicas, port, source,
				repo_url, repo_branch
			) VALUES ($1, $2, 'operation-dispatch-' || $2,
				'registry.test/old:web', 1, 8080, 'git',
				'https://example.test/repo.git', 'main')`, ownerID, name); err != nil {
			t.Fatalf("seed Git app %d: %v", i, err)
		}
		if err := s.Redeploy(ctx, ownerID, name); err != nil {
			t.Fatalf("Redeploy %d: %v", i, err)
		}
	}
	for i := range 2 {
		select {
		case <-builder.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("build %d did not start", i+1)
		}
	}
	select {
	case <-builder.started:
		t.Fatal("third build started above the configured ceiling")
	case <-time.After(100 * time.Millisecond):
	}

	go s.RunOperationAdmission(ctx)
	builder.release <- struct{}{}
	select {
	case <-builder.started:
	case <-time.After(3 * time.Second):
		t.Fatal("third queued build did not start after capacity returned")
	}
	if got := builder.max.Load(); got != 2 {
		t.Fatalf("maximum concurrent builds = %d, want exactly 2", got)
	}
	builder.release <- struct{}{}
	builder.release <- struct{}{}

	deadline := time.Now().Add(3 * time.Second)
	for {
		var live int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM deployment_operations
			WHERE owner_id = $1
			  AND status IN ('queued','claimed','building','applying','verifying')`, ownerID).Scan(&live); err != nil {
			t.Fatalf("count live operations: %v", err)
		}
		if live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operations still live after releases: %d", live)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReclaimedWorkerCannotPublishWithItsOldToken(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "operation-fence-reclaim")
	a, _, stale := claimedCandidate(t, s, ownerID, "web")
	if _, err := pool.Exec(ctx, `
		UPDATE deployment_operations SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1`, stale.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := s.reclaimOperations(ctx); err != nil {
		t.Fatalf("reclaimOperations: %v", err)
	}
	fresh, err := s.claimOperation(ctx, ownerID, stale.ID)
	if err != nil {
		t.Fatalf("reclaim operation: %v", err)
	}
	if stale.ClaimToken == nil || fresh.ClaimToken == nil || *stale.ClaimToken == *fresh.ClaimToken {
		t.Fatal("reclaim did not issue a new fencing token")
	}
	if err := s.completeOperation(ctx, stale, nil); !errors.Is(err, ErrOperationClaimLost) {
		t.Fatalf("stale completion = %v, want ErrOperationClaimLost", err)
	}
	row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: fresh.ID,
	})
	if err != nil {
		t.Fatalf("read fresh claim: %v", err)
	}
	if row.Status != OperationClaimed || !row.ClaimToken.Valid ||
		uuid.UUID(row.ClaimToken.Bytes) != *fresh.ClaimToken {
		t.Fatalf("fresh claim changed by stale worker: %#v", row)
	}
	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActiveReleaseID == nil || a.ActiveReleaseID == nil ||
		*got.ActiveReleaseID != *a.ActiveReleaseID {
		t.Fatal("stale worker moved the active release pointer")
	}
}

func TestCancellationFencesAWorkerBeforeItCanActivate(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "operation-fence-cancel")
	a, _, claimed := claimedCandidate(t, s, ownerID, "web")
	if err := s.CancelOperation(ctx, ownerID, claimed.ID); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	if err := s.completeOperation(ctx, claimed, nil); !errors.Is(err, ErrOperationClaimLost) {
		t.Fatalf("late success = %v, want ErrOperationClaimLost", err)
	}
	row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: claimed.ID,
	})
	if err != nil {
		t.Fatalf("read cancelled operation: %v", err)
	}
	if row.Status != OperationCancelled || !row.CancelledAt.Valid {
		t.Fatalf("cancelled operation = %#v", row)
	}
	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActiveReleaseID == nil || a.ActiveReleaseID == nil ||
		*got.ActiveReleaseID != *a.ActiveReleaseID {
		t.Fatal("cancelled worker moved the active release pointer")
	}
}

func TestCancellingABuildRemovesItsRecordedJob(t *testing.T) {
	ctx := context.Background()
	builder := &cancellingOperationBuilder{cancelled: make(chan string, 1)}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "operation-cancel-build")
	var appID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO apps (
			owner_id, name, namespace, image, replicas, port, source,
			repo_url, repo_branch
		) VALUES ($1, 'web', 'operation-cancel-build-web',
			'registry.test/old:web', 1, 8080, 'git',
			'https://example.test/repo.git', 'main') RETURNING id`, ownerID).Scan(&appID); err != nil {
		t.Fatalf("seed Git app: %v", err)
	}
	a := App{ID: appID, OwnerID: ownerID, Name: "web", Namespace: "operation-cancel-build-web",
		Image: "registry.test/old:web", Source: SourceGit}
	op, err := s.admitDeployment(ctx, ownerID, a, "redeploy", uuid.Nil, true)
	if err != nil {
		t.Fatalf("admit build: %v", err)
	}
	claimed, err := s.claimOperation(ctx, ownerID, op.ID)
	if err != nil {
		t.Fatalf("claim build: %v", err)
	}
	build, err := s.q.CreateBuild(ctx, dbgen.CreateBuildParams{
		OwnerID: ownerID, AppID: appID, DeploymentID: claimed.DeploymentID,
		RepoUrl: "https://example.test/repo.git", RepoRef: "main",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	if err := s.q.SetBuildJob(ctx, dbgen.SetBuildJobParams{
		ID: build.ID, JobName: "build-cancelled",
	}); err != nil {
		t.Fatalf("SetBuildJob: %v", err)
	}
	if err := s.CancelOperation(ctx, ownerID, claimed.ID); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	select {
	case got := <-builder.cancelled:
		if got != "build-cancelled" {
			t.Fatalf("cancelled Job = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("builder cancellation was not called")
	}
}

func TestTwoWorkersReclaimAnExpiredLeaseExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "operation-reclaim-race")
	_, _, claimed := claimedCandidate(t, s, ownerID, "web")
	if _, err := pool.Exec(ctx, `
		UPDATE deployment_operations SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	connections := make([]*dbgen.Queries, 2)
	for i := range connections {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire connection %d: %v", i, err)
		}
		defer conn.Release()
		connections[i] = dbgen.New(conn)
	}
	start := make(chan struct{})
	counts := make(chan int, 2)
	errs := make(chan error, 2)
	for i := range 2 {
		go func(i int) {
			<-start
			rows, err := connections[i].ReclaimExpiredDeploymentOperations(ctx)
			counts <- len(rows)
			errs <- err
		}(i)
	}
	close(start)
	total := 0
	for range 2 {
		total += <-counts
		if err := <-errs; err != nil {
			t.Fatalf("reclaim: %v", err)
		}
	}
	if total != 1 {
		t.Fatalf("reclaimed rows = %d, want exactly one", total)
	}
	if _, err := s.claimOperation(ctx, ownerID, claimed.ID); err != nil {
		t.Fatalf("claim reclaimed operation: %v", err)
	}
	if _, err := s.claimOperation(ctx, ownerID, claimed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim = %v, want ErrNotFound", err)
	}
}

func TestLeaseRenewalRequiresTheCurrentToken(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "operation-renew-token")
	_, _, claimed := claimedCandidate(t, s, ownerID, "web")
	before := *claimed.LeaseExpiresAt
	time.Sleep(5 * time.Millisecond)
	if err := s.renewOperation(ctx, claimed); err != nil {
		t.Fatalf("renewOperation: %v", err)
	}
	row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: claimed.ID,
	})
	if err != nil {
		t.Fatalf("read renewed operation: %v", err)
	}
	if !row.LeaseExpiresAt.Valid || !row.LeaseExpiresAt.Time.After(before) {
		t.Fatalf("renewed lease = %v, before %v", row.LeaseExpiresAt, before)
	}
	if err := s.CancelOperation(ctx, ownerID, claimed.ID); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	if err := s.renewOperation(ctx, claimed); !errors.Is(err, ErrOperationClaimLost) {
		t.Fatalf("renew after cancellation = %v, want ErrOperationClaimLost", err)
	}
}

func TestCancellingARunningBuildCannotBecomeActiveLater(t *testing.T) {
	ctx := context.Background()
	builder := &liveCancelledBuilder{
		started: make(chan struct{}), cancelled: make(chan struct{}),
	}
	s, _, pool := testService(t, Options{Builder: builder, Images: stubImages{}})
	ownerID := owner(t, s, pool, "operation-live-build-cancel")
	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE apps SET source = 'git', repo_url = 'https://example.test/repo.git',
		                repo_branch = 'main' WHERE id = $1`, a.ID); err != nil {
		t.Fatalf("make app buildable: %v", err)
	}
	if err := s.Redeploy(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	select {
	case <-builder.started:
	case <-time.After(2 * time.Second):
		t.Fatal("build did not start")
	}
	var operationID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id FROM deployment_operations
		WHERE app_id = $1 AND status = 'building'`, a.ID).Scan(&operationID); err != nil {
		t.Fatalf("read claimed operation: %v", err)
	}
	if err := s.CancelOperation(ctx, ownerID, operationID); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
			OwnerID: ownerID, ID: operationID,
		})
		if err != nil {
			t.Fatalf("read operation: %v", err)
		}
		if row.Status == OperationCancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation status = %q, want cancelled", row.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActiveReleaseID == nil || a.ActiveReleaseID == nil ||
		*got.ActiveReleaseID != *a.ActiveReleaseID {
		t.Fatal("cancelled running build changed the active release")
	}
	for {
		var status string
		if err := pool.QueryRow(ctx,
			`SELECT status FROM builds WHERE deployment_id = (
				SELECT deployment_id FROM deployment_operations WHERE id = $1
			)`, operationID).Scan(&status); err != nil {
			t.Fatalf("read cancelled build: %v", err)
		}
		if status != BuildRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled build goroutine did not settle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOperationPersistsEveryStageBeforeAdvancing(t *testing.T) {
	ctx := context.Background()
	s, base, pool := testService(t, Options{
		RolloutTimeout: 2 * time.Second, RolloutPollInterval: time.Millisecond,
	})
	ownerID := owner(t, s, pool, "operation-stage-boundaries")
	_, candidate, claimed := claimedCandidate(t, s, ownerID, "web")
	hold := make(chan struct{})
	observed := make(chan struct{})
	s.orch = &rolloutScriptOrchestrator{
		recordingOrchestrator: base, observed: observed, hold: hold,
		statuses: []orchestrator.AppStatus{{
			Generation: 2, ObservedGeneration: 2, Desired: 1,
			Updated: 1, Ready: 1, Available: 1, AvailableCondition: true,
		}},
	}
	done := make(chan error, 1)
	go func() { done <- s.executeOperation(ctx, claimed) }()
	select {
	case <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("operation never reached rollout observation")
	}
	row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: claimed.ID,
	})
	if err != nil {
		t.Fatalf("read verifying operation: %v", err)
	}
	if row.Status != OperationVerifying || row.Checkpoint != OperationVerifying {
		t.Fatalf("operation at observation = %s/%s, want verifying/verifying",
			row.Status, row.Checkpoint)
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("executeOperation: %v", err)
	}
	row, err = s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: claimed.ID,
	})
	if err != nil {
		t.Fatalf("read succeeded operation: %v", err)
	}
	if row.Status != OperationSucceeded || row.Checkpoint != OperationVerifying {
		t.Fatalf("terminal operation = %s/%s", row.Status, row.Checkpoint)
	}
	app, err := s.Get(ctx, ownerID, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if app.ActiveReleaseID == nil || *app.ActiveReleaseID != candidate.ID {
		t.Fatal("verified release was not activated")
	}
}

func TestSlowRolloutWaitsAndThenSucceeds(t *testing.T) {
	ctx := context.Background()
	s, base, pool := testService(t, Options{
		RolloutTimeout: time.Second, RolloutPollInterval: time.Millisecond,
	})
	ownerID := owner(t, s, pool, "operation-slow-rollout")
	_, candidate, claimed := claimedCandidate(t, s, ownerID, "web")
	s.orch = &rolloutScriptOrchestrator{
		recordingOrchestrator: base,
		statuses: []orchestrator.AppStatus{
			{Generation: 2, ObservedGeneration: 1, Desired: 1},
			{Generation: 2, ObservedGeneration: 2, Desired: 1,
				Updated: 1, Ready: 1, Available: 1, AvailableCondition: true},
		},
	}
	if err := s.executeOperation(ctx, claimed); err != nil {
		t.Fatalf("slow rollout: %v", err)
	}
	app, err := s.Get(ctx, ownerID, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if app.ActiveReleaseID == nil || *app.ActiveReleaseID != candidate.ID {
		t.Fatal("slow healthy rollout did not activate")
	}
}

func TestTerminalRolloutReasonFailsWithoutMovingTheActiveRelease(t *testing.T) {
	ctx := context.Background()
	s, base, pool := testService(t, Options{
		RolloutTimeout: time.Second, RolloutPollInterval: time.Millisecond,
	})
	ownerID := owner(t, s, pool, "operation-rollout-failure")
	prior, _, claimed := claimedCandidate(t, s, ownerID, "web")
	s.orch = &rolloutScriptOrchestrator{
		recordingOrchestrator: base,
		statuses: []orchestrator.AppStatus{{
			Generation: 2, ObservedGeneration: 2, Desired: 1,
			Terminal: true, Reason: "ProgressDeadlineExceeded",
			Message: "deployment exceeded its progress deadline",
		}},
	}
	err := s.executeOperation(ctx, claimed)
	if err == nil || !strings.Contains(err.Error(), "ProgressDeadlineExceeded") {
		t.Fatalf("failed rollout error = %v", err)
	}
	row, readErr := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
		OwnerID: ownerID, ID: claimed.ID,
	})
	if readErr != nil {
		t.Fatalf("read failed operation: %v", readErr)
	}
	if row.Status != OperationFailed ||
		!strings.Contains(row.Message, "ProgressDeadlineExceeded") {
		t.Fatalf("failed operation = %#v", row)
	}
	app, readErr := s.Get(ctx, ownerID, "web")
	if readErr != nil {
		t.Fatalf("Get: %v", readErr)
	}
	if app.ActiveReleaseID == nil || prior.ActiveReleaseID == nil ||
		*app.ActiveReleaseID != *prior.ActiveReleaseID {
		t.Fatal("failed rollout moved the prior active release")
	}
}

func TestCancellationIsTerminalFromEveryOperationStage(t *testing.T) {
	stages := []string{
		OperationQueued, OperationClaimed, OperationBuilding,
		OperationApplying, OperationVerifying,
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			s, _, pool := testService(t, Options{})
			ownerID := owner(t, s, pool, "operation-cancel-stage-"+stage)
			a, candidate, claimed := claimedCandidate(t, s, ownerID, "web")
			op := claimed
			if stage == OperationQueued {
				if _, err := pool.Exec(ctx, `
					UPDATE deployment_operations
					SET status = 'queued', claimed_at = NULL,
					    claim_token = NULL, lease_expires_at = NULL
					WHERE id = $1`, op.ID); err != nil {
					t.Fatalf("return to queue: %v", err)
				}
			} else {
				for _, next := range []string{OperationBuilding, OperationApplying, OperationVerifying} {
					if stage == OperationClaimed {
						break
					}
					if err := s.transitionOperation(ctx, &op, next); err != nil {
						t.Fatalf("transition to %s: %v", next, err)
					}
					if next == stage {
						break
					}
				}
			}
			if err := s.CancelOperation(ctx, ownerID, op.ID); err != nil {
				t.Fatalf("CancelOperation: %v", err)
			}
			row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
				OwnerID: ownerID, ID: op.ID,
			})
			if err != nil {
				t.Fatalf("read cancelled operation: %v", err)
			}
			if row.Status != OperationCancelled || !row.CancelledAt.Valid {
				t.Fatalf("cancelled row = %#v", row)
			}
			if stage != OperationQueued {
				if err := s.completeOperation(ctx, op, nil); !errors.Is(err, ErrOperationClaimLost) {
					t.Fatalf("late completion = %v", err)
				}
			}
			got, err := s.Get(ctx, ownerID, a.Name)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.ActiveReleaseID == nil || *got.ActiveReleaseID == candidate.ID {
				t.Fatal("cancelled candidate became active")
			}
		})
	}
}

func TestFailureIsTerminalFromEveryOwnedOperationStage(t *testing.T) {
	for _, stage := range []string{
		OperationClaimed, OperationBuilding, OperationApplying, OperationVerifying,
	} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			s, _, pool := testService(t, Options{})
			ownerID := owner(t, s, pool, "operation-fail-stage-"+stage)
			prior, candidate, op := claimedCandidate(t, s, ownerID, "web")
			for _, next := range []string{OperationBuilding, OperationApplying, OperationVerifying} {
				if stage == OperationClaimed {
					break
				}
				if err := s.transitionOperation(ctx, &op, next); err != nil {
					t.Fatalf("transition to %s: %v", next, err)
				}
				if next == stage {
					break
				}
			}
			cause := errors.New("stage failed with a useful reason")
			if err := s.completeOperation(ctx, op, cause); !errors.Is(err, cause) {
				t.Fatalf("complete failure = %v", err)
			}
			row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
				OwnerID: ownerID, ID: op.ID,
			})
			if err != nil {
				t.Fatalf("read failed operation: %v", err)
			}
			if row.Status != OperationFailed || row.Message != cause.Error() {
				t.Fatalf("failed row = %#v", row)
			}
			got, err := s.Get(ctx, ownerID, prior.Name)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.ActiveReleaseID == nil || *got.ActiveReleaseID == candidate.ID {
				t.Fatal("failed candidate became active")
			}
		})
	}
}

func TestLeaseReclaimPreservesEveryStageCheckpoint(t *testing.T) {
	for _, stage := range []string{
		OperationClaimed, OperationBuilding, OperationApplying, OperationVerifying,
	} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			s, _, pool := testService(t, Options{})
			ownerID := owner(t, s, pool, "operation-reclaim-stage-"+stage)
			_, _, op := claimedCandidate(t, s, ownerID, "web")
			for _, next := range []string{OperationBuilding, OperationApplying, OperationVerifying} {
				if stage == OperationClaimed {
					break
				}
				if err := s.transitionOperation(ctx, &op, next); err != nil {
					t.Fatalf("transition to %s: %v", next, err)
				}
				if next == stage {
					break
				}
			}
			if _, err := pool.Exec(ctx, `
				UPDATE deployment_operations
				SET lease_expires_at = now() - interval '1 second'
				WHERE id = $1`, op.ID); err != nil {
				t.Fatalf("expire lease: %v", err)
			}
			if err := s.reclaimOperations(ctx); err != nil {
				t.Fatalf("reclaimOperations: %v", err)
			}
			row, err := s.q.GetDeploymentOperation(ctx, dbgen.GetDeploymentOperationParams{
				OwnerID: ownerID, ID: op.ID,
			})
			if err != nil {
				t.Fatalf("read checkpoint: %v", err)
			}
			if row.Status != OperationQueued || row.Checkpoint != stage ||
				row.ClaimToken.Valid || row.LeaseExpiresAt.Valid {
				t.Fatalf("reclaimed stage = %#v", row)
			}
			if stage != OperationClaimed {
				if _, err := s.claimOperation(ctx, ownerID, op.ID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("stage without recovery was claimed: %v", err)
				}
			}
		})
	}
}
