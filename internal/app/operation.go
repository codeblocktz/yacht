package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

const (
	OperationQueued    = "queued"
	OperationClaimed   = "claimed"
	OperationBuilding  = "building"
	OperationApplying  = "applying"
	OperationVerifying = "verifying"
	OperationSucceeded = "succeeded"
	OperationFailed    = "failed"
	OperationCancelled = "cancelled"
)

const operationLease = 30 * time.Second

// ErrOperationInFlight is the durable per-app admission decision. Callers may
// report it, but must not work around it with an application-level pre-check.
var ErrOperationInFlight = errors.New("app: a deployment operation is already in flight")
var ErrOperationClaimLost = errors.New("app: deployment operation claim was lost")
var ErrClusterObservation = errors.New("app: cluster observation unavailable")

// ErrOperationRecoveryPending is deliberately non-terminal. Build and app
// reconcilers observe cluster truth after a process stops inside a stage; the
// durable checkpoint lets them do that without guessing or rebuilding.
var ErrOperationRecoveryPending = errors.New("app: operation stage awaits reconciliation")

// Operation is the durable, fenced stage record behind one deployment attempt.
// Checkpoint remains at the last entered stage when a lease is reclaimed, so a
// later reconciler can observe cluster truth rather than repeat earlier work.
type Operation struct {
	ID             uuid.UUID
	OwnerID        string
	AppID          uuid.UUID
	DeploymentID   uuid.UUID
	ReleaseID      *uuid.UUID
	RequiresBuild  bool
	Status         string
	Message        string
	CreatedAt      time.Time
	ClaimedAt      *time.Time
	FinishedAt     *time.Time
	ClaimToken     *uuid.UUID
	LeaseExpiresAt *time.Time
	CancelledAt    *time.Time
	Checkpoint     string
	StageStartedAt time.Time
}

func (s *Service) enqueueOperation(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID, deploymentID uuid.UUID,
	releaseID *uuid.UUID, requiresBuild bool,
) (Operation, error) {
	row, err := q.CreateDeploymentOperation(ctx, dbgen.CreateDeploymentOperationParams{
		OwnerID: ownerID, AppID: appID, DeploymentID: deploymentID,
		ReleaseID: pgOptionalUUID(releaseID), RequiresBuild: requiresBuild,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Operation{}, ErrOperationInFlight
		}
		return Operation{}, fmt.Errorf("app: enqueue deployment operation: %w", err)
	}
	return toOperation(row), nil
}

// admitDeployment commits the attempt and its queue record together. A
// request can therefore return only after both exist, and a uniqueness failure
// cannot leave a running-looking deployment with no operation behind it.
func (s *Service) admitDeployment(
	ctx context.Context, ownerID string, a App, trigger string,
	releaseID uuid.UUID, requiresBuild bool,
) (Operation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, fmt.Errorf("app: begin deployment admission: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	deployment, err := q.CreateDeployment(ctx, dbgen.CreateDeploymentParams{
		OwnerID: ownerID, AppID: a.ID, Image: a.Image,
		Revision: trigger, Status: DeployRunning,
		ReleaseID: pgUUID(releaseID), Trigger: trigger, ActorKind: "user",
	})
	if err != nil {
		return Operation{}, fmt.Errorf("app: record admitted deployment: %w", err)
	}
	var release *uuid.UUID
	if releaseID != uuid.Nil {
		release = &releaseID
	}
	op, err := s.enqueueOperation(ctx, q, ownerID, a.ID, deployment.ID, release, requiresBuild)
	if err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, fmt.Errorf("app: commit deployment admission: %w", err)
	}
	return op, nil
}

