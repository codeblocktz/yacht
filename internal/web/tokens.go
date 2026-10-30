package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/apitoken"
	"github.com/codeblocktz/yacht/internal/identity"
)

// mountMemberTokens puts Team › API tokens and the CLI's approval page on
// the member group. Member, because a person signing their own CLI in is
// the commonest reason to make one; what a token may do is bounded by the
// service, never by the form.
func (s *Server) mountMemberTokens(r chi.Router) {
	r.Get("/team/tokens", s.teamTokens)
	r.Post("/team/tokens", s.teamTokenCreate)
	r.Post("/team/tokens/{id}/revoke", s.teamTokenRevoke)
	r.Get("/cli/authorize", s.cliAuthorize)
	r.Post("/cli/authorize", s.cliAuthorizeDecide)
}

// NewToken is a token just made, shown on the page that made it and never
// again: the response is the only place it exists. CLI is the product's CLI,
// for saying how to use it.
type NewToken struct {
	Name  string
	Value string
	CLI   string
}

// TokenEnv is the variable the CLI reads a token from in CI.
func (t NewToken) TokenEnv() string {
	return strings.ToUpper(strings.ReplaceAll(t.CLI, "-", "_")) + "_TOKEN"
}

// TeamTokensData is Team › API tokens.
type TeamTokensData struct {
	TeamName string
	Viewer   account.Role
	ViewerID uuid.UUID
	// Acting is an operator acting as the team: not a member of it, so a
	// token they made would resolve to nothing.
	Acting  bool
	Tokens  []apitoken.Token
	New     *NewToken
	Error   string
	BaseURL string
	Tabs    TeamTabs
}

// TeamTabs is which of the team's pages exist, for the tabs above them.
type TeamTabs struct {
	Active string
	Tokens bool
	Audit  bool
}

func (s *Server) teamTabs(r *http.Request, active string) TeamTabs {
	t := TeamTabs{Active: active, Tokens: s.tokens != nil}
	if s.audit != nil {
		if role, ok := s.roleOf(r); ok && role.CanAdminister() {
			t.Audit = true
		}
	}
	return t
}

func (s *Server) teamTokens(w http.ResponseWriter, r *http.Request) {
	s.renderTeamTokens(w, r, http.StatusOK, nil, "")
}

func (s *Server) renderTeamTokens(w http.ResponseWriter, r *http.Request, status int, made *NewToken, fail string) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tokens, err := s.tokens.List(ctx, owner.ID)
	if err != nil {
		s.log.Error("list api tokens", slog.String("error", err.Error()))
		http.Error(w, "could not load the tokens", http.StatusInternalServerError)
		return
	}
	d := TeamTokensData{
		TeamName: displayName(owner), Viewer: sess.Role, ViewerID: sess.UserID,
		Acting: sess.ActingTeamID == owner.ID, Tokens: tokens, New: made, Error: fail,
		BaseURL: s.baseURL, Tabs: s.teamTabs(r, "tokens"),
	}
	s.renderStatus(w, r, status, TeamTokensPage(d))
}

// teamTokenCreate makes a team token and shows it once, on the page this
// answers with rather than across a redirect: a flash is a cookie, and a
// token in a cookie is a token stored somewhere.
func (s *Server) teamTokenCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if sess.ActingTeamID == owner.ID {
		s.renderTeamTokens(w, r, http.StatusForbidden, nil,
			"You are acting as this team, not a member of it — a token you made would not work.")
		return
	}
	raw, tok, err := s.tokens.Create(ctx, apitoken.CreateInput{
		OwnerID: owner.ID, Name: r.FormValue("name"),
		Role: account.Role(strings.TrimSpace(r.FormValue("role"))), Actor: sess.UserID,
	})
	if err != nil {
		markAuditFailed(ctx)
		s.renderTeamTokens(w, r, tokenStatus(err), nil, tokenMessage(err))
		return
	}
	s.renderTeamTokens(w, r, http.StatusOK, &NewToken{Name: tok.Name, Value: raw, CLI: s.cliName}, "")
}

func (s *Server) teamTokenRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "no such token", http.StatusNotFound)
		return
	}
	tok, err := s.tokens.Revoke(ctx, owner.ID, sess.UserID, id)
	if err != nil {
		markAuditFailed(ctx)
		s.renderTeamTokens(w, r, tokenStatus(err), nil, tokenMessage(err))
		return
	}
	auditTarget(ctx, tok.Name)
	s.flashOK(w, r, "Revoked "+tok.Name+". Anything using it is refused from now on.")
	http.Redirect(w, r, "/team/tokens", http.StatusSeeOther)
}

