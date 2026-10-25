package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// The events a project's webhooks can be told about, named the way Railway's
// deployment statuses are.
const (
	// EventDeploySucceeded is a deploy whose new version came up.
	EventDeploySucceeded = "deploy.succeeded"
	// EventDeployFailed is a deploy that did not: the build failed, the
	// pre-deploy command did, or the rollout never became ready.
	EventDeployFailed = "deploy.failed"
	// EventDeployCrashed is a deploy whose new version crashed as it
	// started — Railway's CRASHED, as opposed to a build that never got
	// that far.
	EventDeployCrashed = "deploy.crashed"
	// EventDeployRemoved is a deployment taken out of service because its
	// app was deleted.
	EventDeployRemoved = "deploy.removed"
	// EventAppCrashed is an app that was serving and has started failing:
	// its containers crash on every start. Said once when it starts, not on
	// every look.
	EventAppCrashed = "app.crashed"
)

// Event is something that happened to one of a project's apps, for whoever
// is listening.
type Event struct {
	Kind      string
	OwnerID   string
	ProjectID uuid.UUID
	AppID     uuid.UUID
	AppName   string

	// DeploymentID, Trigger and Message describe the deploy, where the event
	// is about one. Message is why it failed, in the words the deployments
	// list uses.
	DeploymentID uuid.UUID
	Trigger      string
	Message      string

	At time.Time
}

// EventSink is told about events after they happen, off the critical path:
// the deploy has already finished, and whether anybody hears about it cannot
// change how it went. An implementation records what it needs and returns
// quickly — the webhooks write a delivery row and send it later.
type EventSink interface {
	Emit(ctx context.Context, e Event)
}

// emit hands an event to the sink, if there is one, with a context that
// outlives the request or operation that caused it.
func (s *Service) emit(ctx context.Context, e Event) {
	if s.opts.Events == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	s.opts.Events.Emit(ctx, e)
}

// emitDeployFinished says how an operation's deploy ended. Read from the app
// row rather than carried on the operation, because the operation knows the
// app only by id and the webhook needs its name and project.
func (s *Service) emitDeployFinished(ctx context.Context, op Operation, cause error) {
	if s.opts.Events == nil {
		return
	}
	row, err := s.q.GetAppByID(context.WithoutCancel(ctx), dbgen.GetAppByIDParams{
		OwnerID: op.OwnerID, ID: op.AppID,
	})
	if err != nil {
		s.log.Warn("read the app a finished deploy was for", slog.String("error", err.Error()))
		return
	}
	a := toApp(row)
	e := Event{
		Kind: EventDeploySucceeded, OwnerID: op.OwnerID, ProjectID: a.ProjectID,
		AppID: a.ID, AppName: a.Name, DeploymentID: op.DeploymentID,
	}
	if d, err := s.q.GetDeployment(context.WithoutCancel(ctx), dbgen.GetDeploymentParams{
		OwnerID: op.OwnerID, ID: op.DeploymentID,
	}); err == nil {
		e.Trigger = d.Trigger
	}
	if cause != nil {
		e.Kind, e.Message = EventDeployFailed, cause.Error()
		if crashed(cause.Error()) {
			e.Kind = EventDeployCrashed
		}
	}
	s.emit(ctx, e)
}

// crashed reports whether a failed rollout failed because the new version's
// container crashed, which the orchestrator says in the reason it gives:
// a crash loop, an exit under the never policy, or a process that could not
// start at all.
func crashed(message string) bool {
	for _, reason := range []string{"CrashLoopBackOff", "Crashed:", "StartError"} {
		if strings.Contains(message, reason) {
			return true
		}
	}
	return false
}

// servingDeployment is the deployment of the release an app is serving, the
// newest when several deployed it; uuid.Nil when none did or it cannot be
// read — a removed event without a deployment still says the app went.
func (s *Service) servingDeployment(ctx context.Context, a App) uuid.UUID {
	if s.opts.Events == nil {
		return uuid.Nil
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT d.id FROM deployments d
		JOIN apps a ON a.id = d.app_id AND a.active_release_id = d.release_id
		WHERE d.owner_id = $1 AND d.app_id = $2
		ORDER BY d.started_at DESC LIMIT 1`, a.OwnerID, a.ID).Scan(&id)
	if err != nil {
		return uuid.Nil
	}
	return id
}
