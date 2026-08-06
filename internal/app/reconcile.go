package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/registry"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ReconcileInterval is how often builds are settled against the cluster.
//
// A minute rather than seconds: this exists to catch builds whose process went
// away, which is a rare event, and polling hard for it would cost a query per
// second forever to shorten a wait nobody is sitting through.
const ReconcileInterval = time.Minute

// buildGrace is how long a build may exist before the reconciler will settle
// it on the strength of a missing Job.
//
// Without it there is a race the reconciler would lose: a build row is written
// before the Job is created, so a reconcile landing in that window would see no
// Job, conclude the build had died, and fail a build that was about to start.
const buildGrace = 2 * time.Minute

// ReconcileBuilds settles builds that claim to be running.
//
// Level-triggered, against the cluster, which is the only arrangement that
// works. A build is driven by a goroutine, and a goroutine is not a source of
// truth: it does not survive a restart, and it never existed on the other
// replicas. The Job does, and every replica can read it — so "is this still
// running" is a lookup rather than a guess about a process.
//
// Safe to run concurrently with the goroutine that started the build, and on
// several replicas at once. Everything here is idempotent and decides from the
// cluster's answer, so two reconcilers reaching the same conclusion write the
// same thing.
func (s *Service) ReconcileBuilds(ctx context.Context) error {
	// The build grace is longer than the operation lease. Reclaim first so a
	// finished Job can be attached only to an unowned building checkpoint; a
	// live worker keeps its token and remains the sole writer.
	if err := s.reclaimOperations(ctx); err != nil {
		return err
	}
	if err := s.recoverMissingBuildRows(ctx); err != nil {
		return err
	}
	if s.builder == nil {
		return nil
	}

	rows, err := s.q.ListRunningBuilds(ctx)
	if err != nil {
		return fmt.Errorf("app: list running builds: %w", err)
	}

	var settled int
	for _, row := range rows {
		if time.Since(row.StartedAt) < buildGrace {
			continue
		}

		state, err := s.builder.BuildState(ctx, row.JobName)
		if err != nil {
			// The cluster not answering is not evidence a build died. Left
			// alone: the next pass asks again, and marking it failed here
			// would turn an unreachable API server into a wave of failed
			// deployments.
			s.log.Warn("could not read a build's state",
				slog.String("job", row.JobName), slog.String("error", err.Error()))
			continue
		}
		if state.Found && !state.Done {
			continue
		}

		if s.settleBuild(ctx, row, state) {
			settled++
		}
	}

	if settled > 0 {
		s.log.Info("settled builds that were no longer running",
			slog.Int("count", settled))
	}
	return nil
}

const missingBuildRecoveryBatch = 100

