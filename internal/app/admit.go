package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Admission: the one question a wrapping application may ask of every
// deploy before it is admitted.
//
// The engine admits a deploy from many places — a person's redeploy, a
// token's upload, a Git host's push, a push that waited for the deploy before
// it, a pull request's environment, a stack from a template. A wrapper that
// keeps some deploys from happening could guard each of those in front of the
// engine, and would miss the ones nobody asks it about: a queued push is
// admitted by the engine's own loop. So the engine asks, at each of them,
// before it writes anything for the deploy and before it takes any lock.

// AdmitTrigger is what is asking for a deploy, for the wrapper to decide by.
type AdmitTrigger string

const (
	// AdmitCreate is a new service's first deploy: from the dashboard, a
	// repository, a template's stack or a database.
	AdmitCreate AdmitTrigger = "create"
	// AdmitRedeploy is a person or a token deploying again what the app
	// already runs or builds.
	AdmitRedeploy AdmitTrigger = "redeploy"
	// AdmitRollback is going back to an earlier release.
	AdmitRollback AdmitTrigger = "rollback"
	// AdmitUpload is source uploaded from a working directory: the CLI's up.
	AdmitUpload AdmitTrigger = "upload"
	// AdmitStaged is deploying the changes an app has waiting.
	AdmitStaged AdmitTrigger = "staged"
	// AdmitUpdate is a change that rolls out when it is saved: settings, a
	// variable, a health check, a database's new release.
	AdmitUpdate AdmitTrigger = "update"
	// AdmitScale is a change of replica count, which rolls out like any
	// other: Replicas is the count asked for, CurrentReplicas the count
	// now. Scaling to zero stops an app and is never asked.
	AdmitScale AdmitTrigger = "scale"
	// AdmitVolume is storage attached to an app, or grown.
	AdmitVolume AdmitTrigger = "volume"
	// AdmitMove is an app redeployed in another region, which Region names.
	AdmitMove AdmitTrigger = "move"
	// AdmitPush is a push to the app's own webhook.
	AdmitPush AdmitTrigger = "push"
	// AdmitGitHubPush is a push through the GitHub App's webhook.
	AdmitGitHubPush AdmitTrigger = "github_push"
	// AdmitQueuedPush is a push that waited behind a deploy in flight, now
	// admitted by the engine itself. It was asked about when it arrived too.
	AdmitQueuedPush AdmitTrigger = "queued_push"
	// AdmitEnvironment is a service copied into an environment made from
	// another.
	AdmitEnvironment AdmitTrigger = "environment"
	// AdmitPullRequest is a service copied into a pull request's
	// environment.
	AdmitPullRequest AdmitTrigger = "pull_request"
)

// AdmitRequest is one deploy the engine is about to admit.
type AdmitRequest struct {
	OwnerID string
	// AppID is the app deployed; uuid.Nil for one the deploy creates.
	AppID uuid.UUID
	// App is the app's name: for a deploy that creates one — AdmitCreate,
	// AdmitEnvironment, AdmitPullRequest — the name it will have.
	App string
	// ProjectID is uuid.Nil for an app in no project, and for a template's
	// stack, whose project is made after it is admitted.
	ProjectID   uuid.UUID
	Environment string
	// Region is where the deploy runs, "" for the install's default; for
	// AdmitMove, where the app is going.
	Region  string
	Trigger AdmitTrigger
	// ActorKind is who asked, as the deployment records it: "user" for a
	// person, "token" for an API token, "webhook" for a Git host, "system"
	// for the engine or a wrapper's own code (see WithActor). ActorID is the
	// person's or the token's id, when the request carries one.
	ActorKind string
	ActorID   string
	// Revision is the commit a push carries, shortened, when there is one.
	Revision string
	// Replicas is how many copies the deploy runs, and CurrentReplicas how
	// many the app runs now (zero for one being created).
	Replicas        int32
	CurrentReplicas int32
}

// AdmitFunc decides whether a deploy may be admitted. Nil admits every one.
// It returns nil to admit, an *AdmitRefusal to refuse with a sentence the
// person or the Git host is shown, or any other error when it could not
// decide — which fails the deploy with ErrAdmissionUnavailable, the error
// itself going only to the log.
//
// It is called with no lock held and nothing written, and given
// AdmitTimeout to answer; a slower answer is an error.
type AdmitFunc func(ctx context.Context, r AdmitRequest) error

// AdmitTimeout is how long Admit has to answer.
const AdmitTimeout = 5 * time.Second

// admitTimeout is AdmitTimeout, shortened by a test.
var admitTimeout = AdmitTimeout

// AdmitRefusal is a deploy the wrapper refused. Message is shown as it is:
// on the dashboard, in the API's error, and after "Ignored — " beside a Git
// host's delivery. Empty, a plain sentence is shown instead.
type AdmitRefusal struct {
	Message string
}

