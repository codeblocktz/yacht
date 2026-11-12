package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Erasing a volume: its data deleted, everything else about the app kept.
//
// DeleteVolume detaches, and never deletes a claim: storage destroyed as a
// side effect of an edit cannot be got back. Erasing is the deliberate
// opposite, for whoever decides data is to go — a wrapper keeping data only
// so long, say — while the app, its settings, variables, domains, history
// and the volume's own definition stay as they are. Not on the dashboard or
// the API: there is no undo, and nothing a person does from a page needs it
// that deleting the app does not do.

// ErrVolumeInUse means the volume's data cannot be erased while something
// runs that mounts it: the app must be stopped (scaled to zero, not only
// asleep), its pods gone, and no deploy of it in flight.
var ErrVolumeInUse = errors.New("app: the volume is in use — stop the app, and let its pods finish, before erasing it")

// ErrVolumeRetained means the cluster keeps a volume's data when its claim
// is deleted, so erasing it would delete nothing.
var ErrVolumeRetained = errors.New("app: this cluster keeps a volume's data after it is deleted, so it cannot be erased here")

// EraseVolume deletes the data on one of an app's volumes. The volume stays
// attached, as it was: the app's next start is given a new, empty one of the
// same size at the same path, and until then a stopped app has none. Erasing
// a volume already erased does nothing.
//
// A database is a workload like any other here, and its volume is its data
// directory: the next start initialises an empty database with the
// credentials it had, so what refers to it still connects — to nothing. Its
// backups are not on its volume and are not erased; Backups and DeleteBackup
// are for those.
func (s *Service) EraseVolume(ctx context.Context, ownerID, appName, volumeName string) error {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}
	found := false
	for _, v := range a.Volumes {
		found = found || v.Name == volumeName
	}
	if !found {
		return ErrVolumeNotFound
	}
	eraser, ok := s.orch.(orchestrator.VolumeEraser)
	if !ok {
		return fmt.Errorf("app: erase %s's volume %s: %w", appName, volumeName, orchestrator.ErrNotSupported)
	}

	// Under the app's convergence lock, so no reconcile, deploy or wake
	// applies it — and makes a claim — while it is checked and erased.
	return s.withAppConvergenceLock(ctx, a.ID, func() error {
		row, err := s.q.GetAppByID(ctx, dbgen.GetAppByIDParams{OwnerID: ownerID, ID: a.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: re-read app to erase: %w", err)
		}
		a = toApp(row)
		if a.Replicas > 0 {
			return ErrVolumeInUse
		}
		if _, err := s.liveOperation(ctx, a); err == nil {
			return ErrVolumeInUse
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		switch err := eraser.EraseVolume(ctx, a.Ref(), volumeName); {
		case errors.Is(err, orchestrator.ErrVolumeInUse):
			return fmt.Errorf("%w (%v)", ErrVolumeInUse, err)
		case errors.Is(err, orchestrator.ErrVolumeRetained):
			return fmt.Errorf("%w (%v)", ErrVolumeRetained, err)
		case err != nil:
			return fmt.Errorf("app: erase %s's volume %s: %w", appName, volumeName, err)
		}
		s.log.Warn("volume erased: its data is deleted",
			slog.String("owner", ownerID), slog.String("app", appName), slog.String("volume", volumeName))
		return nil
	})
}