func tokenStatus(err error) int {
	switch {
	case errors.Is(err, apitoken.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, apitoken.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, apitoken.ErrName), errors.Is(err, apitoken.ErrRole):
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

func tokenMessage(err error) string {
	switch {
	case errors.Is(err, apitoken.ErrForbidden):
		return "Only an admin can do that to somebody else's token."
	case errors.Is(err, apitoken.ErrNotFound):
		return "That token does not exist any more."
	case errors.Is(err, apitoken.ErrName):
		return "Give the token a name of 1 to 60 characters, so you can tell it apart later."
	case errors.Is(err, apitoken.ErrRole):
		return "A token is a member's or an admin's, and can be no stronger than your own role."
	}
	return "Something went wrong — try again."
}

// projectTokens is Project Settings › Tokens: the project's tokens and, just
// after making one, the token itself.
func (s *Server) projectTokens(r *http.Request, p app.Project, made *NewToken) *ProjectTokensData {
	if s.tokens == nil || s.accounts == nil {
		return nil
	}
	owner := identity.MustFromContext(r.Context())
	all, err := s.tokens.List(r.Context(), owner.ID)
	if err != nil {
		s.log.Warn("list project tokens", slog.String("error", err.Error()))
	}
	d := &ProjectTokensData{New: made, BaseURL: s.baseURL}
	for _, t := range all {
		if t.ProjectID != nil && *t.ProjectID == p.ID {
			d.Tokens = append(d.Tokens, t)
		}
	}
	return d
}

func (s *Server) projectTokenCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	p, err := s.apps.Project(ctx, owner.ID, chi.URLParam(r, "slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw, tok, err := s.tokens.Create(ctx, apitoken.CreateInput{
		OwnerID: owner.ID, ProjectID: &p.ID, Name: r.FormValue("name"),
		Role: account.Role(strings.TrimSpace(r.FormValue("role"))), Actor: sess.UserID,
	})
	if err != nil {
		markAuditFailed(ctx)
		s.renderProjectSettingsWith(w, r, tokenStatus(err), tokenMessage(err), projectExtras{})
		return
	}
	s.renderProjectSettingsWith(w, r, http.StatusOK, "", projectExtras{token: &NewToken{Name: tok.Name, Value: raw, CLI: s.cliName}})
}

func (s *Server) projectTokenRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug := chi.URLParam(r, "slug")
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	tok, err := s.tokens.Revoke(ctx, owner.ID, sess.UserID, id)
	if err != nil {
		s.flashErr(w, r, tokenMessage(err))
	} else {
		auditTarget(ctx, tok.Name)
		s.flashOK(w, r, "Revoked "+tok.Name+".")
	}
	http.Redirect(w, r, "/projects/"+slug+"/settings#tokens", http.StatusSeeOther)
}

// CLIAuthorizeData is the page a CLI's login opens in the browser.
type CLIAuthorizeData struct {
	Code     string
	Login    *apitoken.Login
	TeamName string
	Viewer   account.Role
	Acting   bool
	// Done is the outcome once a decision is made: "approved" or "denied".
	Done  string
	Error string
}

// cliAuthorize shows a pending login: the code, what the CLI says it is
// running on, and which team and role approving would give it. The person
// is asked to check the code matches their terminal — the one thing that
// tells their own login from somebody else's sent to them as a link.
func (s *Server) cliAuthorize(w http.ResponseWriter, r *http.Request) {
	s.renderCLIAuthorize(w, r, http.StatusOK, apitoken.NormaliseUserCode(r.URL.Query().Get("code")), "", "")
}

func (s *Server) renderCLIAuthorize(w http.ResponseWriter, r *http.Request, status int, code, done, fail string) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	d := CLIAuthorizeData{
		Code: code, TeamName: displayName(owner), Viewer: sess.Role,
		Acting: sess.ActingTeamID == owner.ID, Done: done, Error: fail,
	}
	if code != "" && done == "" {
		if l, err := s.tokens.LoginByCode(ctx, code); err == nil {
			d.Login = &l
		} else if fail == "" {
			d.Error = "That code has expired or was already used. Run login again in your terminal."
			status = http.StatusNotFound
		}
	}
	s.renderStatus(w, r, status, CLIAuthorizePage(d))
}

func (s *Server) cliAuthorizeDecide(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	sess, err := s.accounts.ResolveSession(ctx, sessionToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	code := apitoken.NormaliseUserCode(r.FormValue("code"))
	if r.FormValue("decision") != "approve" {
		if err := s.tokens.DenyLogin(ctx, code); err != nil {
			s.renderCLIAuthorize(w, r, http.StatusNotFound, code, "",
				"That code has expired or was already used.")
			return
		}
		s.renderCLIAuthorize(w, r, http.StatusOK, code, "denied", "")
		return
	}
	if sess.ActingTeamID == owner.ID {
		markAuditFailed(ctx)
		s.renderCLIAuthorize(w, r, http.StatusForbidden, code, "",
			"You are acting as this team, not a member of it — sign a CLI in from a team of your own.")
		return
	}
	role := account.Role(strings.TrimSpace(r.FormValue("role")))
	if err := s.tokens.ApproveLogin(ctx, code, sess.UserID, owner.ID, role); err != nil {
		markAuditFailed(ctx)
		msg := "That code has expired or was already used. Run login again in your terminal."
		if errors.Is(err, apitoken.ErrRole) || errors.Is(err, apitoken.ErrForbidden) {
			msg = "A CLI can be given a member's or an admin's role, and no more than your own."
		}
		s.renderCLIAuthorize(w, r, http.StatusUnprocessableEntity, code, "", msg)
		return
	}
	s.renderCLIAuthorize(w, r, http.StatusOK, code, "approved", "")
}