// ClaimOperation selects one globally admissible operation. The advisory lock
// and count live in the same short transaction, which is what makes the build
// ceiling hold across independent Yacht processes rather than only goroutines.
func (s *Service) ClaimOperation(ctx context.Context) (Operation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, fmt.Errorf("app: begin operation claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	if err := q.LockDeploymentAdmission(ctx); err != nil {
		return Operation{}, fmt.Errorf("app: lock operation admission: %w", err)
	}
	row, err := q.ClaimNextDeploymentOperation(ctx, dbgen.ClaimNextDeploymentOperationParams{
		LeaseDuration:       leaseInterval(),
		MaxConcurrentBuilds: int64(s.opts.MaxConcurrentBuilds),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, ErrNotFound
		}
		return Operation{}, fmt.Errorf("app: claim deployment operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, fmt.Errorf("app: commit operation claim: %w", err)
	}
	return toOperation(row), nil
}

func (s *Service) claimOperation(
	ctx context.Context, ownerID string, id uuid.UUID,
) (Operation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, fmt.Errorf("app: begin operation claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	if err := q.LockDeploymentAdmission(ctx); err != nil {
		return Operation{}, fmt.Errorf("app: lock operation admission: %w", err)
	}
	row, err := q.ClaimDeploymentOperation(ctx, dbgen.ClaimDeploymentOperationParams{
		LeaseDuration: leaseInterval(), ID: id, OwnerID: ownerID,
		MaxConcurrentBuilds: int64(s.opts.MaxConcurrentBuilds),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, ErrNotFound
		}
		return Operation{}, fmt.Errorf("app: claim deployment operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, fmt.Errorf("app: commit operation claim: %w", err)
	}
	return toOperation(row), nil
}

func (s *Service) transitionOperation(
	ctx context.Context, op *Operation, next string,
) error {
	if op.ClaimToken == nil {
		return ErrOperationClaimLost
	}
	row, err := s.q.TransitionDeploymentOperation(ctx,
		dbgen.TransitionDeploymentOperationParams{
			NextStatus: next, OwnerID: op.OwnerID, ID: op.ID,
			ExpectedStatus: op.Status, ClaimToken: pgUUID(*op.ClaimToken),
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOperationClaimLost
		}
		return fmt.Errorf("app: transition operation %s to %s: %w", op.ID, next, err)
	}
	*op = toOperation(row)
	return nil
}

func (s *Service) renewOperation(ctx context.Context, op Operation) error {
	if op.ClaimToken == nil {
		return ErrOperationClaimLost
	}
	n, err := s.q.RenewDeploymentOperationLease(ctx,
		dbgen.RenewDeploymentOperationLeaseParams{
			LeaseDuration: leaseInterval(), OwnerID: op.OwnerID, ID: op.ID,
			ClaimToken: pgUUID(*op.ClaimToken),
		})
	if err != nil {
		return fmt.Errorf("app: renew operation lease: %w", err)
	}
	if n != 1 {
		return ErrOperationClaimLost
	}
	return nil
}

func (s *Service) maintainOperationLease(
	ctx context.Context, op Operation,
) (context.Context, func()) {
	work, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			cancel()
		})
	}
	go func() {
		tick := time.NewTicker(operationLease / 3)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-work.Done():
				return
			case <-tick.C:
				if err := s.renewOperation(work, op); err != nil {
					s.log.Warn("operation lease lost", "operation_id", op.ID, "error", err)
					cancel()
					return
				}
			}
		}
	}()
	return work, stop
}

func (s *Service) reclaimOperations(ctx context.Context) error {
	rows, err := s.q.ReclaimExpiredDeploymentOperations(ctx)
	if err != nil {
		return fmt.Errorf("app: reclaim expired operations: %w", err)
	}
	for _, row := range rows {
		s.log.Info("expired operation returned to queue", "operation_id", row.ID)
	}
	return nil
}

