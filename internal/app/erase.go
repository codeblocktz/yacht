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

// Erasing a volume: the claim behind it deleted, everything else about the
// app kept.
//
// DeleteVolume detaches, and never deletes a claim: storage destroyed as a
// side effect of an edit cannot be got back. Erasing is the deliberate
// opposite, for whoever decides data is to go — a wrapper keeping data only
// so long, say — while the app, its settings, variables, domains, history
// and the volume's own definition stay as they are. Not on the dashboard or
// the API: there is no undo, and nothing a person does from a page needs it
// that deleting the app does not do.

// ErrVolumeInUse means the volume's data cannot be erased while anything
// runs, or may start, that mounts it: the app must be stopped — scaled to
// zero and running a release of zero, not only asleep — with no deploy in
// flight and its pods gone.
var ErrVolumeInUse = errors.New("app: the volume is in use — stop the app, and let its pods finish, before erasing it")

// ErrVolumeRetained means the cluster keeps a volume's data when its claim
// is deleted, so erasing it would delete nothing.
var ErrVolumeRetained = errors.New("app: this cluster keeps a volume's data after its claim is deleted, so it cannot be erased here")

// ErrVolumeReserved means the name is not a volume's: "backups" is the claim
// a database's backups are kept on when there is no bucket for them.
var ErrVolumeReserved = errors.New(`app: "backups" is where a database's backups are kept, not a volume to attach or erase`)

// EraseVolume deletes the claim behind one of an app's volumes; with reclaim
// policy Delete, the storage class then deletes the data. The volume stays
// attached, as it was: the app's next start is given a new, empty one of the
// same size at the same path, and until then a stopped app has none. A
// volume detached with DeleteVolume, whose claim was kept, can be erased by
// its name too. Erasing what is already erased does nothing.
//
// Whether the data is gone is the storage's to say, not the engine's: a
// claim deleted from local-path storage whose node is down, a statically
// provisioned volume, or NFS with archiveOnDelete can keep it. A volume whose
// class says Retain is refused (ErrVolumeRetained).
//
// A database is a workload like any other here, and its volume is its data
// directory: the next start initialises an empty database with the
// credentials it had, so what refers to it still connects — to nothing. Its
// backups are not on its volume and are not erased; Backups and DeleteBackup
// are for those.
func (s *Service) EraseVolume(ctx context.Context, ownerID, appName, volumeName string) error {
	if volumeName == backupVolume {
		return ErrVolumeReserved
	}
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}
	attached := false
	for _, v := range a.Volumes {
		attached = attached || v.Name == volumeName
	}
	eraser, ok := s.orch.(orchestrator.VolumeEraser)
	if !ok {
		return fmt.Errorf("app: erase %s's volume %s: %w", appName, volumeName, orchestrator.ErrNotSupported)
	}
	log := s.log.With(slog.String("owner", ownerID), slog.String("app", appName), slog.String("volume", volumeName))

	// Decided and the delete issued under the app's convergence lock, so no
	// reconcile, deploy or wake applies it — and mounts the claim — in
	// between. Waiting for the claim to go is not: a claim slow to go holds
	// up this call, not every other app's convergence.
	var erasure orchestrator.VolumeErasure
	err = s.withAppConvergenceLock(ctx, a.ID, func() error {
		row, err := s.q.GetAppByID(ctx, dbgen.GetAppByIDParams{OwnerID: ownerID, ID: a.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: re-read app to erase: %w", err)
		}
		a = toApp(row)
		if attached {
			if err := s.stoppedForErase(ctx, a); err != nil {
				return err
			}
		}
		erasure, err = eraser.EraseVolume(ctx, a.Ref(), volumeName)
		switch {
		case errors.Is(err, orchestrator.ErrVolumeInUse):
			return fmt.Errorf("%w (%v)", ErrVolumeInUse, err)
		case errors.Is(err, orchestrator.ErrVolumeRetained):
			return fmt.Errorf("%w (%v)", ErrVolumeRetained, err)
		case err != nil:
			return fmt.Errorf("app: erase %s's volume %s: %w", appName, volumeName, err)
		}
		return nil
	})
	if errors.Is(err, ErrVolumeInUse) || errors.Is(err, ErrVolumeRetained) {
		log.Info("volume not erased", slog.String("reason", err.Error()))
		return err
	}
	if err != nil {
		return err
	}
	if !erasure.Deleted {
		if !attached {
			return ErrVolumeNotFound
		}
		log.Info("volume not erased: it has no claim, so nothing to erase")
		return nil
	}

	log = log.With(slog.String("claim", erasure.Claim), slog.String("uid", erasure.UID),
		slog.String("persistent_volume", erasure.PersistentVolume))
	if err := eraser.WaitVolumeGone(ctx, a.Ref(), volumeName); err != nil {
		log.Warn("volume erase: its claim is being deleted, and is not gone yet", slog.String("error", err.Error()))
		return fmt.Errorf("app: erase %s's volume %s: %w", appName, volumeName, err)
	}
	log.Warn("volume erased: its claim is deleted, and the storage class deletes the data (with reclaim policy Delete)")
	return nil
}

// stoppedForErase refuses an app that runs, or may start, pods mounting its
// volumes. The app row is not enough: it says what was asked for, and a
// scale to zero whose deploy failed leaves the release still running its
// replicas — at none while the app sleeps, and all of them at its next
// request.
func (s *Service) stoppedForErase(ctx context.Context, a App) error {
	if a.Replicas > 0 || a.Sleep.State != SleepAwake {
		return ErrVolumeInUse
	}
	if a.ActiveReleaseID != nil {
		r, err := s.ReleaseByID(ctx, a.OwnerID, a.ID, *a.ActiveReleaseID)
		if err != nil {
			return err
		}
		if r.Replicas > 0 {
			return ErrVolumeInUse
		}
	}
	if _, err := s.liveOperation(ctx, a); err == nil {
		return ErrVolumeInUse
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}
