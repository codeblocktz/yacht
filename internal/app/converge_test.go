package app

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

func TestRequestMutationFilesCannotBypassTheConvergenceBoundary(t *testing.T) {
	files := []string{"service.go", "variable.go", "volume.go", "networking.go"}
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "apply" || fn.Name.Name == "applyRelease" {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, _ := sel.X.(*ast.Ident)
				serviceApply := ident != nil && ident.Name == "s" &&
					(sel.Sel.Name == "apply" || sel.Sel.Name == "applyRelease")
				requestOwnedOperation := name == "service.go" && ident != nil && ident.Name == "s" &&
					(sel.Sel.Name == "claimOperation" ||
						sel.Sel.Name == "executeOperation" ||
						sel.Sel.Name == "executeImageOperation" ||
						sel.Sel.Name == "startClaimedOperation")
				if serviceApply || requestOwnedOperation || sel.Sel.Name == "ApplyApp" {
					t.Errorf("%s: %s bypasses the operation/reconciler boundary",
						name, fn.Name.Name)
				}
				return true
			})
		}
	}
}

type convergenceOrchestrator struct {
	*recordingOrchestrator
	applyCalls  atomic.Int32
	statusCalls atomic.Int32
	mu          sync.Mutex
	statusErr   error
}

func (o *convergenceOrchestrator) ApplyApp(
	ctx context.Context, spec orchestrator.AppSpec,
) error {
	o.applyCalls.Add(1)
	return o.recordingOrchestrator.ApplyApp(ctx, spec)
}

func (o *convergenceOrchestrator) AppStatus(
	ctx context.Context, ref orchestrator.Ref,
) (orchestrator.AppStatus, error) {
	o.statusCalls.Add(1)
	o.mu.Lock()
	err := o.statusErr
	o.mu.Unlock()
	if err != nil {
		return orchestrator.AppStatus{}, err
	}
	return o.Noop.AppStatus(ctx, ref)
}

func convergenceService(t *testing.T, opts Options) (*Service, *convergenceOrchestrator, string) {
	t.Helper()
	s, base, pool := testService(t, opts)
	orch := &convergenceOrchestrator{recordingOrchestrator: base}
	s.orch = orch
	return s, orch, owner(t, s, pool, "app-convergence-"+t.Name())
}

func TestLiveOverlayMutationsWaitForTheAppReconciler(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{
		Keeper: testKeeper(t), AppDomain: "apps.test",
	})
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	initialCalls := orch.applyCalls.Load()
	if err := s.SetVariable(ctx, ownerID, a.Name, VariableInput{
		Key: "TOKEN", Value: "current-secret", Secret: true,
	}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if _, err := s.AttachVolume(ctx, ownerID, a.Name, VolumeInput{
		Name: "data", MountPath: "/data", SizeBytes: 1 << 20,
	}); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if err := s.SetNetworking(ctx, ownerID, a.Name, true, false); err != nil {
		t.Fatalf("SetNetworking: %v", err)
	}
	if got := orch.applyCalls.Load(); got != initialCalls {
		t.Fatalf("request paths applied %d workloads, want none", got-initialCalls)
	}

	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if got := orch.applyCalls.Load(); got != initialCalls+1 {
		t.Fatalf("reconciler apply calls = %d, want one", got-initialCalls)
	}
	spec := orch.lastAppSpec()
	if spec.ReleaseID == "" || spec.ConfigVersion <= a.ConfigVersion ||
		spec.Secrets["TOKEN"] != "current-secret" || len(spec.Volumes) != 1 || !spec.HTTPSOnly {
		t.Fatalf("reconciled overlay spec = %#v", spec)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("steady ReconcileApps: %v", err)
	}
	if got := orch.applyCalls.Load(); got != initialCalls+1 {
		t.Fatalf("steady state wrote Kubernetes again: calls=%d", got)
	}
}

