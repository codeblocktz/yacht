package web

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Acting as a team: an operator reaching a customer's team to help them.
//
// Three rules hold it together, and each is here because the obvious version
// of the feature breaks one of them.
//
// Only named operators act. With YACHT_OPERATORS empty, the operator is the
// owner of whichever team a request acts as — so if impersonation were on,
// every team owner could act as every other team. It is off in that mode, and
// says so rather than hiding.
//
// Authority is re-checked on every request, by the identity provider and by
// roleOf, against the same list. Checked only when acting starts, it would stay
// with whoever started it; taking somebody off the list has to end their
// impersonation on their next request.
//
// It is always visible. Every page shows who is being acted as, with the way
// out beside it, and the start and the stop are written down where the team's
// page shows them.

// OperatorCheck is the ActingCheck for an install whose operators are named:
// the person's address must be on the list. With no names it approves nobody,
// because with no names every team owner reads as an operator — see above.
//
// Exported so the engine wires the identity provider with exactly the check
// the dashboard's own gate uses. Two copies of this rule are two chances for
// the door and the corridor to disagree about who is allowed through.
func OperatorCheck(operators []string) account.ActingCheck {
	named := normaliseEmails(operators)
	return func(_ context.Context, u account.User) bool {
		return len(named) > 0 && named[strings.ToLower(strings.TrimSpace(u.Email))]
	}
}

// impersonation reports whether this install lets operators act as teams.
func (s *Server) impersonation() bool {
	return s.accounts != nil && len(s.operators) > 0
}

// actingAs reports whether a session is acting — and is still allowed to act
// — as the team a request resolved to.
//
// Both halves matter. A session can carry an acting team whose holder has
// since been taken off the list: the provider clears it on their next request,
// but this is asked on that same request and must not read the column as
// permission in the meantime. And the resolved owner must be the acting team:
// under a wrapping application's own provider it may be something else
// entirely, and a role proved in one team is never authority in another.
func (s *Server) actingAs(ctx context.Context, sess account.Session, ownerID string) bool {
	if !s.impersonation() || sess.ActingTeamID == "" || sess.ActingTeamID != ownerID {
		return false
	}
	u, err := s.accounts.User(ctx, sess.UserID)
	if err != nil {
		return false
	}
	return s.mayAct(ctx, u)
}

// actingSession is the request's session when it is validly acting as the
// team the request resolved to.
func (s *Server) actingSession(r *http.Request) (account.Session, bool) {
	if !s.impersonation() {
		return account.Session{}, false
	}
	owner, ok := identity.FromContext(r.Context())
	if !ok {
		return account.Session{}, false
	}
	sess, err := s.accounts.ResolveSession(r.Context(), sessionToken(r))
	if err != nil {
		return account.Session{}, false
	}
	return sess, s.actingAs(r.Context(), sess, owner.ID)
}

// adminTeamAct starts acting as a team. Behind the operator gate, and asked
// again here of the same check the identity provider will ask on every request
// after, so that what lets somebody start is exactly what lets them continue.
func (s *Server) adminTeamAct(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.impersonation() {
		// Refused plainly rather than left unmounted, so the person who tries
		// learns what would make it work. With nobody named, every team owner
		// passes the operator gate — and every one of them could act as every
		// other team.
		http.Error(w, "forbidden — acting as a team needs the install's operators named in "+
			"YACHT_OPERATORS. With none named, every team's owner counts as an operator, "+
			"and any of them could act as every other team.", http.StatusForbidden)
		return
	}

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := s.accounts.User(ctx, sess.UserID)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.mayAct(ctx, user) {
		http.Error(w, "forbidden — this is for the people who run the install", http.StatusForbidden)
		return
	}

	team, err := s.accounts.StartActing(ctx, sess.ID, chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, account.ErrNoSuchTeam):
		http.NotFound(w, r)
		return
	case errors.Is(err, account.ErrSessionInvalid):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case err != nil:
		s.log.Error("start acting as a team",
			slog.String("team", chi.URLParam(r, "id")), slog.String("error", err.Error()))
		http.Error(w, "could not start acting as the team", http.StatusInternalServerError)
		return
	}

	s.log.Info("impersonation started",
		slog.String("operator", user.Email),
		slog.String("user", user.ID.String()),
		slog.String("team", team.ID))
	s.flashWarn(w, r, "You are acting as "+cmp.Or(team.DisplayName, team.ID)+
		". Everything you do is done as this team, and the start and stop are recorded on its page.")
	// To the overview, which is now the team's: what the operator came to see.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// actingStop puts the session back in its own team.
