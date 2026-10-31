package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// rolloutLogTail is how much of a failed rollout's output is kept with its
// deployment: as much as the Deploy logs show of a live pod.
const rolloutLogTail = 2000

// releasePods is those of an app's pods started for a release.
func releasePods(pods []orchestrator.PodInfo, releaseID uuid.UUID) []orchestrator.PodInfo {
	var out []orchestrator.PodInfo
	for _, p := range pods {
		if p.ReleaseID == releaseID.String() {
			out = append(out, p)
		}
	}
	return out
}

// newestPod is the most recently created of pods, the one a rollout is
// waiting on when a crash has replaced the others; false for none.
func newestPod(pods []orchestrator.PodInfo) (orchestrator.PodInfo, bool) {
	var newest orchestrator.PodInfo
	for _, p := range pods {
		if newest.Name == "" || p.CreatedAt.After(newest.CreatedAt) {
			newest = p
		}
	}
	return newest, newest.Name != ""
}

// keepRolloutLog keeps what a rollout's new version printed with its
// deployment, as the rollout fails and before the rollback that follows
// removes the pod: afterwards the cluster has only the old version's, and the
// failed deployment's Deploy logs showed those (QA DEPLOY-17). A pod that
// has restarted is read from its last run, which is the one that died.
//
// Best effort, as newVersionOutput is: the deploy has failed either way.
func (s *Service) keepRolloutLog(ctx context.Context, op Operation, a App, releaseID uuid.UUID) {
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	pods, err := s.orch.Pods(read, orchestrator.PodListOptions{
		Namespace: a.Namespace, ManagedOnly: true, Owner: orchestrator.OwnerID(a.OwnerID),
	})
	if err != nil {
		return
	}
	pod, ok := newestPod(releasePods(pods, releaseID))
	if !ok {
		return
	}
	previous := pod.Restarts > 0
	lines, err := s.orch.Logs(read, orchestrator.LogOptions{
		Namespace: pod.Namespace, Pod: pod.Name, Tail: rolloutLogTail, Previous: previous,
	})
	if err != nil || len(lines) == 0 {
		return
	}
	text := make([]string, 0, len(lines))
	for _, l := range lines {
		text = append(text, strings.TrimRight(l.Text, "\r\n"))
	}
	if err := s.q.KeepRolloutLog(read, dbgen.KeepRolloutLogParams{
		DeploymentID: op.DeploymentID, OwnerID: op.OwnerID,
		Pod: pod.Name, Previous: previous, Output: strings.Join(text, "\n"),
	}); err != nil {
		s.log.Warn("could not keep the failed rollout's output",
			"operation_id", op.ID, "error", err)
	}
}

// keptRolloutLog is the output keepRolloutLog kept for a deployment; ok is
// false when it kept none.
func (s *Service) keptRolloutLog(ctx context.Context, ownerID string, deployID uuid.UUID) (Logs, bool, error) {
	row, err := s.q.GetRolloutLog(ctx, dbgen.GetRolloutLogParams{OwnerID: ownerID, DeploymentID: deployID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Logs{}, false, nil
	}
	if err != nil {
		return Logs{}, false, fmt.Errorf("app: read the failed rollout's output: %w", err)
	}
	out := Logs{Pod: row.Pod, Pods: []string{row.Pod}, Previous: row.Previous}
	for _, line := range strings.Split(row.Output, "\n") {
		out.Lines = append(out.Lines, orchestrator.LogLine{Text: line})
	}
	return out, true, nil
}

// keepFailedRolloutLog keeps the new version's output when its rollout
// failed: not for one that passed, whose pods are there to read, nor for a
// cluster that could not be asked or a worker stopping, neither of which
// failed the deploy.
func (s *Service) keepFailedRolloutLog(ctx context.Context, op Operation, a App, releaseID uuid.UUID, err error) {
	if err == nil || errors.Is(err, ErrClusterObservation) || ctx.Err() != nil {
		return
	}
	s.keepRolloutLog(ctx, op, a, releaseID)
}

// restarted is whether the pod named has restarted, and so has a previous
// run to read.
func restarted(pods []orchestrator.PodInfo, name string) bool {
	for _, p := range pods {
		if p.Name == name {
			return p.Restarts > 0
		}
	}
	return false
}
