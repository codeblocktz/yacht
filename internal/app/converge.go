package app

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// AppReconcileInterval is short because a steady pass normally performs one
// read per app and no writes. The database convergence key, not this interval,
// decides whether Kubernetes is touched.
const AppReconcileInterval = 2 * time.Second

// withAppConvergenceLock serializes cluster writes for one app across Yacht
// replicas. A transaction lock is too short: ApplyApp is a network operation,
// and releasing the database transaction before it returns lets an old active
// reconcile race and overwrite a candidate deployment.
func (s *Service) withAppConvergenceLock(
	ctx context.Context, appID uuid.UUID, fn func() error,
) error {
	key := int64(binary.BigEndian.Uint64(appID[:8]))
	return s.withSessionLock(ctx, key, "convergence of app "+appID.String(), fn)
}

func convergenceMatches(status orchestrator.AppStatus, releaseID uuid.UUID, version int64) bool {
	return status.ReleaseID == releaseID.String() && status.ConfigVersion == version
}

// ReconcileApps performs one deterministic convergence pass. It is exported
// so tests and repair tooling can run one pass without starting a background
// clock.
func (s *Service) ReconcileApps(ctx context.Context) error {
	if err := s.reclaimOperations(ctx); err != nil {
		return err
	}
	rows, err := s.q.ListAppsForReconciliation(ctx)
	if err != nil {
		return fmt.Errorf("app: list apps for reconciliation: %w", err)
	}
	for _, row := range rows {
		if err := s.reconcileApp(ctx, toApp(row)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Warn("app convergence paused",
				slog.String("app", row.Name), slog.String("error", err.Error()))
		}
	}
	return nil
}

func (s *Service) reconcileApp(ctx context.Context, a App) error {
	op, err := s.liveOperation(ctx, a)
	if err == nil {
		if op.Status == OperationQueued &&
			(op.Checkpoint == OperationApplying || op.Checkpoint == OperationVerifying) {
			claimed, claimErr := s.claimRecoverableOperation(ctx, op)
			if claimErr == nil {
				s.startRecoveredAppOperation(ctx, claimed)
				return nil
			}
			if !errors.Is(claimErr, ErrNotFound) {
				return claimErr
			}
		}
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}

	return s.withAppConvergenceLock(ctx, a.ID, func() error {
		// Both facts are re-read after acquiring the cross-replica lock. An
		// operation admitted while this reconciler waited must win.
		row, readErr := s.q.GetAppByID(ctx, dbgen.GetAppByIDParams{
			OwnerID: a.OwnerID, ID: a.ID,
		})
		if readErr != nil {
			if errors.Is(readErr, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("app: re-read app for convergence: %w", readErr)
		}
		a = toApp(row)
		if _, liveErr := s.liveOperation(ctx, a); liveErr == nil {
			return nil
		} else if !errors.Is(liveErr, ErrNotFound) {
			return liveErr
		}
		if a.ActiveReleaseID == nil {
			return nil
		}

		status, observeErr := s.orch.AppStatus(ctx, a.Ref())
		if observeErr == nil && convergenceMatches(status, *a.ActiveReleaseID, a.ConfigVersion) {
			return s.markVerifiedDomainsRouted(ctx, a)
		}
		if observeErr != nil && !errors.Is(observeErr, orchestrator.ErrNotFound) {
			return fmt.Errorf("app: observe active workload: %w", observeErr)
		}
		return s.apply(ctx, s.q, a)
	})
}

func (s *Service) markVerifiedDomainsRouted(ctx context.Context, a App) error {
	hasVerified, err := s.q.HasVerifiedDomainsForApp(ctx,
		dbgen.HasVerifiedDomainsForAppParams{OwnerID: a.OwnerID, AppID: a.ID})
	if err != nil {
		return fmt.Errorf("app: inspect verified domains: %w", err)
	}
	if !hasVerified {
		return nil
	}
	if _, err := s.q.MarkVerifiedDomainsRoutedForApp(ctx,
		dbgen.MarkVerifiedDomainsRoutedForAppParams{OwnerID: a.OwnerID, AppID: a.ID}); err != nil {
		return fmt.Errorf("app: mark converged domains routed: %w", err)
	}
	return nil
}

func (s *Service) liveOperation(ctx context.Context, a App) (Operation, error) {
	row, err := s.q.GetLiveDeploymentOperationForApp(ctx,
		dbgen.GetLiveDeploymentOperationForAppParams{OwnerID: a.OwnerID, AppID: a.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, ErrNotFound
		}
		return Operation{}, fmt.Errorf("app: read live operation: %w", err)
	}
	return toOperation(row), nil
}

func (s *Service) claimRecoverableOperation(ctx context.Context, op Operation) (Operation, error) {
	row, err := s.q.ClaimRecoverableDeploymentOperation(ctx,
		dbgen.ClaimRecoverableDeploymentOperationParams{
			LeaseDuration: leaseInterval(), OwnerID: op.OwnerID, ID: op.ID,
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, ErrNotFound
		}
		return Operation{}, fmt.Errorf("app: claim recoverable operation: %w", err)
	}
	return toOperation(row), nil
}

func (s *Service) releaseRecoverableOperation(ctx context.Context, op Operation) error {
	if op.ClaimToken == nil {
		return ErrOperationClaimLost
	}
	n, err := s.q.ReleaseRecoverableDeploymentOperation(ctx,
		dbgen.ReleaseRecoverableDeploymentOperationParams{
			OwnerID: op.OwnerID, ID: op.ID, ExpectedStatus: op.Status,
			ClaimToken: pgUUID(*op.ClaimToken),
		})
	if err != nil {
		return fmt.Errorf("app: release recoverable operation: %w", err)
	}
	if n != 1 {
		return ErrOperationClaimLost
	}
	return nil
}

// RunAppReconciler restores missing/drifted active workloads and resumes the
// post-build stages whose worker disappeared.
func (s *Service) RunAppReconciler(ctx context.Context) {
	tick := time.NewTicker(AppReconcileInterval)
	defer tick.Stop()
	for {
		if err := s.ReconcileApps(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("app reconciliation paused", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