// CancelOperation makes cancellation terminal before any best-effort cluster
// cleanup. Clearing the claim token in the same transaction is what prevents a
// worker already past its last checkpoint from publishing success afterward.
func (s *Service) CancelOperation(ctx context.Context, ownerID string, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin operation cancellation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	row, err := q.CancelDeploymentOperation(ctx, dbgen.CancelDeploymentOperationParams{
		OwnerID: ownerID, ID: id,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("app: cancel deployment operation: %w", err)
	}
	if _, err := q.FinishDeployment(ctx, dbgen.FinishDeploymentParams{
		OwnerID: ownerID, ID: row.DeploymentID,
		Status: OperationCancelled, Message: row.Message,
	}); err != nil {
		return fmt.Errorf("app: record cancelled deployment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit operation cancellation: %w", err)
	}
	if row.RequiresBuild {
		build, readErr := s.q.GetBuildForDeployment(ctx, dbgen.GetBuildForDeploymentParams{
			OwnerID: ownerID, DeploymentID: row.DeploymentID,
		})
		if readErr != nil && !errors.Is(readErr, pgx.ErrNoRows) {
			return fmt.Errorf("app: read cancelled build: %w", readErr)
		}
		if readErr == nil && build.JobName != "" {
			if s.builder == nil {
				return ErrNoBuilder
			}
			if err := s.builder.CancelBuild(ctx, build.JobName); err != nil {
				return fmt.Errorf("app: cancel build job: %w", err)
			}
		}
	}
	return nil
}

// completeOperation fences the terminal attempt, deployment history, and
// active pointer in one transaction. Cancellation or lease reclaim clears the
// token first, so a displaced worker changes none of the three.
func (s *Service) completeOperation(ctx context.Context, op Operation, cause error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin operation completion: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	status, deployStatus, message := OperationSucceeded, DeploySucceeded, ""
	if cause != nil {
		status, deployStatus, message = OperationFailed, DeployFailed, cause.Error()
	}
	if op.ClaimToken == nil {
		return ErrOperationClaimLost
	}
	expected := op.Status
	if cause == nil {
		expected = OperationVerifying
	}
	n, err := q.FinishDeploymentOperation(ctx, dbgen.FinishDeploymentOperationParams{
		Status: status, Message: message, OwnerID: op.OwnerID, ID: op.ID,
		ExpectedStatus: expected, ClaimToken: pgUUID(*op.ClaimToken),
	})
	if err != nil {
		return fmt.Errorf("app: fence operation completion: %w", err)
	}
	if n != 1 {
		return ErrOperationClaimLost
	}
	if _, err := q.FinishDeployment(ctx, dbgen.FinishDeploymentParams{
		OwnerID: op.OwnerID, ID: op.DeploymentID,
		Status: deployStatus, Message: message,
	}); err != nil {
		return fmt.Errorf("app: finish fenced deployment: %w", err)
	}
	if cause == nil && op.ReleaseID != nil {
		n, err := q.SetActiveRelease(ctx, dbgen.SetActiveReleaseParams{
			OwnerID: op.OwnerID, AppID: op.AppID, ReleaseID: pgUUID(*op.ReleaseID),
		})
		if err != nil || n != 1 {
			if err == nil {
				err = fmt.Errorf("updated %d rows", n)
			}
			return fmt.Errorf("app: activate fenced release: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit operation completion: %w", err)
	}
	return cause
}

func (s *Service) operationApp(ctx context.Context, op Operation) (App, error) {
	row, err := s.q.GetAppByID(ctx, dbgen.GetAppByIDParams{
		OwnerID: op.OwnerID, ID: op.AppID,
	})
	if err != nil {
		return App{}, fmt.Errorf("app: read admitted app: %w", err)
	}
	return toApp(row), nil
}

func (s *Service) operationRelease(ctx context.Context, op Operation) (Release, error) {
	if op.ReleaseID == nil {
		return Release{}, errors.New("app: admitted operation has no release")
	}
	row, err := s.q.GetAppRelease(ctx, dbgen.GetAppReleaseParams{
		OwnerID: op.OwnerID, AppID: op.AppID, ID: *op.ReleaseID,
	})
	if err != nil {
		return Release{}, fmt.Errorf("app: read admitted release: %w", err)
	}
	return toRelease(row), nil
}

func (s *Service) recordBuiltRelease(
	ctx context.Context, op *Operation, release Release,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin built release checkpoint: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	if n, err := q.SetDeploymentRelease(ctx, dbgen.SetDeploymentReleaseParams{
		OwnerID: op.OwnerID, DeploymentID: op.DeploymentID,
		ReleaseID: pgUUID(release.ID),
	}); err != nil || n != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d rows", n)
		}
		return fmt.Errorf("app: link built release: %w", err)
	}
	if op.ClaimToken == nil {
		return ErrOperationClaimLost
	}
	n, err := q.SetClaimedOperationRelease(ctx, dbgen.SetClaimedOperationReleaseParams{
		ReleaseID: pgUUID(release.ID), OwnerID: op.OwnerID, ID: op.ID,
		ExpectedStatus: OperationBuilding, ClaimToken: pgUUID(*op.ClaimToken),
	})
	if err != nil {
		return fmt.Errorf("app: attach built release: %w", err)
	}
	if n != 1 {
		return ErrOperationClaimLost
	}
	row, err := q.TransitionDeploymentOperation(ctx,
		dbgen.TransitionDeploymentOperationParams{
			NextStatus: OperationApplying, OwnerID: op.OwnerID, ID: op.ID,
			ExpectedStatus: OperationBuilding, ClaimToken: pgUUID(*op.ClaimToken),
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOperationClaimLost
		}
		return fmt.Errorf("app: advance built release to apply: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit built release checkpoint: %w", err)
	}
	*op = toOperation(row)
	return nil
}