func TestAppReconcilerRestoresADeletedActiveWorkload(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{})
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := orch.applyCalls.Load()
	if err := orch.Noop.DeleteApp(ctx, a.Ref()); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if got := orch.applyCalls.Load(); got != before+1 {
		t.Fatalf("restore applies = %d, want one", got-before)
	}
	status, err := orch.AppStatus(ctx, a.Ref())
	if err != nil || a.ActiveReleaseID == nil ||
		!convergenceMatches(status, *a.ActiveReleaseID, a.ConfigVersion) {
		t.Fatalf("restored status = %#v, err=%v", status, err)
	}
}

func TestLiveCandidateWinsAndCancellationRestoresTheActiveRelease(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{})
	active, candidate, claimed := claimedCandidate(t, s, ownerID, "web")
	if err := orch.Noop.DeleteApp(ctx, active.Ref()); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	before := orch.applyCalls.Load()
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps with candidate: %v", err)
	}
	if got := orch.applyCalls.Load(); got != before {
		t.Fatalf("active reconciler clobbered a live candidate: calls=%d", got-before)
	}

	current, err := s.Get(ctx, ownerID, active.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.applyRelease(ctx, s.q, current, &candidate); err != nil {
		t.Fatalf("simulate candidate apply: %v", err)
	}
	if err := s.CancelOperation(ctx, ownerID, claimed.ID); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps after cancel: %v", err)
	}
	got := orch.lastAppSpec()
	if active.ActiveReleaseID == nil || got.ReleaseID != active.ActiveReleaseID.String() {
		t.Fatalf("release after cancellation = %q, want active %v",
			got.ReleaseID, active.ActiveReleaseID)
	}
}

func TestApplyingRecoveryAdoptsAnAlreadyAppliedCandidate(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{
		RolloutTimeout: time.Second, RolloutPollInterval: time.Millisecond,
	})
	a, candidate, claimed := claimedCandidate(t, s, ownerID, "web")
	current, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.applyRelease(ctx, s.q, current, &candidate); err != nil {
		t.Fatalf("simulate candidate apply: %v", err)
	}
	before := orch.applyCalls.Load()
	if _, err := s.pool.Exec(ctx, `
		UPDATE deployment_operations
		SET status = 'queued', checkpoint = 'applying', claimed_at = NULL,
		    claim_token = NULL, lease_expires_at = NULL, stage_started_at = now()
		WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("seed applying recovery: %v", err)
	}
	// Initial deployments have no prior active pointer. Recovery must still
	// discover them from the durable operation checkpoint.
	if _, err := s.pool.Exec(ctx,
		`UPDATE apps SET active_release_id = NULL WHERE id = $1`, a.ID); err != nil {
		t.Fatalf("clear active pointer: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		row, readErr := s.q.GetDeploymentOperation(ctx,
			dbgen.GetDeploymentOperationParams{OwnerID: ownerID, ID: claimed.ID})
		if readErr != nil {
			t.Fatalf("read operation: %v", readErr)
		}
		if row.Status == OperationSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered status = %s/%s", row.Status, row.Checkpoint)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := orch.applyCalls.Load(); got != before {
		t.Fatalf("already-applied candidate was repeated: calls=%d", got-before)
	}
	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil || got.ActiveReleaseID == nil || *got.ActiveReleaseID != candidate.ID {
		t.Fatalf("recovered active pointer = %v, err=%v", got.ActiveReleaseID, err)
	}
}

func TestVerifyingRecoveryAdoptsAnAlreadyHealthyCandidate(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{
		RolloutTimeout: time.Second, RolloutPollInterval: time.Millisecond,
	})
	a, candidate, claimed := claimedCandidate(t, s, ownerID, "web")
	current, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.applyRelease(ctx, s.q, current, &candidate); err != nil {
		t.Fatalf("simulate candidate apply: %v", err)
	}
	before := orch.applyCalls.Load()
	if _, err := s.pool.Exec(ctx, `
		UPDATE deployment_operations
		SET status = 'queued', checkpoint = 'verifying', claimed_at = NULL,
		    claim_token = NULL, lease_expires_at = NULL, stage_started_at = now()
		WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("seed verifying recovery: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		row, readErr := s.q.GetDeploymentOperation(ctx,
			dbgen.GetDeploymentOperationParams{OwnerID: ownerID, ID: claimed.ID})
		if readErr != nil {
			t.Fatalf("read operation: %v", readErr)
		}
		if row.Status == OperationSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered status = %s/%s", row.Status, row.Checkpoint)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := orch.applyCalls.Load(); got != before {
		t.Fatalf("healthy verifying candidate was repeated: calls=%d", got-before)
	}
}

