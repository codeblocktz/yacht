package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/github"
	"github.com/codeblocktz/yacht/internal/identity"
)

// GitHub is the install's GitHub App, as the dashboard drives it: a team
// connecting installations on its Team page, and the pickers that choose a
// repository from them.
type GitHub interface {
	GitHubInstallURL(ownerID, user string) (string, error)
	GitHubAuthorizeURL(ownerID, user string) (string, error)
	GitHubConnections(ctx context.Context, ownerID string) ([]app.GitHubConnection, error)

	// CompleteGitHubConnect is the return from GitHub; ChooseGitHubInstallation
	// picks one of several it offered. Both check the state against the
	// session's team and person, and GitHub's own list of what the person
	// reaches, themselves.
	CompleteGitHubConnect(
		ctx context.Context, ownerID, user, email, state, code string, installationID int64,
	) (app.GitHubConnect, error)
	ChooseGitHubInstallation(ctx context.Context, ownerID, user, email, choice string) (app.GitHubConnection, error)
	DisconnectGitHub(ctx context.Context, ownerID string, installationID int64) error

	GitHubRepos(ctx context.Context, ownerID string) ([]app.GitHubRepo, []string, error)
	GitHubBranches(ctx context.Context, ownerID string, ref app.GitHubRef, query string) ([]string, error)
	GitHubDirectories(ctx context.Context, ownerID string, ref app.GitHubRef, path string) ([]string, error)
}

// GitHubCallbackPath is where GitHub sends a person back after installing the
// App or authorizing it: the App's Callback URL, under YACHT_BASE_URL.
// GitHubHookPath is the App's webhook URL, under the same.
const (
	GitHubCallbackPath = "/github/callback"
	GitHubHookPath     = "/hooks/github"
)

// githubPerson is who is connecting: the session's user, by id for the state
// and by address for the record of who did it. From the cookie and nothing
// the request says, like every other decision about a session.
func (s *Server) githubPerson(r *http.Request) (user, email string, ok bool) {
	if s.accounts == nil {
		return "", "", false
	}
	sess, err := s.accounts.ResolveSession(r.Context(), sessionToken(r))
	if err != nil {
		return "", "", false
	}
	email = ""
	if u, err := s.accounts.User(r.Context(), sess.UserID); err == nil {
		email = u.Email
	}
	return sess.UserID.String(), email, true
}

// githubInstall sends a person to GitHub to install the App on an account of
// theirs, or to change which repositories an installation reaches.
//
// A GET, followed as a link: the page's form-action policy keeps a form from
// submitting anywhere but here, and a redirect after a POST counts as the
// form's. Nothing changes here — the link it builds only works for this
// person, in this team, on the way back.
func (s *Server) githubInstall(w http.ResponseWriter, r *http.Request) {
	s.githubRedirect(w, r, s.github.GitHubInstallURL)
}

// githubAuthorize is the same for an account that already has the App.
func (s *Server) githubAuthorize(w http.ResponseWriter, r *http.Request) {
	s.githubRedirect(w, r, s.github.GitHubAuthorizeURL)
}

