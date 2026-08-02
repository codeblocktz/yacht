package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// LogRequest asks for one app's container output.
type LogRequest struct {
	// Pod is optional. Empty reads the app's first pod, which is what somebody
	// looking at a single-replica service wants and is the common case.
	Pod string

	// Previous reads the container that died rather than the one running.
	Previous bool

	Tail int64
}

// Deployment returns one of an app's deployments.
//
// Separate from DeploymentLogs because the sheet has views that show no
// container output at all and still need the deployment's own detail for their
// heading. Reading the cluster to fill a pane that discards the result would
// cost an API call per tab and, worse, make a log fetch happen where the page
// says none did.
func (s *Service) Deployment(
	ctx context.Context, ownerID, appName string, deployID uuid.UUID,
) (Deployment, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return Deployment{}, err
	}

	row, err := s.q.GetDeployment(ctx, dbgen.GetDeploymentParams{OwnerID: ownerID, ID: deployID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Deployment{}, ErrNotFound
		}
		return Deployment{}, fmt.Errorf("app: read deployment: %w", err)
	}
	// The deployment has to belong to the app in the URL, or a deployment id is
	// a way to read across apps within a team.
	if row.AppID != a.ID {
		return Deployment{}, ErrNotFound
	}

	d := Deployment{
		ID: row.ID, AppID: row.AppID, Image: row.Image,
		Revision: row.Revision, Status: row.Status,
		Message: row.Message, StartedAt: row.StartedAt,
	}
	if row.FinishedAt.Valid {
		finished := row.FinishedAt.Time
		d.FinishedAt = &finished
	}
	return d, nil
}

// DeployLogs is what one deployment's log view can honestly show.
type DeployLogs struct {
	Deployment Deployment
	Logs       Logs

	// Live is true when this deployment is the one currently running, which is
	// the only case where there are containers left to read.
	Live bool
}

// DeploymentLogs reads the output of one deployment.
//
// Only the running deployment has any. Kubernetes keeps a container's log with
// the container, so the pods that served a superseded deployment took their
// output with them when they were replaced — retaining it would need a log
// store shipped off the cluster, which this install does not have.
//
// Saying that is the point. Reading the current pods and captioning them with
// an old deployment's revision would look like history and be the live log,
// which is worse than an empty pane: somebody would draw conclusions about a
// deploy from output that postdates it.
func (s *Service) DeploymentLogs(
	ctx context.Context, ownerID, appName string, deployID uuid.UUID, req LogRequest,
) (DeployLogs, error) {
	d, err := s.Deployment(ctx, ownerID, appName, deployID)
	if err != nil {
		return DeployLogs{}, err
	}

	out := DeployLogs{Deployment: d}
	out.Live = out.Deployment.Status == DeployRunning || out.Deployment.Status == DeployActive
	if !out.Live {
		out.Logs.Note = "This deployment has been replaced. Its containers are gone, and " +
			"their output went with them — nothing here stores logs off the cluster."
		return out, nil
	}

	if out.Logs, err = s.Logs(ctx, ownerID, appName, req); err != nil {
		return DeployLogs{}, err
	}
	return out, nil
}

// Logs is an app's container output, and which pods it could have come from.
type Logs struct {
	Pod      string
	Pods     []string
	Lines    []orchestrator.LogLine
	Previous bool

	// Note explains an empty result that is not a failure — a pod that has
	// never restarted has no previous container, and saying so beats an empty
	// pane that looks like a bug.
	Note string
}

// Logs reads an app's container output.
//
// The app is resolved by owner first and the namespace comes from that row, so
// the only pods reachable are the ones belonging to an app the caller owns. A
// pod name is enough to read any tenant's output, so it is never trusted from
// the request: the name is checked against the pods this app actually has.
func (s *Service) Logs(ctx context.Context, ownerID, appName string, req LogRequest) (Logs, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return Logs{}, err
	}

	pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{
		Namespace: a.Namespace, ManagedOnly: true, Owner: orchestrator.OwnerID(ownerID),
	})
	if err != nil {
		return Logs{}, fmt.Errorf("app: list pods for logs: %w", err)
	}

	out := Logs{Previous: req.Previous}
	for _, p := range pods {
		out.Pods = append(out.Pods, p.Name)
	}
	if len(out.Pods) == 0 {
		out.Note = "This app has no running pods, so there is no output to read."
		return out, nil
	}

	// A pod named by the request is honoured only if it is one of this app's.
	// Otherwise the name is a way to read somebody else's container.
	out.Pod = out.Pods[0]
	if req.Pod != "" {
		var ok bool
		for _, name := range out.Pods {
			if name == req.Pod {
				out.Pod, ok = name, true
				break
			}
		}
		if !ok {
			return Logs{}, ErrNotFound
		}
	}

	lines, err := s.orch.Logs(ctx, orchestrator.LogOptions{
		Namespace: a.Namespace, Pod: out.Pod, Tail: req.Tail, Previous: req.Previous,
	})
	if err != nil {
		return Logs{}, fmt.Errorf("app: read logs: %w", err)
	}
	out.Lines = lines

	if len(lines) == 0 {
		if req.Previous {
			out.Note = "This container has not restarted, so there is no earlier run to show."
		} else {
			out.Note = "The container has not written anything yet."
		}
	}
	return out, nil
}
