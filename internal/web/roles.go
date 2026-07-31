package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/identity"
)

// requireRole refuses a request whose session does not carry at least min.
//
// Mounted with r.Use on a route group and never called from a handler. A check
// each handler has to remember is one some handler will forget, and the
// forgotten one is the security bug — so a route added to a gated group later
// is covered whether or not its author thought about it.
//
// The role comes from the session and from nothing else. It is never read from
// a form field, a query parameter or a header: those are things the caller
// sends, and a permission the caller can assert is not a permission.
func (s *Server) requireRole(min account.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, ok := s.roleOf(r)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !role.AtLeast(min) {
				// Said plainly. The person is signed in and proven to be in this
				// team, so what they may do in it is not a secret being kept from
				// them, and a 404 here would only send them to look for a bug.
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// roleOf reports the role the request's own session carries in the team the
// request is acting as.
//
// The role arrives proven rather than looked up: resolving a session joins
// memberships, so the one query that says the session is live is the same query
// that says what its holder may do. Nothing here reads the request's own
// account of itself.
func (s *Server) roleOf(r *http.Request) (account.Role, bool) {
	owner, ok := identity.FromContext(r.Context())
	if !ok {
		// Only reachable if a gate is ever mounted outside the authenticated
		// group. Refusing is the safe reading of a wiring mistake.
		return "", false
	}

	// An install with no account service has no roles to read: its owner comes
	// from an injected identity.Provider — a shared token, or a wrapping
	// application's own sessions — and that principal is the whole owner.
	// Refusing here would lock every such install out of its own dashboard,
	// which is not what a role gate is for.
	if s.accounts == nil {
		return account.RoleOwner, true
	}

	sess, err := s.accounts.ResolveSession(r.Context(), sessionToken(r))
	if err != nil {
		if !errors.Is(err, account.ErrSessionInvalid) {
			s.log.Error("resolve session for a role gate", slog.String("error", err.Error()))
		}
		return "", false
	}
	// The role is only good for the team the request is acting as. The session
	// proves a role in its own active team; if the request is scoped to another
	// owner the two are not statements about the same thing, and reading one as
	// authority over the other is how a member of one team gets admin over
	// another.
	if sess.ActiveTeamID != owner.ID {
		return "", false
	}
	return sess.Role, true
}

// teamRouteStub answers the team-management routes until the tasks that build
// that page fill them in.
//
// The routes are registered now because their gate is: putting each one in its
// group is what makes "a member cannot invite" and "an admin cannot appoint an
// admin" true today rather than the day someone remembers. Supplying the
// behaviour later replaces a handler and leaves the gate alone.
func (s *Server) teamRouteStub(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "team management is not available yet", http.StatusNotImplemented)
}
