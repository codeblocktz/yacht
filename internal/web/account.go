package web

import (
	"log/slog"
	"net/http"

	"github.com/codeblocktz/yacht/internal/identity"
)

// accountPage shows a person what their own account is and how it is reached.
//
// Everything it renders comes from the session the request presented. It takes
// no identifier from the request at all, which is what makes "whose account is
// this" a question with one possible answer rather than one the handler has to
// get right.
func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		// The same answer teamPage gives. A route inside the authenticated group
		// whose cookie will not resolve has nothing to render and nothing to say
		// about why.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	user, err := s.accounts.User(ctx, sess.UserID)
	if err != nil {
		s.log.Error("read the signed-in person", slog.String("error", err.Error()))
		http.Error(w, "could not load your account", http.StatusInternalServerError)
		return
	}

	s.render(w, r, Account(AccountData{
		Email: user.Email,
		Name:  user.DisplayName,
		Team:  displayName(owner),
		Role:  string(sess.Role),
		Since: sess.CreatedAt.Format("2 January 2006"),
	}))
}