func (s *Service) buildOperation(ctx context.Context, op *Operation, a App) (App, error) {
	built, err := s.buildIfNeeded(ctx, op.OwnerID, a, op.DeploymentID)
	if err != nil {
		return a, err
	}
	sourceRevision := revisionFor(op.DeploymentID)
	if build, buildErr := s.q.GetBuildForDeployment(ctx,
		dbgen.GetBuildForDeploymentParams{
			OwnerID: op.OwnerID, DeploymentID: op.DeploymentID,
		}); buildErr == nil && build.CommitSha != "" {
		sourceRevision = build.CommitSha
	}
	release, err := s.createRelease(ctx, s.q, built, sourceRevision)
	if err != nil {
		return built, err
	}
	if err := s.recordBuiltRelease(ctx, op, release); err != nil {
		return built, err
	}
	return built, nil
}

func rolloutHealthy(status orchestrator.AppStatus) bool {
	if status.ObservedGeneration < status.Generation {
		return false
	}
	if status.Desired == 0 {
		return status.Ready == 0 && status.Available == 0
	}
	return status.AvailableCondition &&
		status.Updated >= status.Desired &&
		status.Ready >= status.Desired &&
		status.Available >= status.Desired
}

func rolloutReason(status orchestrator.AppStatus) string {
	if status.Reason != "" && status.Message != "" {
		return status.Reason + ": " + status.Message
	}
	if status.Message != "" {
		return status.Message
	}
	if status.Reason != "" {
		return status.Reason
	}
	return fmt.Sprintf("%d/%d replicas updated, %d ready, %d available",
		status.Updated, status.Desired, status.Ready, status.Available)
}

func (s *Service) verifyRollout(
	ctx context.Context, a App, releaseID uuid.UUID, configVersion int64, started time.Time,
) error {
	remaining := time.Until(started.Add(s.opts.RolloutTimeout))
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	verify, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	ticker := time.NewTicker(s.opts.RolloutPollInterval)
	defer ticker.Stop()
	var last orchestrator.AppStatus
	for {
		status, err := s.orch.AppStatus(verify, a.Ref())
		if err == nil {
			last = status
			if convergenceMatches(status, releaseID, configVersion) && rolloutHealthy(status) {
				return nil
			}
			if status.Terminal {
				return fmt.Errorf("app: rollout failed: %s", rolloutReason(status))
			}
		} else if !errors.Is(err, orchestrator.ErrNotFound) {
			return fmt.Errorf("%w: %v", ErrClusterObservation, err)
		}

		select {
		case <-verify.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("app: rollout verification timed out: %s", rolloutReason(last))
		case <-ticker.C:
		}
	}
}

func (s *Service) executeOperation(ctx context.Context, op Operation) error {
	ctx, stopLease := s.maintainOperationLease(ctx, op)
	defer stopLease()
	if op.Status != OperationClaimed || op.Checkpoint != OperationClaimed {
		return ErrOperationRecoveryPending
	}
	a, err := s.operationApp(ctx, op)
	if err != nil {
		_ = s.completeOperation(ctx, op, err)
		return err
	}
	if op.RequiresBuild {
		if err = s.transitionOperation(ctx, &op, OperationBuilding); err == nil {
			a, err = s.buildOperation(ctx, &op, a)
		}
	} else {
		err = s.transitionOperation(ctx, &op, OperationApplying)
	}
	return s.applyAndVerifyOperation(ctx, op, a, err)
}

