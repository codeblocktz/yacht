package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/identity"
)

// passwordChangeAttempts is what a stolen cookie is allowed to guess.
//
// This route is reachable only with a live session, so the attacker it bounds is
// somebody holding a copied cookie trying to turn it into permanent access by
// producing the current password. Counted against the user id rather than the
// address, because the id is the thing they cannot rotate.
const passwordChangeAttempts = 5

// accountPage shows a person what their own account is and how it is reached.
//
// Everything it renders comes from the session the request presented. It takes
// no identifier from the request at all, which is what makes "whose account is
// this" a question with one possible answer rather than one the handler has to
// get right.
func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, http.StatusOK, AccountData{})
}

// renderAccount loads the page's own facts and draws it.
//
// One funnel, so that the error path and the happy path cannot disagree about
// what the page shows — which is how a refusal ends up telling somebody who has
// a password that they have none.
func (s *Server) renderAccount(
	w http.ResponseWriter, r *http.Request, status int, over AccountData,
) {
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

	has, err := s.accounts.HasPassword(ctx, sess.UserID)
	if err != nil {
		s.log.Error("read password state", slog.String("error", err.Error()))
		http.Error(w, "could not load your account", http.StatusInternalServerError)
		return
	}

	s.renderStatus(w, r, status, Account(AccountData{
		Email:       user.Email,
		Name:        user.DisplayName,
		Team:        displayName(owner),
		Role:        string(sess.Role),
		Since:       sess.CreatedAt.Format("2 January 2006"),
		HasPassword: has,
		// Read here only to decide what to draw. The service checks it again at
		// the moment of the write, which is the check that counts: a form
		// rendered inside the window and submitted outside it must not work.
		RecentAuth: sess.RecentlyAuthenticated(account.StepUpWindow),
		MinLength:  account.MinPasswordLength,
		Error:      over.Error,
		Notice:     over.Notice,
	}))
}

// accountPasswordSet adds a password or replaces the one that is there.
//
// The handler checks almost nothing. Whose account this is comes from the
// session, the policy is the service's, and so is whether the session has proved
// itself recently enough — all three are verified at the write rather than here,
// so a handler that forgot one could not succeed anyway. What is left is the
// confirmation field, which exists only in the form and therefore has nowhere
// else to be checked.
func (s *Server) accountPasswordSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if !s.signInLimit.allow(
		"current:"+sess.UserID.String(), passwordChangeAttempts) {
		s.refuseAccount(w, r, http.StatusTooManyRequests,
			"Too many attempts. Try again in a few minutes.")
		return
	}

	password := r.FormValue("password")
	if password != r.FormValue("confirm") {
		s.refuseAccount(w, r, http.StatusUnprocessableEntity,
			"Those two passwords are not the same.")
		return
	}

	err = s.accounts.SetPassword(ctx, sess.ID, password,
		account.Proof{CurrentPassword: r.FormValue("current_password")})
	switch {
	case err == nil:
	case errors.Is(err, account.ErrNeedsReauthentication):
		s.refuseAccount(w, r, http.StatusUnprocessableEntity,
			"Sign in again before changing this. It has been a while since "+
				"this browser proved who is using it.")
		return
	case errors.Is(err, account.ErrCredentialInvalid):
		s.refuseAccount(w, r, http.StatusUnprocessableEntity,
			"That is not your current password.")
		return
	case errors.Is(err, account.ErrPasswordTooShort),
		errors.Is(err, account.ErrPasswordTooLong),
		errors.Is(err, account.ErrPasswordIsEmail):
		// The service's own wording. These are the three things it refuses, and
		// each says what to do differently.
		s.refuseAccount(w, r, http.StatusUnprocessableEntity, passwordFault(err))
		return
	default:
		s.log.Error("set password",
			slog.String("user", sess.UserID.String()), slog.String("error", err.Error()))
		s.refuseAccount(w, r, http.StatusInternalServerError,
			"Something went wrong saving that. Try again in a moment.")
		return
	}

	s.log.Info("password set", slog.String("user", sess.UserID.String()))
	s.flashOK(w, r, "Password saved.")
	http.Redirect(w, r, "/account#password", http.StatusSeeOther)
}

// refuseAccount re-renders the account page carrying a message.
//
// Nothing the person typed is carried back. A password put into the HTML would
// survive in a screenshot, a print, or a pasted bug report, and no-store reaches
// none of those.
func (s *Server) refuseAccount(
	w http.ResponseWriter, r *http.Request, status int, msg string,
) {
	s.renderAccount(w, r, status, AccountData{Error: msg})
}

// passwordFault turns the service's policy errors into what the page says.
//
// They are already written for a reader, so this strips the package prefix
// rather than restating them — two wordings for one rule is how they drift.
func passwordFault(err error) string {
	switch {
	case errors.Is(err, account.ErrPasswordTooShort):
		return "That password is too short. Use at least " +
			strconv.Itoa(account.MinPasswordLength) + " characters."
	case errors.Is(err, account.ErrPasswordTooLong):
		return "That password is too long."
	default:
		return "Your password cannot be your email address."
	}
}
