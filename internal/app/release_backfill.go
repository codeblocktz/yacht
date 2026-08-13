package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/codeblocktz/yacht/internal/registry"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

const (
	BaselinePending          = "pending"
	BaselineReady            = "ready"
	BaselinePendingImage     = "pending_image"
	BaselineNeverDeployed    = "never_deployed"
	BaselineImageUnavailable = "image_unavailable"
	BaselineBlocked          = "blocked"
)

// BackfillReleases resolves and records one bounded batch of legacy apps. It
// performs no cluster calls and is safe to run repeatedly or from several
// replicas: the app row is locked and the release/pointer/state commit is one
// transaction.
func (s *Service) BackfillReleases(ctx context.Context) error {
	if s.manifests == nil {
		return ErrNoManifestResolver
	}
	rows, err := s.q.ListPendingReleaseBackfills(ctx, 25)
	if err != nil {
		return fmt.Errorf("app: list release backfills: %w", err)
	}
	for _, row := range rows {
		digest, resolveErr := s.manifests.ResolveDigest(ctx, row.App.Image)
		if resolveErr != nil {
			if errors.Is(resolveErr, context.Canceled) ||
				errors.Is(resolveErr, context.DeadlineExceeded) {
				return resolveErr
			}
			state := BaselineBlocked
			if errors.Is(resolveErr, registry.ErrManifestNotFound) {
				state = BaselineImageUnavailable
			} else if errors.Is(resolveErr, registry.ErrUnreachable) {
				state = BaselinePending
			}
			if _, err := s.q.SetReleaseBackfillState(ctx,
				dbgen.SetReleaseBackfillStateParams{
					OwnerID: row.App.OwnerID, AppID: row.App.ID,
					State: state, LastError: resolveErr.Error(),
				}); err != nil {
				return fmt.Errorf("app: record release backfill failure: %w", err)
			}
			continue
		}
		if err := s.commitBackfill(ctx, row.App, digest); err != nil {
			return err
		}
	}

	pending, err := s.q.CountPendingReleaseBackfills(ctx)
	if err != nil {
		return fmt.Errorf("app: count pending release backfills: %w", err)
	}
	if pending == 0 {
		if _, err := s.q.NormalizeLegacyDeploymentStatuses(ctx); err != nil {
			return fmt.Errorf("app: normalize legacy deployment statuses: %w", err)
		}
	}
	return nil
}

func (s *Service) commitBackfill(ctx context.Context, observed dbgen.App, digest string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin release backfill: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	locked, err := q.GetAppForReleaseBackfill(ctx, dbgen.GetAppForReleaseBackfillParams{
		OwnerID: observed.OwnerID, AppID: observed.ID,
	})
	if err != nil {
		return fmt.Errorf("app: lock release backfill: %w", err)
	}
	// Resolution happened outside the transaction. Never bind it to state that
	// changed while the registry was being asked; the next pass resolves again.
	if locked.ActiveReleaseID.Valid || locked.Image != observed.Image ||
		locked.ConfigVersion != observed.ConfigVersion {
		return nil
	}

	a := toApp(locked)
	release, err := s.recordRelease(ctx, q, a, "", digest, "backfill")
	if err != nil {
		return err
	}
	if n, err := q.SetActiveRelease(ctx, dbgen.SetActiveReleaseParams{
		OwnerID: a.OwnerID, AppID: a.ID, ReleaseID: pgUUID(release.ID),
	}); err != nil || n != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d rows", n)
		}
		return fmt.Errorf("app: activate backfilled release: %w", err)
	}
	if n, err := q.SetReleaseBackfillState(ctx, dbgen.SetReleaseBackfillStateParams{
		OwnerID: a.OwnerID, AppID: a.ID, State: BaselineReady,
	}); err != nil || n != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d rows", n)
		}
		return fmt.Errorf("app: finish release backfill: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit release backfill: %w", err)
	}
	return nil
}

// RunReleaseBackfill retries registry failures without holding up startup.
// Transient failures retry quickly; missing images and configuration failures
// remain visible and retry on the slower schedule stored with the cohort row.
func (s *Service) RunReleaseBackfill(ctx context.Context) {
	run := func() {
		if err := s.BackfillReleases(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("release backfill paused", slog.String("error", err.Error()))
		}
	}
	run()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			run()
		}
	}
}