func TestVerifyingRecoveryGetsOneBoundedCurrentStateObservationAfterOriginalBudget(t *testing.T) {
	ctx := context.Background()
	s, base, pool := testService(t, Options{
		RolloutTimeout: 2 * time.Millisecond, RolloutPollInterval: time.Millisecond,
	})
	ownerID := owner(t, s, pool, "app-convergence-expired-verification")
	a, candidate, claimed := claimedCandidate(t, s, ownerID, "web")
	current, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.applyRelease(ctx, s.q, current, &candidate); err != nil {
		t.Fatalf("simulate candidate apply: %v", err)
	}
	s.orch = &delayedHealthyOperationOrchestrator{
		recordingOrchestrator: base, delay: 10 * time.Millisecond,
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE deployment_operations
		SET status = 'queued', checkpoint = 'verifying', claimed_at = NULL,
		    claim_token = NULL, lease_expires_at = NULL,
		    stage_started_at = now() - interval '1 hour'
		WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("seed expired verifying recovery: %v", err)
	}

	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		row, readErr := s.q.GetDeploymentOperation(ctx,
			dbgen.GetDeploymentOperationParams{OwnerID: ownerID, ID: claimed.ID})
		if readErr != nil {
			t.Fatalf("read recovered operation: %v", readErr)
		}
		if row.Status == OperationSucceeded {
			break
		}
		if row.Status == OperationFailed {
			t.Fatalf("healthy rollout failed after outage: %s", row.Message)
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered operation = %s/%s, want succeeded", row.Status, row.Checkpoint)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReconcilerRechecksOperationOwnershipAfterWaitingForTheAppLock(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{})
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := orch.Noop.DeleteApp(ctx, a.Ref()); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	locked := make(chan struct{})
	releaseLock := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- s.withAppConvergenceLock(ctx, a.ID, func() error {
			close(locked)
			<-releaseLock
			return nil
		})
	}()
	<-locked
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- s.ReconcileApps(ctx) }()
	// Let the pass observe that there is no operation and block on the lock.
	time.Sleep(25 * time.Millisecond)
	candidate := a
	candidate.Image = "nginx:1.28"
	release, err := s.createRelease(ctx, s.q, candidate, "")
	if err != nil {
		t.Fatalf("create candidate release: %v", err)
	}
	if _, err := s.admitDeployment(ctx, ownerID, candidate, "candidate", release.ID, false); err != nil {
		t.Fatalf("admit candidate: %v", err)
	}
	before := orch.applyCalls.Load()
	close(releaseLock)
	if err := <-lockDone; err != nil {
		t.Fatalf("lock holder: %v", err)
	}
	if err := <-reconcileDone; err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if got := orch.applyCalls.Load(); got != before {
		t.Fatalf("waiting reconciler overwrote candidate ownership: calls=%d", got-before)
	}
}

func TestClusterObservationFailureDoesNotWriteOrFailHistory(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{})
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	beforeCalls := orch.applyCalls.Load()
	beforePointer := *a.ActiveReleaseID
	orch.mu.Lock()
	orch.statusErr = errors.New("cluster API unavailable")
	orch.mu.Unlock()
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if got := orch.applyCalls.Load(); got != beforeCalls {
		t.Fatalf("cluster outage caused %d writes", got-beforeCalls)
	}
	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil || got.ActiveReleaseID == nil || *got.ActiveReleaseID != beforePointer {
		t.Fatalf("cluster outage changed active pointer: %v, err=%v", got.ActiveReleaseID, err)
	}
}

