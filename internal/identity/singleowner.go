package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// DefaultOwnerID is the owner every resource belongs to in a single-owner
// deployment.
//
// It is a fixed value rather than a generated one so that the schema's
// owner_id column is populated identically across restarts and reinstalls,
// and so a later migration to multi-owner has one known value to rewrite.
const DefaultOwnerID = "owner-local"

// SingleOwner resolves every request to the same owner without authenticating
// it.
//
// This is the right default for the engine's primary use case — one person
// running it on their own machine or a private network — and the wrong choice
// for anything reachable from the internet. Pair it with StaticToken, a
// reverse proxy that authenticates, or a provider of your own for exposed
// deployments.
type SingleOwner struct {
	owner Owner
}

var _ Provider = (*SingleOwner)(nil)

// NewSingleOwner returns a provider that always resolves to owner.
// A zero-value owner falls back to a sensible local default.
func NewSingleOwner(owner Owner) *SingleOwner {
	if !owner.Valid() {
		owner = Owner{ID: DefaultOwnerID, DisplayName: "Local"}
	}
	return &SingleOwner{owner: owner}
}

func (s *SingleOwner) Resolve(context.Context, *http.Request) (Owner, error) {
	return s.owner, nil
}

// StaticToken authenticates a single owner with a shared bearer token.
//
// Modest by design: it exists so a self-hoster can expose the engine without
// running it wide open, and so the Provider interface ships with more than one
// implementation — an interface with a single implementation tends not to stay
// a real seam.
type StaticToken struct {
	owner Owner
	token []byte
}

var _ Provider = (*StaticToken)(nil)

// NewStaticToken returns a provider that accepts exactly one bearer token.
// It returns an error rather than silently allowing everything if token is
// empty.
func NewStaticToken(owner Owner, token string) (*StaticToken, error) {
	if token == "" {
		return nil, errors.New("identity: static token must not be empty")
	}
	if !owner.Valid() {
		owner = Owner{ID: DefaultOwnerID, DisplayName: "Local"}
	}
	return &StaticToken{owner: owner, token: []byte(token)}, nil
}

func (s *StaticToken) Resolve(_ context.Context, r *http.Request) (Owner, error) {
	header := r.Header.Get("Authorization")
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return Owner{}, ErrUnauthenticated
	}
	// Constant time: a token check that leaks timing leaks the token.
	if subtle.ConstantTimeCompare([]byte(value), s.token) != 1 {
		return Owner{}, ErrUnauthenticated
	}
	return s.owner, nil
}