//
// Mounted for any signed-in session rather than behind the operator gate. An
// operator who has just been taken off the list must still be able to press
// the button in front of them — and by then the provider has already put them
// back, so this finds nothing to stop and says nothing about it.
func (s *Server) actingStop(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	team, err := s.accounts.StopActing(ctx, sess.ID)
	if err != nil {
		s.log.Error("stop acting as a team", slog.String("error", err.Error()))
		http.Error(w, "could not stop acting as the team", http.StatusInternalServerError)
		return
	}
	if team == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.logActingStopped(ctx, "impersonation stopped", sess.UserID, team)
	s.flashOK(w, r, "You are back in your own team.")

	// Back to the team's page in the Admin area, which is where acting began
	// and where the stop just recorded is shown.
	if s.quotas != nil {
		http.Redirect(w, r, adminTeamHref(team), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// logActingStopped writes the Info line every stop gets, with who and which
// team. The address is looked up best-effort: the stop has already happened
// and been recorded, and a log line missing an address is better than none.
func (s *Server) logActingStopped(ctx context.Context, msg string, userID uuid.UUID, team string) {
	email := ""
	if u, err := s.accounts.User(ctx, userID); err == nil {
		email = u.Email
	}
	s.log.Info(msg,
		slog.String("operator", email),
		slog.String("user", userID.String()),
		slog.String("team", team))
}

// ActingPanel is the team page's support-access section.
type ActingPanel struct {
	// Shown is whether there are accounts at all. Without them there are no
	// sessions to layer a team on, and nothing to say.
	Shown bool

	// Available is whether operators are named, which is what impersonation
	// needs. See the note at the top of this file.
	Available bool

	// Now is whether the viewer is acting as this team at this moment.
	Now bool

	// Events is the recent record, newest first.
	Events []account.ImpersonationEvent
}

// impersonationEventsShown is how much of the record the team page shows. The
// page answers "has anybody been in here lately", and the log holds the rest.
const impersonationEventsShown = 8

// actingPanel builds the team page's section for one team.
func (s *Server) actingPanel(r *http.Request, teamID string) ActingPanel {
	if s.accounts == nil {
		return ActingPanel{}
	}
	p := ActingPanel{Shown: true, Available: s.impersonation()}
	if sess, err := s.accounts.ResolveSession(r.Context(), sessionToken(r)); err == nil {
		p.Now = sess.ActingTeamID == teamID && s.actingAs(r.Context(), sess, teamID)
	}
	events, err := s.accounts.ImpersonationEvents(r.Context(), teamID, impersonationEventsShown)
	if err != nil {
		// The page still renders: the record is also in the log, and an
		// operator who cannot set a quota because an audit list failed to load
		// has been handed a worse problem than the one they came with.
		s.log.Error("list impersonation events", slog.String("team", teamID),
			slog.String("error", err.Error()))
	}
	p.Events = events
	return p
}

// ActingBanner is the notice every page carries while an operator is acting as
// a team: which team, and the way out.
//
// DefaultSlots puts it in Banner. The layout also puts it above whatever
// Banner a SlotProvider sets, so a wrapping application that fills the slot
// with its own notice — a low balance, say — cannot hide this one by
// accident; it can read Surfaces.ActingAs to decide what else to show.
func ActingBanner(team string) templ.Component {
	return actingBanner{team: team}
}

// actingBanner is a type of its own so the layout can tell that a banner
// already is the acting notice, and not draw it twice.
type actingBanner struct {
	team  string
	below templ.Component
}

func (b actingBanner) Render(ctx context.Context, w io.Writer) error {
	return actingBannerView(b.team, b.below).Render(ctx, w)
}

// withActingBanner is the banner a page is drawn with: whatever the slots
// carry, with the acting notice above it while the request is acting.
//
// The one thing the layout reads from the request rather than from Slots, and
// deliberately. Everything else in the chrome is a wrapping application's to
// replace; this is the one statement it must not be able to drop, because an
// operator who cannot see that they are acting as a customer will eventually
// do something to the customer's apps believing they are their own. Read from
// Surfaces, which only this package's middleware can put on a context.
func withActingBanner(ctx context.Context, banner templ.Component) templ.Component {
	team := SurfacesFromContext(ctx).ActingAs
	if team == "" {
		return banner
	}
	if b, ok := banner.(actingBanner); ok && b.team == team {
		return banner
	}
	return actingBanner{team: team, below: banner}
}

// isActingBanner reports whether the layout should draw a banner as the acting
// strip, which spans the column itself, rather than inside the padded box a
// SlotProvider's banner sits in.
func isActingBanner(c templ.Component) bool {
	_, ok := c.(actingBanner)
	return ok
}

// impersonationAction is how the record names what happened.
func impersonationAction(action string) string {
	if action == account.ImpersonationStart {
		return "Started acting"
	}
	return "Stopped acting"
}
