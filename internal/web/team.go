package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/notify"
)

// TeamChoice is one team a person may act as, as the chrome needs it.
//
// Public because the switcher is chrome, and chrome is the seam a wrapping
// application replaces: its own SlotProvider reads the same list out of the
// request and renders it however its product does.
type TeamChoice struct {
	ID   string
	Name string
	Role account.Role

	// Active marks the team the session is acting as — the owner every query on
	// this request is scoped by.
	Active bool
}

// teamsKey addresses the switcher's data on the request. An unexported struct
// type, so nothing outside this package can plant a list of teams on a context
// and have the chrome offer them.
type teamsKey struct{}

// teamLister defers the lookup until something actually renders the chrome.
type teamLister func() []TeamChoice

// TeamsFromContext returns the teams the request's own session may act as.
//
// Empty on an install with no account service, and for a request with no
// session: in both cases there is nothing to switch between.
func TeamsFromContext(ctx context.Context) []TeamChoice {
	list, _ := ctx.Value(teamsKey{}).(teamLister)
	if list == nil {
		return nil
	}
	return list()
}

// withTeams makes the switcher's data available to whatever renders the chrome.
//
// The lookup is deferred rather than done here: it costs a query, and most
// requests through this group are POSTs that redirect and never render a
// sidebar at all. Deferring means only the responses that show the switcher pay
// for it.
func (s *Server) withTeams(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			once  sync.Once
			teams []TeamChoice
		)
		list := teamLister(func() []TeamChoice {
			once.Do(func() { teams = s.teamsFor(r) })
			return teams
		})
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), teamsKey{}, list)))
	})
}

// teamsFor lists the teams the request's session belongs to.
//
// Resolved from the presented cookie and nothing else. The session says who the
// person is; the memberships say which teams are theirs. A list built from
// anything the request asserted would be a list of teams to try switching into.
func (s *Server) teamsFor(r *http.Request) []TeamChoice {
	if s.accounts == nil {
		return nil
	}
	ctx := r.Context()

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		if !errors.Is(err, account.ErrSessionInvalid) {
			s.log.Error("resolve session for the team switcher",
				slog.String("error", err.Error()))
		}
		return nil
	}

	memberships, err := s.accounts.TeamsFor(ctx, sess.UserID)
	if err != nil {
		// The switcher disappears rather than the page failing: it is chrome, and
		// a dashboard that 500s because one control could not be drawn is a worse
		// answer than a dashboard with no switcher on it.
		s.log.Error("list teams for the switcher",
			slog.String("user", sess.UserID.String()), slog.String("error", err.Error()))
		return nil
	}

	teams := make([]TeamChoice, 0, len(memberships))
	for _, m := range memberships {
		name := m.TeamName
		if name == "" {
			name = m.TeamID
		}
		teams = append(teams, TeamChoice{
			ID:     m.TeamID,
			Name:   name,
			Role:   m.Role,
			Active: m.TeamID == sess.ActiveTeamID,
		})
	}
	return teams
}

// activeTeamName labels the closed switcher.
//
// The first team is the fallback for a session whose active team is somehow not
// in the list: a switcher whose label is blank looks like a broken control, and
// the list underneath is still correct.
func activeTeamName(teams []TeamChoice) string {
	for _, t := range teams {
		if t.Active {
			return t.Name
		}
	}
	if len(teams) > 0 {
		return teams[0].Name
	}
	return ""
}

// teamSwitch points this browser's session at another of the person's teams.
//
// Membership is verified by account.SwitchTeam, against the user the session
// resolves to rather than against anything in the form. That check is not
// repeated here and must not be bypassed here: the team id arrives from the
// request, and it is the only part of this that a caller chooses.
func (s *Server) teamSwitch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Which session moves comes from the cookie. A session id read from the form
	// would be a way to move somebody else's session into a team of your
	// choosing.
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		if !errors.Is(err, account.ErrSessionInvalid) {
			s.log.Error("resolve session to switch team", slog.String("error", err.Error()))
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	team := strings.TrimSpace(r.FormValue("team"))
	if err := s.accounts.SwitchTeam(ctx, sess.ID, team); err != nil {
		switch {
		case errors.Is(err, account.ErrNotAMember):
			// Said plainly, and it discloses nothing: the caller already named the
			// team, and the only thing they learn is that they are not in it.
			http.Error(w, "forbidden", http.StatusForbidden)
		case errors.Is(err, account.ErrSessionInvalid):
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			s.log.Error("switch team",
				slog.String("team", team), slog.String("error", err.Error()))
			http.Error(w, "could not switch team", http.StatusInternalServerError)
		}
		return
	}

	s.log.Info("switched team",
		slog.String("user", sess.UserID.String()), slog.String("team", team))

	// Back to the overview rather than to the page they were on: every other page
	// is scoped by an owner that has just changed, and an app detail page in the
	// team they left is now a 404 they did not ask for.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// TeamPageData is the team management page.
type TeamPageData struct {
	TeamID   string
	TeamName string

	// Viewer is the role of the person looking at the page. The page offers
	// only the controls their role can actually use — an admin is not shown a
	// role selector they would be refused for using.
	Viewer  account.Role
	ViewerID uuid.UUID

	Members     []account.Member
	Invitations []account.Invitation

	// Error is a message from the action that redirected here, if it failed.
	Error string
}

// teamPage shows who is in the team and who has been invited.
func (s *Server) teamPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	members, err := s.accounts.ListMembers(ctx, owner.ID)
	if err != nil {
		s.log.Error("list members", slog.String("error", err.Error()))
		http.Error(w, "could not load the team", http.StatusInternalServerError)
		return
	}

	invitations, err := s.accounts.ListPendingInvitations(ctx, owner.ID)
	if err != nil {
		s.log.Error("list invitations", slog.String("error", err.Error()))
		http.Error(w, "could not load the team", http.StatusInternalServerError)
		return
	}

	s.render(w, r, TeamPage(TeamPageData{
		TeamID:      owner.ID,
		TeamName:    displayName(owner),
		Viewer:      sess.Role,
		ViewerID:    sess.UserID,
		Members:     members,
		Invitations: invitations,
		Error:       r.URL.Query().Get("error"),
	}))
}