func (s *Service) applyAndVerifyOperation(
	ctx context.Context, op Operation, a App, stageErr error,
) error {
	err := stageErr
	var release Release
	if err == nil {
		err = s.withAppConvergenceLock(ctx, op.AppID, func() error {
			current, readErr := s.q.GetDeploymentOperation(ctx,
				dbgen.GetDeploymentOperationParams{OwnerID: op.OwnerID, ID: op.ID})
			if readErr != nil {
				return readErr
			}
			if op.ClaimToken == nil || !current.ClaimToken.Valid ||
				uuid.UUID(current.ClaimToken.Bytes) != *op.ClaimToken || current.Status != op.Status {
				return ErrOperationClaimLost
			}
			a, readErr = s.operationApp(ctx, op)
			if readErr != nil {
				return readErr
			}
			release, readErr = s.operationRelease(ctx, op)
			if readErr != nil {
				return readErr
			}
			return s.applyRelease(ctx, s.q, a, &release)
		})
	}
	if err == nil {
		err = s.transitionOperation(ctx, &op, OperationVerifying)
	}
	if err == nil {
		err = s.verifyRollout(ctx, a, release.ID, a.ConfigVersion, op.StageStartedAt)
	}
	if errors.Is(err, ErrClusterObservation) {
		// An API outage says nothing about the rollout. Preserve the verifying
		// checkpoint and let the app reconciler observe it again rather than
		// publishing a wave of false failures.
		if releaseErr := s.releaseRecoverableOperation(ctx, op); releaseErr != nil {
			return releaseErr
		}
		return err
	}
	return s.completeOperation(ctx, op, err)
}

// executeRecoveredBuildOperation starts at applying only for the recovery
// transaction in reconcile.go. General applying-stage recovery lives in the
// app reconciler, which first observes whether ApplyApp already took effect.
func (s *Service) executeRecoveredBuildOperation(ctx context.Context, op Operation) error {
	ctx, stopLease := s.maintainOperationLease(ctx, op)
	defer stopLease()
	if op.Status != OperationApplying || op.Checkpoint != OperationApplying ||
		op.ReleaseID == nil {
		return ErrOperationRecoveryPending
	}
	a, err := s.operationApp(ctx, op)
	if err != nil {
		return s.completeOperation(ctx, op, err)
	}
	return s.applyAndVerifyOperation(ctx, op, a, nil)
}

func (s *Service) executeImageOperation(ctx context.Context, op Operation) error {
	return s.executeOperation(ctx, op)
}

func (s *Service) startClaimedOperation(ctx context.Context, op Operation) {
	go func() {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), deployTimeout)
		defer cancel()
		if err := s.executeOperation(work, op); err != nil &&
			!errors.Is(err, ErrOperationClaimLost) &&
			!errors.Is(err, ErrOperationRecoveryPending) {
			s.log.Warn("admitted deploy failed", "operation_id", op.ID, "error", err)
		}
	}()
}

func (s *Service) startRecoveredBuildOperation(ctx context.Context, op Operation) {
	go func() {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), deployTimeout)
		defer cancel()
		if err := s.executeRecoveredBuildOperation(work, op); err != nil &&
			!errors.Is(err, ErrOperationClaimLost) {
			s.log.Warn("recovered build deploy failed", "operation_id", op.ID, "error", err)
		}
	}()
}

