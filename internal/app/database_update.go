package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// DatabaseUpdateTriggerPrefix starts the trigger of a deployment the Update
// button made, followed by what it updated to: "update:PostgreSQL 16.16".
const DatabaseUpdateTriggerPrefix = "update:"

// ErrNoDatabaseUpdate is Update pressed on a database with nothing newer in
// its series to move to: the page was older than what it showed, or the
// update has already been taken.
var ErrNoDatabaseUpdate = errors.New("app: this database is already on the newest image of its version")

// UpdateDatabase moves a database to the newer image of its series that
// DatabaseRelease offers, worked out again here rather than taken from the
// page: what deploys is what the registry says now.
//
// A database that can be backed up is backed up first, and the update
// deploys when the backup succeeds: the returned update's Backup says which
// happened. Either way it deploys as a redeploy does, through the readiness
// gate and onto the same volume, and a release that never becomes ready
// leaves the one before it serving.
func (s *Service) UpdateDatabase(ctx context.Context, ownerID, name, actor string) (DatabaseUpdate, error) {
	rel, err := s.DatabaseRelease(ctx, ownerID, name)
	if err != nil {
		return DatabaseUpdate{}, err
	}
	if rel.Update == nil {
		return DatabaseUpdate{}, ErrNoDatabaseUpdate
	}
	u := *rel.Update
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return DatabaseUpdate{}, err
	}
	if u.Backup {
		if _, err := s.startBackup(ctx, a, BackupBeforeUpdate, actor, &u); err != nil {
			return DatabaseUpdate{}, err
		}
		s.log.Info("database update waits on a backup",
			slog.String("app", a.Name), slog.String("owner", ownerID), slog.String("to", u.Label))
		return u, nil
	}
	return u, s.applyDatabaseUpdate(ctx, a, u)
}

// applyDatabaseUpdate deploys an update. An image that changes is written
// first, as a change in Settings would be, and put back when the deploy is
// refused before it starts, so a refused update leaves nothing behind.
func (s *Service) applyDatabaseUpdate(ctx context.Context, a App, u DatabaseUpdate) error {
	before := a.Image
	if u.Image != a.Image {
		if _, err := s.q.SetAppImage(ctx, dbgen.SetAppImageParams{
			OwnerID: a.OwnerID, ID: a.ID, Image: u.Image,
		}); err != nil {
			return fmt.Errorf("app: record the updated image: %w", err)
		}
		a.Image = u.Image
	}
	err := s.deployCurrentRelease(ctx, a.OwnerID, a, DatabaseUpdateTriggerPrefix+u.Label)
	if err != nil && a.Image != before {
		if _, rerr := s.q.SetAppImage(ctx, dbgen.SetAppImageParams{
			OwnerID: a.OwnerID, ID: a.ID, Image: before,
		}); rerr != nil {
			s.log.Warn("could not put a refused update's image back",
				slog.String("app", a.Name), slog.String("error", rerr.Error()))
		}
	}
	if err == nil {
		s.log.Info("database update deploying",
			slog.String("app", a.Name), slog.String("owner", a.OwnerID), slog.String("to", u.Label))
	}
	return err
}

// afterUpdateBackup deploys the update a backup was taken for, once it has
// succeeded. A backup that failed deploys nothing — an update is not taken
// without the backup it was promised — and says so in the deployments list,
// where the person who pressed Update looks for it.
func (s *Service) afterUpdateBackup(ctx context.Context, ownerID, name string, u DatabaseUpdate, status, reason string) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return
	}
	var cause error
	if status == BackupSucceeded {
		if cause = s.applyDatabaseUpdate(ctx, a, u); cause == nil {
			return
		}
	} else {
		cause = fmt.Errorf("the backup taken first failed, so nothing was changed: %s", reason)
	}
	s.log.Warn("database update not deployed",
		slog.String("app", a.Name), slog.String("to", u.Label), slog.String("error", cause.Error()))
	// Nobody is on the page to be told, and a deploy refused before it was
	// admitted has no row of its own: this is it.
	a.Image = u.Image
	id := s.beginDeployment(ctx, ownerID, a, DatabaseUpdateTriggerPrefix+u.Label)
	_ = s.endDeployment(ctx, ownerID, a.ID, uuid.Nil, id, cause)
}
