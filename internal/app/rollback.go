package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/registry"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Rollback refusals. Each is something a person can act on, so each is its own
// error rather than one "cannot roll back" with the reason in a string.
var (
	// ErrReleaseActive means the release asked for is the one already running.
	ErrReleaseActive = errors.New("app: that release is the one running now")

	// ErrReleaseImageGone means the registry no longer has the release's image.
	// Nothing is changed: an operation that could only fail at the image pull
	// would leave the app's settings rolled back and its workload not.
	ErrReleaseImageGone = errors.New(
		"app: the image this release ran is no longer in the registry, so it cannot be restored")
)

// RollbackTrigger is the deployment trigger, and revision, a rollback records.
const RollbackTrigger = "rollback"

// Rollback puts an app back on one of its earlier releases.
//
// Two things happen, in one transaction: the app's release-owned settings —
// image, replicas, port, limits, health check and plain variables — become the
// release's again, and a deployment of exactly that release is admitted. The
// first is what makes a rollback stay rolled back. Were only the workload
// changed, the next scale or variable edit would build a release from the
// newer settings still on the app and quietly roll forward.
//
// Current state is kept, because a release never held it: hostnames, HTTPS,
// storage, the repository and every secret stay as they are now. A secret is
// deliberately not reverted even where the release predates it — the value was
// never in the release, and deleting it would not restore whatever it replaced.
//
// The image is checked before anything is written. A release whose image the
// registry has since dropped cannot be restored, and saying so here is better
// than rolling the app's settings back behind a deployment that fails to pull.
func (s *Service) Rollback(ctx context.Context, ownerID, name string, releaseID uuid.UUID) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if a.ActiveReleaseID != nil && *a.ActiveReleaseID == releaseID {
		return ErrReleaseActive
	}
	release, err := s.ReleaseByID(ctx, ownerID, a.ID, releaseID)
	if err != nil {
		return err
	}
	if release.Source != a.Source {
		return fmt.Errorf("app: that release was built from %s and %s is now built from %s",
			release.Source, a.Name, a.Source)
	}
	// The same rule Scale holds: a volume mounts on one node at a time.
	if release.Replicas > 1 && len(a.Volumes) > 0 {
		return fmt.Errorf("app: that release ran %d replicas, and %s now has storage attached, "+
			"so it runs one — detach its volumes to roll back to it", release.Replicas, a.Name)
	}
	if err := s.checkReleaseImage(ctx, release); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin rollback: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	row, err := q.RestoreAppFromRelease(ctx, dbgen.RestoreAppFromReleaseParams{
		OwnerID: ownerID, AppID: a.ID, ReleaseID: release.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("app: restore settings from release: %w", err)
	}
	restored := toApp(row)
	if err := s.restoreVariables(ctx, q, ownerID, restored, release.Env); err != nil {
		return err
	}
	// A live operation refuses admission here, which abandons the whole
	// transaction: the settings are not rolled back behind a deploy that never
	// got to run.
	if _, err := s.admitDeploymentTx(ctx, q, ownerID, restored, RollbackTrigger, release.ID, false); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit rollback: %w", err)
	}

	s.log.Info("rollback admitted", slog.String("app", name),
		slog.String("release", release.ID.String()), slog.String("image", release.ImageRef))
	return nil
}

// checkReleaseImage asks the registry whether a release's pinned image is still
// there, where the install can ask. One that cannot is let through: the
// rollout's own verification will report a pull that fails, which is no worse
// than a deploy of any other image this install cannot inspect.
func (s *Service) checkReleaseImage(ctx context.Context, release Release) error {
	avail, ok := s.manifests.(ManifestAvailability)
	if !ok {
		return nil
	}
	err := avail.CheckDigest(ctx, pinImage(release.ImageRef, release.ImageDigest))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, registry.ErrManifestNotFound):
		return ErrReleaseImageGone
	}
	return fmt.Errorf("app: check the release's image is still available: %w", err)
}

// restoreVariables makes an app's plain variables the release's.
//
// Plain ones only. A key that is secret now stays secret and keeps its current
// value even where the release had it in the clear: the secret is the more
// recent decision about that value, and writing it back out as plain text
// would undo it.
func (s *Service) restoreVariables(
	ctx context.Context, q *dbgen.Queries, ownerID string, a App, env map[string]string,
) error {
	rows, err := q.ListVariablesForApp(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("app: list variables for rollback: %w", err)
	}
	current := make(map[string]dbgen.Variable, len(rows))
	for _, row := range rows {
		current[row.Key] = row
		if row.Secret {
			continue
		}
		if _, keep := env[row.Key]; keep {
			continue
		}
		if _, err := q.DeleteVariable(ctx, dbgen.DeleteVariableParams{
			OwnerID: ownerID, AppID: a.ID, Key: row.Key,
		}); err != nil {
			return fmt.Errorf("app: remove %s for rollback: %w", row.Key, err)
		}
		if err := q.ReplaceAppLinks(ctx, dbgen.ReplaceAppLinksParams{
			FromAppID: a.ID, ViaKey: row.Key,
		}); err != nil {
			return fmt.Errorf("app: clear links for %s: %w", row.Key, err)
		}
	}
	for key, value := range env {
		if cur, ok := current[key]; ok && (cur.Secret || cur.Value == value) {
			continue
		}
		if _, err := q.UpsertVariable(ctx, dbgen.UpsertVariableParams{
			OwnerID: ownerID, AppID: a.ID, Key: key, Value: value,
		}); err != nil {
			return fmt.Errorf("app: restore %s for rollback: %w", key, err)
		}
		// The canvas edge follows the value, as it does for any edit.
		if err := s.recordLinks(ctx, q, ownerID, a, key, value); err != nil {
			return err
		}
	}
	return nil
}