func (r *AdmitRefusal) Error() string {
	if r == nil || r.Message == "" {
		return defaultRefusal
	}
	return r.Message
}

const defaultRefusal = "This deploy was refused."

// ErrAdmissionUnavailable is a deploy the wrapper's Admit could not decide
// on — it failed, or took too long. The deploy is not admitted; what went
// wrong is logged, never shown, since it may name the wrapper's own
// systems.
var ErrAdmissionUnavailable = errors.New("app: deploys can't be checked right now — try again in a moment")

// refusedDelivery is how a Git host's delivery reads when its deploy was
// refused: ignored, as a push to another branch is, with the reason.
func refusedDelivery(r *AdmitRefusal) string {
	return "Ignored — " + r.Error()
}

// admittedKey marks a context with the apps whose deploy has been asked
// about, by owner and name: a caller that asked before writing anything
// hands it down, so the checks where the deploy is finally written do not
// ask again — for those apps, and no others.
type admittedKey struct{}

func admittedName(owner, app string) string { return owner + "/" + app }

func markAdmitted(ctx context.Context, owner, app string) context.Context {
	prev, _ := ctx.Value(admittedKey{}).(map[string]bool)
	next := make(map[string]bool, len(prev)+1)
	for k := range prev {
		next[k] = true
	}
	next[admittedName(owner, app)] = true
	return context.WithValue(ctx, admittedKey{}, next)
}

func alreadyAdmitted(ctx context.Context, owner, app string) bool {
	set, _ := ctx.Value(admittedKey{}).(map[string]bool)
	return set[admittedName(owner, app)]
}

// askAdmission asks the wrapper whether a deploy may be admitted, once per
// app: an app the context says was asked about is admitted, and the
// returned context says so for this one.
//
// A refusal comes back as an *AdmitRefusal, unwrapped, so every caller that
// shows an error's text shows the wrapper's sentence. Anything else the
// hook returns, or a hook that does not answer in AdmitTimeout, is logged
// here and comes back as ErrAdmissionUnavailable.
func (s *Service) askAdmission(ctx context.Context, req AdmitRequest) (context.Context, error) {
	if s.opts.Admit == nil || alreadyAdmitted(ctx, req.OwnerID, req.App) {
		return ctx, nil
	}
	if req.ActorKind == "" {
		req.ActorKind, req.ActorID = deployActor(ctx, "user")
	}
	hctx, cancel := context.WithTimeout(ctx, admitTimeout)
	err := s.opts.Admit(hctx, req)
	cancel()
	if err == nil {
		return markAdmitted(ctx, req.OwnerID, req.App), nil
	}
	var refusal *AdmitRefusal
	if errors.As(err, &refusal) && refusal != nil {
		if refusal.Message == "" {
			return ctx, &AdmitRefusal{Message: defaultRefusal}
		}
		return ctx, refusal
	}
	s.log.Error("admission could not be decided",
		slog.String("owner", req.OwnerID), slog.String("app", req.App),
		slog.String("trigger", string(req.Trigger)), slog.String("error", err.Error()))
	return ctx, ErrAdmissionUnavailable
}

// isRefusal reports whether err is the wrapper refusing a deploy.
func isRefusal(err error) (*AdmitRefusal, bool) {
	var refusal *AdmitRefusal
	if errors.As(err, &refusal) && refusal != nil {
		return refusal, true
	}
	return nil, false
}

// admitRequest is the request for a deploy of an app that exists.
func admitRequest(a App, trigger AdmitTrigger) AdmitRequest {
	return AdmitRequest{
		OwnerID: a.OwnerID, AppID: a.ID, App: a.Name,
		ProjectID: a.ProjectID, Environment: a.EnvSlug, Region: a.Region,
		Trigger: trigger, Replicas: a.Replicas, CurrentReplicas: a.Replicas,
	}
}

// admitTriggerFor reads the trigger a deployment records as the one asked
// about, for an admission no caller asked before: the fallback in
// admitDeploymentTx and deployCurrentRelease.
func admitTriggerFor(trigger, actorKind string) AdmitTrigger {
	switch {
	case trigger == "redeploy":
		return AdmitRedeploy
	case trigger == RollbackTrigger:
		return AdmitRollback
	case strings.HasPrefix(trigger, UploadTriggerPrefix):
		return AdmitUpload
	case strings.HasPrefix(trigger, PushTriggerPrefix):
		if actorKind == "webhook" {
			return AdmitPush
		}
		return AdmitQueuedPush
	case strings.HasPrefix(trigger, "scale:"):
		return AdmitScale
	case strings.HasPrefix(trigger, "changes:"):
		return AdmitStaged
	}
	return AdmitUpdate
}
