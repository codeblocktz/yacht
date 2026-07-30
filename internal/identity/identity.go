// Package identity answers one question: who owns this request?
//
// SEAM 2 of 3. The engine deliberately does not own authentication. It defines
// the question and consumes the answer; how the answer is produced is
// injected.
//
// The reason is the open-core boundary. The engine is single-owner: one person
// runs it on their own cluster. A wrapping application is multi-tenant and
// resolves an owner from a signed token, a session, an org membership, or all
// three. If the engine baked in its own session handling, that wrapping layer
// would have to fight it — usually by forking, which defeats the point of a
// module. With the resolution injected, both sides get what they need and
// neither knows about the other.
//
// Everything downstream — handlers, storage, the orchestrator — takes an
// OwnerID and never asks where it came from.
package identity

import (
	"context"
	"errors"
	"net/http"
)

// ErrUnauthenticated means no owner could be resolved for a request.
var ErrUnauthenticated = errors.New("identity: unauthenticated")

// Owner is the principal a request acts as.
//
// The engine treats ID as opaque and never parses it. A wrapping application is
// free to make it a tenant UUID, an org slug, or anything else stable.
type Owner struct {
	ID          string
	DisplayName string
	Email       string
}

// Valid reports whether the owner is usable.
func (o Owner) Valid() bool { return o.ID != "" }

// Provider resolves the owner for an inbound request.
//
// Implementations must not write to the response; the middleware owns the
// failure path so that every rejection looks the same regardless of provider.
type Provider interface {
	Resolve(ctx context.Context, r *http.Request) (Owner, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context, r *http.Request) (Owner, error)

func (f ProviderFunc) Resolve(ctx context.Context, r *http.Request) (Owner, error) {
	return f(ctx, r)
}

type ctxKey struct{}

// NewContext returns a context carrying the owner.
func NewContext(ctx context.Context, o Owner) context.Context {
	return context.WithValue(ctx, ctxKey{}, o)
}

// FromContext returns the owner carried by ctx, if any.
func FromContext(ctx context.Context) (Owner, bool) {
	o, ok := ctx.Value(ctxKey{}).(Owner)
	if !ok || !o.Valid() {
		return Owner{}, false
	}
	return o, true
}

// MustFromContext returns the owner, panicking if absent.
//
// Intended for handlers mounted behind Middleware, where a missing owner is a
// wiring bug rather than a runtime condition. Panicking makes that bug loud in
// development instead of silently serving another owner's data — which is the
// failure this whole seam exists to prevent.
func MustFromContext(ctx context.Context) Owner {
	o, ok := FromContext(ctx)
	if !ok {
		panic("identity: no owner in context — handler is not behind identity.Middleware")
	}
	return o
}

// Middleware resolves the owner once per request and puts it in the context.
//
// Doing this in one place, rather than in each handler, is deliberate. A check
// every handler has to remember is a check some handler will eventually forget,
// and the forgotten one is the security bug. Mount this on the router group and
// handlers cannot opt out by accident.
func Middleware(p Provider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			owner, err := p.Resolve(r.Context(), r)
			if err != nil || !owner.Valid() {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), owner)))
		})
	}
}
