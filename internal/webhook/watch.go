package webhook

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// WatchInterval is how often the apps somebody is listening to are looked at
// for crashing. A minute: an app that has been crash-looping for that long is
// worth a message, and asking the cluster about every watched app more often
// than that costs more than the minute is worth.
const WatchInterval = time.Minute

// WatchCrashes tells app.crashed webhooks when an app starts failing — its
// containers crashing on every start — and remembers that it was, so it is
// said once rather than every minute. Only apps in a project with a webhook
// listening are looked at.
//
// Safe on every replica: whether an app was failing is a row, flipped by an
// update that only one of them can win.
func (s *Service) WatchCrashes(ctx context.Context) {
	if s == nil || s.orch == nil {
		return
	}
	tick := time.NewTicker(WatchInterval)
	defer tick.Stop()
	for {
		s.CheckCrashes(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// CheckCrashes looks once. Exported for tests.
func (s *Service) CheckCrashes(ctx context.Context) {
	rows, err := s.q.AppsWatchedForCrashes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("list apps to watch for crashes", slog.String("error", err.Error()))
		}
		return
	}
	for _, r := range rows {
		status, err := s.orch.AppStatus(ctx, orchestrator.Ref{
			Owner: orchestrator.OwnerID(r.OwnerID), Namespace: r.Namespace, Name: r.Name,
		})
		if err != nil {
			// Not there, or the cluster did not answer: neither says the app
			// is crashing, and neither says it has stopped.
			continue
		}
		// The version the cluster is running may be a deploy's that never
		// became active: a redeploy whose new version could not start. It
		// never served, and its deploy has said it failed, so it is not an
		// app that started crashing.
		serving := status.ReleaseID == "" || status.ReleaseID == r.ActiveReleaseID.String()
		failing := serving && status.Phase == orchestrator.PhaseFailing
		changed, err := s.q.SetAppFailing(ctx, dbgen.SetAppFailingParams{
			AppID: r.ID, OwnerID: r.OwnerID, Failing: failing,
		})
		if err != nil {
			// No row: it was already recorded as it is. Anything else is a
			// fault worth a line, and the next look tries again.
			if !errors.Is(err, pgx.ErrNoRows) {
				s.log.Warn("record whether an app is failing", slog.String("error", err.Error()))
			}
			continue
		}
		if !changed.Failing {
			continue
		}
		var project uuid.UUID
		if r.ProjectID.Valid {
			project = uuid.UUID(r.ProjectID.Bytes)
		}
		msg := status.Message
		if status.Reason != "" && msg != "" {
			msg = status.Reason + ": " + msg
		}
		s.Emit(ctx, app.Event{
			Kind: app.EventAppCrashed, OwnerID: r.OwnerID, ProjectID: project,
			AppID: r.ID, AppName: r.Name, Message: msg, At: time.Now(),
		})
	}
}
