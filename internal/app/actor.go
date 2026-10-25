package app

import "context"

// Actor is who asked for a deploy, as the deployment records it: a kind —
// "user", "token", "webhook", "system" — and an id within it.
//
// Carried on the context rather than threaded through every method, because
// every path that admits a deployment already takes one, and the dashboard,
// the API and a Git host all reach the same methods. A request that says
// nothing is a person's, which is what every deploy was before tokens could
// make one.
type Actor struct {
	Kind string
	ID   string
}

type actorKey struct{}

// WithActor returns a context whose deploys are recorded as act's. The API
// sets it for a token, so a deploy made from CI reads as the token's rather
// than as somebody's click.
func WithActor(ctx context.Context, act Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, act)
}

// actorFrom reads it back. Only a kind the deployments table accepts is
// believed; anything else is ignored rather than failing the deploy on an
// audit detail.
func actorFrom(ctx context.Context) (Actor, bool) {
	act, ok := ctx.Value(actorKey{}).(Actor)
	if !ok {
		return Actor{}, false
	}
	switch act.Kind {
	case "user", "token", "webhook", "system":
		return act, true
	}
	return Actor{}, false
}

// deployActor is who a deployment is recorded as: what the caller said, unless
// it said "user" — the default every caller passes — and the context names
// somebody more particular. A Git host's push says "webhook" itself, and keeps
// saying it whatever is on the context.
func deployActor(ctx context.Context, kind string) (string, string) {
	if kind != "user" {
		return kind, ""
	}
	if act, ok := actorFrom(ctx); ok {
		return act.Kind, act.ID
	}
	return kind, ""
}