// teamInvite mails an invitation.
//
// The token exists in exactly one place — the message — and is never rendered,
// logged, or returned. That is the same position the invited person is in, and
// it is what stops an administrator joining as somebody else.
func (s *Server) teamInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	email := strings.TrimSpace(r.FormValue("email"))
	role := account.Role(strings.TrimSpace(r.FormValue("role")))

	raw, err := s.accounts.Invite(ctx, sess.UserID, owner.ID, email, role, s.inviteTTL())
	if err != nil {
		s.teamActionFailed(w, r, err, "invite")
		return
	}

	// Mailed after the row is written, so a delivery failure leaves an
	// invitation the page can offer to revoke rather than a token nobody holds.
	msg := notify.Message{
		To:      email,
		Subject: "You have been invited to " + displayName(owner) + " on Yacht",
		TextBody: "You have been invited to join " + displayName(owner) +
			" as " + string(role) + ".\n\n" +
			s.baseURL + "/invitations/" + raw + "\n\n" +
			"The link expires. If you were not expecting this, ignore it.",
	}
	if err := s.mailer.Send(ctx, msg); err != nil {
		// Logged, not surfaced: the invitation is real and revocable either
		// way, and the address is not ours to report on.
		s.log.Error("send invitation",
			slog.String("team", owner.ID), slog.String("error", err.Error()))
	}

	http.Redirect(w, r, "/team", http.StatusSeeOther)
}

// teamRevokeInvitation withdraws an invitation.
func (s *Server) teamRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "no such invitation", http.StatusNotFound)
		return
	}

	if err := s.accounts.RevokeInvitation(ctx, sess.UserID, owner.ID, id); err != nil {
		s.teamActionFailed(w, r, err, "revoke invitation")
		return
	}
	http.Redirect(w, r, "/team", http.StatusSeeOther)
}

// teamRemoveMember takes someone out of the team.
func (s *Server) teamRemoveMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	target, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "no such member", http.StatusNotFound)
		return
	}

	if err := s.accounts.RemoveMember(ctx, sess.UserID, owner.ID, target); err != nil {
		s.teamActionFailed(w, r, err, "remove member")
		return
	}
	http.Redirect(w, r, "/team", http.StatusSeeOther)
}

// teamSetRole changes what someone may do.
func (s *Server) teamSetRole(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	target, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "no such member", http.StatusNotFound)
		return
	}

	role := account.Role(strings.TrimSpace(r.FormValue("role")))
	if err := s.accounts.SetRole(ctx, sess.UserID, owner.ID, target, role); err != nil {
		s.teamActionFailed(w, r, err, "set role")
		return
	}
	http.Redirect(w, r, "/team", http.StatusSeeOther)
}

// teamActionFailed maps an account error to a response.
//
// Refusals are their own status rather than a redirect carrying a message: a
// caller that was forbidden should see 403, and a test that asserts the rule
// should not have to read a query parameter to find out whether it held.
func (s *Server) teamActionFailed(
	w http.ResponseWriter, r *http.Request, err error, what string,
) {
	switch {
	case errors.Is(err, account.ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, account.ErrLastOwner):
		http.Error(w, "a team must keep at least one owner", http.StatusConflict)
	case errors.Is(err, account.ErrNotAMember):
		http.Error(w, "not a member of this team", http.StatusNotFound)
	case errors.Is(err, account.ErrInvitationNotFound):
		http.Error(w, "no such invitation", http.StatusNotFound)
	case errors.Is(err, account.ErrSessionInvalid):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	default:
		s.log.Error(what, slog.String("error", err.Error()))
		http.Error(w, "could not "+what, http.StatusInternalServerError)
	}
}

// inviteTTL is how long an invitation stays redeemable.
//
// Longer than a sign-in link, because the recipient may not be expecting it and
// may not read mail the same day, and shorter than a session because it is a
// bearer token sitting in somebody's inbox.
func (s *Server) inviteTTL() time.Duration { return 7 * 24 * time.Hour }