func (s *Server) githubRedirect(
	w http.ResponseWriter, r *http.Request, link func(ownerID, user string) (string, error),
) {
	owner := identity.MustFromContext(r.Context())
	user, _, ok := s.githubPerson(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	target, err := link(owner.ID, user)
	if err != nil {
		s.flashErr(w, r, githubSentence(err))
		http.Redirect(w, r, "/team", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// githubCallback is the return from GitHub, with the code it issued the
// person, the state the link carried, and the installation it names when
// they installed or changed one.
//
// Everything that decides whether an installation is connected happens in
// the service: the state against this session, the installation against
// GitHub's answer to the code. What arrives in this query string is a claim.
func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	q := r.URL.Query()

	// Cancelled on GitHub, or GitHub refusing: it says so instead of a code.
	if q.Get("error") != "" || q.Get("code") == "" {
		s.flashErr(w, r, "GitHub did not connect — nothing was changed.")
		http.Redirect(w, r, "/team", http.StatusSeeOther)
		return
	}
	var installationID int64
	if raw := strings.TrimSpace(q.Get("installation_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			s.flashErr(w, r, "GitHub sent back an installation this cannot read.")
			http.Redirect(w, r, "/team", http.StatusSeeOther)
			return
		}
		installationID = id
	}
	user, email, ok := s.githubPerson(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	got, err := s.github.CompleteGitHubConnect(ctx, owner.ID, user, email,
		q.Get("state"), q.Get("code"), installationID)
	if err != nil {
		s.log.Warn("GitHub connect refused", slog.String("team", owner.ID),
			slog.Int64("installation", installationID), slog.String("error", err.Error()))
		s.flashErr(w, r, githubSentence(err))
		http.Redirect(w, r, "/team", http.StatusSeeOther)
		return
	}
	if len(got.Choices) > 0 {
		s.render(w, r, GitHubChoosePage(got.Choices))
		return
	}
	s.flashOK(w, r, "GitHub connected: "+got.Connected.Account+". Its repositories can be chosen when deploying.")
	http.Redirect(w, r, "/team", http.StatusSeeOther)
}

// githubChoose connects the installation picked from several.
func (s *Server) githubChoose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	user, email, ok := s.githubPerson(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := s.github.ChooseGitHubInstallation(ctx, owner.ID, user, email, r.FormValue("choice"))
	if err != nil {
		s.flashErr(w, r, githubSentence(err))
		http.Redirect(w, r, "/team", http.StatusSeeOther)
		return
	}
	s.flashOK(w, r, "GitHub connected: "+c.Account+". Its repositories can be chosen when deploying.")
	http.Redirect(w, r, "/team", http.StatusSeeOther)
}

// githubDisconnect lets go of an installation for this team.
func (s *Server) githubDisconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.github.DisconnectGitHub(ctx, owner.ID, id); err != nil {
		if errors.Is(err, app.ErrGitHubNotConnected) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("disconnect GitHub", slog.String("error", err.Error()))
		s.flashErr(w, r, "Could not disconnect GitHub.")
		http.Redirect(w, r, "/team", http.StatusSeeOther)
		return
	}
	s.flashOK(w, r, "GitHub disconnected. Apps built through it keep running on their last image, and build again once it is connected.")
	http.Redirect(w, r, "/team", http.StatusSeeOther)
}

// GitHubRepoData is the repository picker and what it offers.
type GitHubRepoData struct {
	Repos    []app.GitHubRepo
	Problems []string

	// Selected is the option chosen already, and SelectedLabel what to call
	// it when the installations no longer list it.
	Selected      string
	SelectedLabel string

	// Error is that the list could not be read at all.
	Error string
}

// githubRepositories is the picker's options, read when the form shows it.
func (s *Server) githubRepositories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	d := GitHubRepoData{
		// The select's own value, which htmx sends with the request it makes
		// for its options.
		Selected:      strings.TrimSpace(r.URL.Query().Get("github_repo")),
		SelectedLabel: strings.TrimSpace(r.URL.Query().Get("label")),
	}
	repos, problems, err := s.github.GitHubRepos(ctx, owner.ID)
	if err != nil {
		s.log.Error("list GitHub repositories", slog.String("error", err.Error()))
		d.Error = "Could not read the repositories"
	}
	d.Repos, d.Problems = repos, problems
	if err := GitHubRepoSelect(d).Render(ctx, w); err != nil {
		s.log.Error("render GitHub repositories", slog.String("error", err.Error()))
	}
}

// githubBranches is the branch picker for a chosen repository.
func (s *Server) githubBranches(w http.ResponseWriter, r *http.Request) {
	ref, err := app.ParseGitHubRef(r.URL.Query().Get("github_repo"))
	if err != nil || !ref.Set() {
		_ = BranchOptions(BranchData{}).Render(r.Context(), w)
		return
	}
	s.renderGitHubBranches(w, r, ref)
}

func (s *Server) renderGitHubBranches(w http.ResponseWriter, r *http.Request, ref app.GitHubRef) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	query := strings.TrimSpace(r.URL.Query().Get("repo_branch"))
	d := BranchData{Searched: query != ""}
	if branches, err := s.github.GitHubBranches(ctx, owner.ID, ref, query); err != nil {
		d.Error = "Could not read that repository's branches"
	} else {
		d.Branches = branches
	}
	if err := BranchOptions(d).Render(ctx, w); err != nil {
		s.log.Error("render branches", slog.String("error", err.Error()))
	}
}

// githubDirectories is the directory picker for a chosen repository.
func (s *Server) githubDirectories(w http.ResponseWriter, r *http.Request) {
	ref, err := app.ParseGitHubRef(r.URL.Query().Get("github_repo"))
	if err != nil || !ref.Set() {
		_ = DirectoryOptions(DirectoryData{}).Render(r.Context(), w)
		return
	}
	s.renderGitHubDirectories(w, r, ref)
}

func (s *Server) renderGitHubDirectories(w http.ResponseWriter, r *http.Request, ref app.GitHubRef) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	path := strings.Trim(strings.TrimSpace(r.URL.Query().Get("repo_subdir")), "/")
	d := DirectoryData{Path: path, Parent: parentPath(path), Searched: true}
	if dirs, err := s.github.GitHubDirectories(ctx, owner.ID, ref, path); err != nil {
		d.Error = "Could not read that repository"
	} else {
		d.Directories = dirs
	}
	if err := DirectoryOptions(d).Render(ctx, w); err != nil {
		s.log.Error("render directories", slog.String("error", err.Error()))
	}
}