// recoverMissingBuildRows closes the crash window between entering building
// and inserting the build row. Queued is proof the original lease expired; a
// row lock plus a second build-row check decides whether retry is still safe.
// Safe retries return to the ordinary admission queue so its advisory-locked
// global build ceiling remains the sole authority for starting build work.
func (s *Service) recoverMissingBuildRows(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT owner_id, id
		FROM deployment_operations o
		WHERE status = 'queued' AND checkpoint = 'building'
		  AND NOT EXISTS (
		      SELECT 1 FROM builds b WHERE b.deployment_id = o.deployment_id
		  )
		ORDER BY stage_started_at, id
		LIMIT $1`, missingBuildRecoveryBatch)
	if err != nil {
		return fmt.Errorf("app: list missing build rows: %w", err)
	}
	type candidate struct {
		ownerID string
		id      uuid.UUID
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ownerID, &c.id); err != nil {
			rows.Close()
			return fmt.Errorf("app: scan missing build row: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("app: iterate missing build rows: %w", err)
	}
	rows.Close()
	for _, candidate := range candidates {
		err := s.resetMissingBuildRow(ctx, candidate.ownerID, candidate.id)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrOperationRecoveryPending) {
			continue
		}
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errMissingBuildRecoveryStoreUnavailable) {
			return err
		}
		if err != nil {
			// A malformed or concurrently changed candidate must not prevent a
			// healthy row from being normalized, nor stop the independent pass
			// that settles builds which already have durable build rows.
			s.log.Warn("could not reset a missing build checkpoint",
				slog.String("owner_id", candidate.ownerID),
				slog.String("operation_id", candidate.id.String()),
				slog.String("error", err.Error()))
			continue
		}
	}
	return nil
}

var errMissingBuildRecoveryStoreUnavailable = errors.New(
	"app: missing build recovery store unavailable",
)

func (s *Service) resetMissingBuildRow(
	ctx context.Context, ownerID string, id uuid.UUID,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin transaction: %v",
			errMissingBuildRecoveryStoreUnavailable, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var status, checkpoint string
	var deploymentID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT status, checkpoint, deployment_id
		FROM deployment_operations
		WHERE owner_id = $1 AND id = $2
		FOR UPDATE`, ownerID, id).Scan(&status, &checkpoint, &deploymentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("app: lock missing build recovery: %w", err)
	}
	if status != OperationQueued || checkpoint != OperationBuilding {
		return ErrNotFound
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM builds WHERE deployment_id = $1)`, deploymentID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("app: recheck missing build row: %w", err)
	}
	if exists {
		return ErrOperationRecoveryPending
	}
	result, err := tx.Exec(ctx, `
		UPDATE deployment_operations
		SET status = 'queued', checkpoint = 'claimed', claimed_at = NULL,
		    stage_started_at = now(), claim_token = NULL, lease_expires_at = NULL
		WHERE owner_id = $1 AND id = $2
		  AND status = 'queued' AND checkpoint = 'building'`, ownerID, id)
	if err != nil {
		return fmt.Errorf("app: reset missing build checkpoint: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit missing build recovery: %w", err)
	}
	return nil
}

// settleBuild writes the end of a build the cluster says is over. A failed or
// vanished Job is still a failed attempt. A clean Job is different now: the
// deployment-derived tag makes its pushed image discoverable, and manifest
// resolution turns that tag into the digest the old implementation correctly
// said it did not otherwise know.
func (s *Service) settleBuild(
	ctx context.Context, row dbgen.Build, state orchestrator.BuildState,
) bool {
	if state.Found && state.Done && !state.Failed {
		op, err := s.recoverPushedBuild(ctx, row)
		switch {
		case err == nil:
			s.startRecoveredBuildOperation(ctx, op)
			return true
		case errors.Is(err, registry.ErrManifestNotFound):
			message := "the build finished but its deployment-specific image is " +
				"absent from the registry — deploy again to rebuild it"
			return s.failInterruptedBuild(ctx, row, message)
		case errors.Is(err, ErrOperationRecoveryPending):
			return false
		default:
			// Authentication, transport, and cancellation failures say nothing
			// about whether the pushed image exists. Keep both rows recoverable;
			// the next pass uses the same stable tag.
			s.log.Warn("could not resolve a finished build",
				"deployment_id", row.DeploymentID, "error", err)
			return false
		}
	}

	message := ""
	switch {
	case state.Found && state.Failed:
		message = state.Reason
	default:
		message = "the build stopped without finishing, and the job that was " +
			"running it is gone — this usually means Yacht was restarted mid-build"
	}
	return s.failInterruptedBuild(ctx, row, message)
}

// recoverPushedBuild reverses settleBuild's old constraint: the result is
// knowable because the per-deployment tag is stable and the registry can name
// its digest. Registry resolution happens before the transaction; the
// operation row is then locked so only one reconciler records and resumes it.
func (s *Service) recoverPushedBuild(
	ctx context.Context, row dbgen.Build,
) (Operation, error) {
	if s.images == nil || s.manifests == nil {
		return Operation{}, ErrNoManifestResolver
	}
	appRow, err := s.q.GetAppByID(ctx, dbgen.GetAppByIDParams{
		OwnerID: row.OwnerID, ID: row.AppID,
	})
	if err != nil {
		return Operation{}, fmt.Errorf("app: read build recovery app: %w", err)
	}
	image, err := s.images.ImageFor(ctx, row.OwnerID, appRow.Name, revisionFor(row.DeploymentID))
	if err != nil {
		return Operation{}, fmt.Errorf("app: name recovered build image: %w", err)
	}
	digest, err := s.manifests.ResolveDigest(ctx, image)
	if err != nil {
		return Operation{}, fmt.Errorf("app: resolve recovered build %s: %w", image, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, fmt.Errorf("app: begin build recovery: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	lockedOperation, operationErr := q.GetBuildRecoveryOperation(ctx,
		dbgen.GetBuildRecoveryOperationParams{
			OwnerID: row.OwnerID, DeploymentID: row.DeploymentID,
		})
	legacy := errors.Is(operationErr, pgx.ErrNoRows)
	if operationErr != nil && !legacy {
		return Operation{}, fmt.Errorf("app: lock build recovery operation: %w", operationErr)
	}
	if !legacy {
		if lockedOperation.Status != OperationQueued ||
			lockedOperation.Checkpoint != OperationBuilding {
			return Operation{}, ErrOperationRecoveryPending
		}
	}

	lockedApp, err := q.GetAppForReleaseBackfill(ctx, dbgen.GetAppForReleaseBackfillParams{
		OwnerID: row.OwnerID, AppID: row.AppID,
	})
	if err != nil {
		return Operation{}, fmt.Errorf("app: lock build recovery app: %w", err)
	}
	if lockedApp.Image != image {
		lockedApp, err = q.SetAppImage(ctx, dbgen.SetAppImageParams{
			OwnerID: row.OwnerID, ID: row.AppID, Image: image,
		})
		if err != nil {
			return Operation{}, fmt.Errorf("app: record recovered image: %w", err)
		}
	}
	release, err := s.recordRelease(ctx, q, toApp(lockedApp),
		revisionFor(row.DeploymentID), digest, "deployment")
	if err != nil {
		return Operation{}, err
	}
	if legacy {
		if _, err := s.enqueueOperation(ctx, q, row.OwnerID, row.AppID,
			row.DeploymentID, &release.ID, true); err != nil {
			return Operation{}, err
		}
	}
	if n, err := q.SetDeploymentRelease(ctx, dbgen.SetDeploymentReleaseParams{
		OwnerID: row.OwnerID, DeploymentID: row.DeploymentID,
		ReleaseID: pgUUID(release.ID),
	}); err != nil || n != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d rows", n)
		}
		return Operation{}, fmt.Errorf("app: link recovered release: %w", err)
	}
	if _, err := q.FinishBuild(ctx, dbgen.FinishBuildParams{
		ID: row.ID, Status: BuildSucceeded, Image: image,
	}); err != nil {
		return Operation{}, fmt.Errorf("app: record recovered build: %w", err)
	}
	opRow, err := q.RecoverBuiltDeploymentOperation(ctx,
		dbgen.RecoverBuiltDeploymentOperationParams{
			ReleaseID: pgUUID(release.ID), LeaseDuration: leaseInterval(),
			OwnerID: row.OwnerID, DeploymentID: row.DeploymentID,
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, ErrOperationRecoveryPending
		}
		return Operation{}, fmt.Errorf("app: claim recovered build: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, fmt.Errorf("app: commit build recovery: %w", err)
	}
	return toOperation(opRow), nil
}

func (s *Service) failInterruptedBuild(
	ctx context.Context, row dbgen.Build, message string,
) bool {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("begin failed build settlement", "error", err)
		return false
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	op, opErr := q.GetBuildRecoveryOperation(ctx, dbgen.GetBuildRecoveryOperationParams{
		OwnerID: row.OwnerID, DeploymentID: row.DeploymentID,
	})
	if opErr != nil && !errors.Is(opErr, pgx.ErrNoRows) {
		s.log.Error("lock failed build operation", "error", opErr)
		return false
	}
	if opErr == nil && (op.Status != OperationQueued || op.Checkpoint != OperationBuilding) {
		return false
	}

	if _, err := q.FinishBuild(ctx, dbgen.FinishBuildParams{
		ID: row.ID, Status: BuildFailed, Message: message,
	}); err != nil {
		s.log.Error("settle build", slog.String("error", err.Error()))
		return false
	}
	if opErr == nil {
		n, err := q.FailQueuedBuildOperation(ctx, dbgen.FailQueuedBuildOperationParams{
			Message: message, OwnerID: row.OwnerID, DeploymentID: row.DeploymentID,
		})
		if err != nil || n != 1 {
			return false
		}
	}
	if _, err := q.FinishDeployment(ctx, dbgen.FinishDeploymentParams{
		OwnerID: row.OwnerID, ID: row.DeploymentID,
		Status: DeployFailed, Message: message,
	}); err != nil {
		s.log.Error("settle build deployment", "error", err)
		return false
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("commit failed build settlement", "error", err)
		return false
	}
	return true
}

// RunReconciler settles builds until the context is cancelled.
//
// Runs once immediately, because the case this exists for is a restart, and
// waiting a full interval to notice would leave every interrupted build
// claiming to run for that long after the process came back.
func (s *Service) RunReconciler(ctx context.Context) {
	if s.builder == nil {
		return
	}

	tick := time.NewTicker(ReconcileInterval)
	defer tick.Stop()

	for {
		if err := s.ReconcileBuilds(ctx); err != nil {
			s.log.Warn("reconcile builds", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