func TestClusterOutageLeavesRecoveredOperationRetryable(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{
		RolloutTimeout: time.Second, RolloutPollInterval: time.Millisecond,
	})
	prior, _, claimed := claimedCandidate(t, s, ownerID, "web")
	if _, err := s.pool.Exec(ctx, `
		UPDATE deployment_operations
		SET status = 'queued', checkpoint = 'verifying', claimed_at = NULL,
		    claim_token = NULL, lease_expires_at = NULL, stage_started_at = now()
		WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("seed verifying recovery: %v", err)
	}
	orch.mu.Lock()
	orch.statusErr = errors.New("cluster API unavailable")
	orch.mu.Unlock()
	before := orch.applyCalls.Load()
	beforeStatus := orch.statusCalls.Load()
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		row, err := s.q.GetDeploymentOperation(ctx,
			dbgen.GetDeploymentOperationParams{OwnerID: ownerID, ID: claimed.ID})
		if err != nil {
			t.Fatalf("read operation: %v", err)
		}
		if orch.statusCalls.Load() > beforeStatus &&
			row.Status == OperationQueued && row.Checkpoint == OperationVerifying &&
			!row.ClaimToken.Valid {
			break
		}
		if row.Status == OperationFailed || time.Now().After(deadline) {
			t.Fatalf("operation during outage = %s/%s", row.Status, row.Checkpoint)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := orch.applyCalls.Load(); got != before {
		t.Fatalf("outage recovery wrote %d workloads", got-before)
	}
	got, err := s.Get(ctx, ownerID, prior.Name)
	if err != nil || prior.ActiveReleaseID == nil || got.ActiveReleaseID == nil ||
		*got.ActiveReleaseID != *prior.ActiveReleaseID {
		t.Fatalf("outage changed active pointer: %v, err=%v", got.ActiveReleaseID, err)
	}
}

func TestClusterOutageDuringLiveVerificationReturnsTheStageToReconciliation(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{
		RolloutTimeout: time.Second, RolloutPollInterval: time.Millisecond,
	})
	prior, _, claimed := claimedCandidate(t, s, ownerID, "web")
	orch.mu.Lock()
	orch.statusErr = errors.New("cluster API unavailable")
	orch.mu.Unlock()
	err := s.executeOperation(ctx, claimed)
	if !errors.Is(err, ErrClusterObservation) {
		t.Fatalf("executeOperation error = %v, want cluster observation", err)
	}
	row, err := s.q.GetDeploymentOperation(ctx,
		dbgen.GetDeploymentOperationParams{OwnerID: ownerID, ID: claimed.ID})
	if err != nil {
		t.Fatalf("read operation: %v", err)
	}
	if row.Status != OperationQueued || row.Checkpoint != OperationVerifying ||
		row.ClaimToken.Valid {
		t.Fatalf("operation after outage = %s/%s token=%v",
			row.Status, row.Checkpoint, row.ClaimToken.Valid)
	}
	var deploymentStatus string
	if err := s.pool.QueryRow(ctx,
		`SELECT status FROM deployments WHERE id = $1`, claimed.DeploymentID).
		Scan(&deploymentStatus); err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	if deploymentStatus != DeployRunning {
		t.Fatalf("deployment status = %q, want retryable running", deploymentStatus)
	}
	got, err := s.Get(ctx, ownerID, prior.Name)
	if err != nil || prior.ActiveReleaseID == nil || got.ActiveReleaseID == nil ||
		*got.ActiveReleaseID != *prior.ActiveReleaseID {
		t.Fatalf("outage changed active pointer: %v, err=%v", got.ActiveReleaseID, err)
	}
}
