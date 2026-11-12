package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// before it writes anything for the deploy.

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
	// AdmitUpdate is a settings change that rolls out when it is saved: a
	// port, limits, a health check, a database's new release.
	AdmitUpdate AdmitTrigger = "update"
	// AdmitScale is a change of replica count, which rolls out like any
	// other; AdmitRequest.Replicas is the count asked for.
	AdmitScale AdmitTrigger = "scale"
	// AdmitPush is a push to the app's own webhook.
	AdmitPush AdmitTrigger = "push"
	// AdmitGitHubPush is a push through the GitHub App's webhook.
	AdmitGitHubPush AdmitTrigger = "github_push"
	// AdmitQueuedPush is a push that waited behind a deploy in flight, now
	// admitted by the engine itself.
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
	// App is the app's name. For AdmitEnvironment and AdmitPullRequest it is
	// the service being copied, whose copy does not exist yet.
	App string
	// ProjectID is uuid.Nil for an app in no project, and for a template's
	// stack, whose project is made after it is admitted.
	ProjectID   uuid.UUID
	Environment string
	Trigger     AdmitTrigger
	// ActorKind is who asked, as the deployment records it: "user" for a
	// person, "token" for an API token, "webhook" for a Git host.
	ActorKind string
	// Revision is the commit a push carries, shortened, when there is one.
	Revision string
	// Replicas is how many copies the deploy runs.
	Replicas int32
}

// AdmitFunc decides whether a deploy may be admitted. Nil admits every one.
// It returns nil to admit, an *AdmitRefusal to refuse with a sentence the
// person or the Git host is shown, or any other error when it could not
// decide — which fails the deploy without saying why to anybody but the log.
//
// It is called before the deploy is written, sometimes inside the
// transaction that would write it, so it should be quick, and must not call
// back into the engine's app service.
type AdmitFunc func(ctx context.Context, r AdmitRequest) error

// AdmitRefusal is a deploy the wrapper refused. Message is shown as it is:
// on the dashboard, in the API's error, and after "Ignored — " beside a Git
// host's delivery.
type AdmitRefusal struct {
	Message string
}

func (r *AdmitRefusal) Error() string { return r.Message }

// refusedDelivery is how a Git host's delivery reads when its deploy was
// refused: ignored, as a push to another branch is, with the reason.
func refusedDelivery(r *AdmitRefusal) string {
	return "Ignored — " + r.Message
}

// admittedKey marks a context whose admission has been asked: a caller that
// asked before writing anything hands it down, so the checks where the
// deploy is finally written do not ask again.
type admittedKey struct{}

func admitted(ctx context.Context) context.Context {
	return context.WithValue(ctx, admittedKey{}, true)
}

func alreadyAdmitted(ctx context.Context) bool {
	v, _ := ctx.Value(admittedKey{}).(bool)
	return v
}

// askAdmission asks the wrapper whether a deploy may be admitted, once:
// a context a caller already asked on is admitted, and the returned context
// says so to everything below. A refusal comes back as the *AdmitRefusal
// itself, unwrapped, so every caller that shows an error's text shows the
// wrapper's sentence; anything else the hook returns fails the deploy.
func (s *Service) askAdmission(ctx context.Context, req AdmitRequest) (context.Context, error) {
	if s.opts.Admit == nil || alreadyAdmitted(ctx) {
		return ctx, nil
	}
	if req.ActorKind == "" {
		req.ActorKind, _ = deployActor(ctx, "user")
	}
	if err := s.opts.Admit(ctx, req); err != nil {
		var refusal *AdmitRefusal
		if errors.As(err, &refusal) {
			return ctx, refusal
		}
		return ctx, fmt.Errorf("app: admission: %w", err)
	}
	return admitted(ctx), nil
}

// admitRequest is the request for a deploy of an app that exists.
func admitRequest(a App, trigger AdmitTrigger) AdmitRequest {
	return AdmitRequest{
		OwnerID: a.OwnerID, AppID: a.ID, App: a.Name,
		ProjectID: a.ProjectID, Environment: a.EnvSlug, Trigger: trigger, Replicas: a.Replicas,
	}
}

// admitTriggerFor reads the trigger a deployment records as the one asked
// about, for an admission no caller asked before: the safety net in
// admitDeploymentTx, which every deploy passes.
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
