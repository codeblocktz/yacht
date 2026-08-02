package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
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

		s.settleBuild(ctx, row, state)
		settled++
	}

	if settled > 0 {
		s.log.Info("settled builds that were no longer running",
			slog.Int("count", settled))
	}
	return nil
}

// settleBuild writes the end of a build the cluster says is over.
//
// A build whose Job is gone is recorded as failed rather than as anything
// softer. It produced no image — there is nothing to deploy — and calling it
// anything else would leave a deployment that never ran looking like one that
// did. What it can say is why, which is the difference between a useless
// status and a usable one.
func (s *Service) settleBuild(
	ctx context.Context, row dbgen.Build, state orchestrator.BuildState,
) {
	status, message := BuildFailed, ""
	switch {
	case state.Found && state.Failed:
		message = state.Reason
	case state.Found && state.Done:
		// Finished cleanly, but this process never wrote the result — so the
		// image was pushed and the record was lost. Still failed, because
		// nothing applied it and nothing knows the digest; a redeploy is the
		// honest next step.
		message = "the build finished but its result was never recorded — " +
			"deploy again to use it"
	default:
		message = "the build stopped without finishing, and the job that was " +
			"running it is gone — this usually means Yacht was restarted mid-build"
	}

	if _, err := s.q.FinishBuild(ctx, dbgen.FinishBuildParams{
		ID: row.ID, Status: status, Message: message,
	}); err != nil {
		s.log.Error("settle build", slog.String("error", err.Error()))
		return
	}

	// And the deployment waiting on it. A build is only ever started for a
	// deployment, so one left running is a row the deployments list would
	// keep showing as in progress forever.
	s.endDeployment(ctx, row.OwnerID, row.DeploymentID, errors.New(message))
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