func (s *Service) executeRecoveredAppOperation(ctx context.Context, op Operation) error {
	ctx, stopLease := s.maintainOperationLease(ctx, op)
	defer stopLease()
	if op.ClaimToken == nil ||
		(op.Status != OperationApplying && op.Status != OperationVerifying) {
		return ErrOperationRecoveryPending
	}

	var a App
	var release Release
	err := s.withAppConvergenceLock(ctx, op.AppID, func() error {
		current, readErr := s.q.GetDeploymentOperation(ctx,
			dbgen.GetDeploymentOperationParams{OwnerID: op.OwnerID, ID: op.ID})
		if readErr != nil {
			return readErr
		}
		if !current.ClaimToken.Valid || uuid.UUID(current.ClaimToken.Bytes) != *op.ClaimToken ||
			current.Status != op.Status {
			return ErrOperationClaimLost
		}
		a, readErr = s.operationApp(ctx, op)
		if readErr != nil {
			return readErr
		}
		release, readErr = s.operationRelease(ctx, op)
		if readErr != nil {
			return readErr
		}
		status, observeErr := s.orch.AppStatus(ctx, a.Ref())
		if observeErr != nil && !errors.Is(observeErr, orchestrator.ErrNotFound) {
			return fmt.Errorf("%w: %v", ErrClusterObservation, observeErr)
		}
		if observeErr != nil || !convergenceMatches(status, release.ID, a.ConfigVersion) {
			if applyErr := s.applyRelease(ctx, s.q, a, &release); applyErr != nil {
				return fmt.Errorf("%w: %v", ErrClusterObservation, applyErr)
			}
		}
		if op.Status == OperationApplying {
			return s.transitionOperation(ctx, &op, OperationVerifying)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrClusterObservation) {
			if releaseErr := s.releaseRecoverableOperation(ctx, op); releaseErr != nil {
				return releaseErr
			}
			return err
		}
		return err
	}
	err = s.verifyRollout(ctx, a, release.ID, a.ConfigVersion, op.StageStartedAt)
	if errors.Is(err, ErrClusterObservation) {
		if releaseErr := s.releaseRecoverableOperation(ctx, op); releaseErr != nil {
			return releaseErr
		}
		return err
	}
	return s.completeOperation(ctx, op, err)
}

func (s *Service) startRecoveredAppOperation(ctx context.Context, op Operation) {
	go func() {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), deployTimeout)
		defer cancel()
		if err := s.executeRecoveredAppOperation(work, op); err != nil &&
			!errors.Is(err, ErrOperationClaimLost) &&
			!errors.Is(err, ErrClusterObservation) {
			s.log.Warn("recovered app deploy failed", "operation_id", op.ID, "error", err)
		}
	}()
}

// RunOperationAdmission reclaims expired ownership and promotes durable queued
// work while this process is alive. The claim token fences any worker that
// returns after another process has reclaimed its lease.
func (s *Service) RunOperationAdmission(ctx context.Context) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := s.reclaimOperations(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("operation reclaim paused", "error", err)
		}
		for {
			op, err := s.ClaimOperation(ctx)
			if errors.Is(err, ErrNotFound) {
				break
			}
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					s.log.Warn("operation admission paused", "error", err)
				}
				break
			}
			s.startClaimedOperation(ctx, op)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func toOperation(row dbgen.DeploymentOperation) Operation {
	out := Operation{
		ID: row.ID, OwnerID: row.OwnerID, AppID: row.AppID,
		DeploymentID: row.DeploymentID, RequiresBuild: row.RequiresBuild,
		Status: row.Status, Message: row.Message, CreatedAt: row.CreatedAt,
		Checkpoint: row.Checkpoint, StageStartedAt: row.StageStartedAt,
	}
	if row.ReleaseID.Valid {
		id := uuid.UUID(row.ReleaseID.Bytes)
		out.ReleaseID = &id
	}
	if row.ClaimedAt.Valid {
		v := row.ClaimedAt.Time
		out.ClaimedAt = &v
	}
	if row.FinishedAt.Valid {
		v := row.FinishedAt.Time
		out.FinishedAt = &v
	}
	if row.ClaimToken.Valid {
		v := uuid.UUID(row.ClaimToken.Bytes)
		out.ClaimToken = &v
	}
	if row.LeaseExpiresAt.Valid {
		v := row.LeaseExpiresAt.Time
		out.LeaseExpiresAt = &v
	}
	if row.CancelledAt.Valid {
		v := row.CancelledAt.Time
		out.CancelledAt = &v
	}
	return out
}

func leaseInterval() pgtype.Interval {
	return pgtype.Interval{
		Microseconds: int64(operationLease / time.Microsecond), Valid: true,
	}
}

func pgOptionalUUID(id *uuid.UUID) (out pgtype.UUID) {
	if id != nil {
		out.Bytes, out.Valid = *id, true
	}
	return out
}
