package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

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
	// An operator acting as a team is its owner for as long as they may act:
	// support is there to fix what the team cannot, and a read-only visit
	// fixes nothing. Asked of the install on every request, like the identity
	// provider asks it, so taking somebody off the operators takes this away
	// on the same request that puts them back in their own team.
	if s.actingAs(r.Context(), sess, owner.ID) {
		return account.RoleOwner, true
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

// requireOperator gates what belongs to the install rather than to a team:
// nodes, the registry, DNS, every team's quota. See IsOperator.
func (s *Server) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := identity.FromContext(r.Context()); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.IsOperator(r) {
			// Plainly forbidden rather than hidden: the person is signed in,
			// and that the install has pages they cannot reach is no secret.
			http.Error(w, "forbidden — this is for the people who run the install", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsOperator reports whether a request comes from somebody who runs the
// install itself.
//
// With no account service there is one principal, and it is the operator.
// With no operators configured, the owner of the team the request acts as is
// — the single-team reading, and what every install did before operators
// existed. Otherwise the signed-in person's email must be one of them.
//
// Always the person, never the team they are acting as: an operator acting as
// a customer's team is still the operator, and keeps the Admin area — which is
// where they go to stop. The owner role acting grants never reaches here,
// because with operators named this reads the address rather than the role.
// Exported so a wrapping application gates its own install-wide pages the
// same way.
func (s *Server) IsOperator(r *http.Request) bool {
	if s.accounts == nil {
		_, ok := identity.FromContext(r.Context())
		return ok
	}
	if len(s.operators) == 0 {
		role, ok := s.roleOf(r)
		return ok && role.AtLeast(account.RoleOwner)
	}
	sess, err := s.accounts.ResolveSession(r.Context(), sessionToken(r))
	if err != nil {
		return false
	}
	u, err := s.accounts.User(r.Context(), sess.UserID)
	if err != nil {
		return false
	}
	return s.operators[strings.ToLower(strings.TrimSpace(u.Email))]
}

func normaliseEmails(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, e := range in {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out[e] = true
		}
	}
	return out
}