// formGitHubRef reads the repository picker from a form. Malformed is an
// error rather than a pasted URL: a picker that sent garbage is not a
// choice to quietly ignore.
func formGitHubRef(r *http.Request) (app.GitHubRef, error) {
	return app.ParseGitHubRef(r.FormValue("github_repo"))
}

// githubSentence is a refusal as a page says it.
func githubSentence(err error) string {
	switch {
	case errors.Is(err, github.ErrStateInvalid):
		return "That GitHub link has expired or was not made here. Start again from the Team page."
	case errors.Is(err, github.ErrNotAuthorized):
		return "GitHub did not accept the sign-in. Start again from the Team page."
	case errors.Is(err, github.ErrInstallationGone):
		return "The GitHub App is no longer installed on that account."
	}
	msg := strings.TrimPrefix(strings.TrimPrefix(err.Error(), "app: "), "github: ")
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

// TeamGitHubData is the GitHub panel on the Team page.
type TeamGitHubData struct {
	Connections []app.GitHubConnection
	CanManage   bool
	Error       string
}

// teamGitHub loads the panel, nil on an install with no GitHub App.
func (s *Server) teamGitHub(ctx context.Context, ownerID string, canManage bool) *TeamGitHubData {
	if s.github == nil {
		return nil
	}
	d := &TeamGitHubData{CanManage: canManage}
	conns, err := s.github.GitHubConnections(ctx, ownerID)
	if err != nil {
		s.log.Error("list GitHub connections", slog.String("error", err.Error()))
		d.Error = "Could not read this team's GitHub connections."
	}
	d.Connections = conns
	return d
}

// pickedGitHubRepo is the repository an app's pickers read through the App:
// the one chosen in the form being edited, or — when the form names neither
// a GitHub repository nor a URL — the one the app has.
func (s *Server) pickedGitHubRepo(r *http.Request, a app.App) (app.GitHubRef, bool) {
	if s.github == nil {
		return app.GitHubRef{}, false
	}
	q := r.URL.Query()
	if _, sent := q["github_repo"]; sent {
		ref, err := app.ParseGitHubRef(q.Get("github_repo"))
		return ref, err == nil && ref.Set()
	}
	if strings.TrimSpace(q.Get("repo_url")) == "" && a.Repo.GitHub.Set() {
		return a.Repo.GitHub, true
	}
	return app.GitHubRef{}, false
}
