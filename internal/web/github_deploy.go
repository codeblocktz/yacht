package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// NewAppGitHubData is the one-click deploy page: the team's repositories,
// each a button that deploys it.
type NewAppGitHubData struct {
	Project, ProjectName string
	// Query filters the list, as somebody types.
	Query    string
	Repos    []app.GitHubRepo
	Problems []string
	// Connected is false when the team has no GitHub connection to list
	// anything from.
	Connected bool
	Error     string
}

// appNewGitHub is the one-click deploy page.
func (s *Server) appNewGitHub(w http.ResponseWriter, r *http.Request) {
	d := s.githubDeployData(r, r.URL.Query().Get("project"), r.URL.Query().Get("q"))
	s.render(w, r, NewAppGitHub(d))
}

// appNewGitHubRepos is the list alone, as the search box asks for it.
func (s *Server) appNewGitHubRepos(w http.ResponseWriter, r *http.Request) {
	d := s.githubDeployData(r, r.URL.Query().Get("project"), r.URL.Query().Get("q"))
	if err := githubDeployRows(d).Render(r.Context(), w); err != nil {
		s.log.Error("render GitHub repositories", slog.String("error", err.Error()))
	}
}

// appDeployGitHub deploys the repository chosen, into the project, and
// sends somebody to it building.
func (s *Server) appDeployGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	project := strings.TrimSpace(r.FormValue("project"))
	fail := func(msg string) {
		d := s.githubDeployData(r, project, "")
		d.Error = msg
		s.renderStatus(w, r, http.StatusUnprocessableEntity, NewAppGitHub(d))
	}
	ref, err := app.ParseGitHubRef(r.FormValue("github_repo"))
	if err != nil || !ref.Set() {
		fail("Choose a repository.")
		return
	}
	projectID, err := s.projectID(ctx, owner.ID, project)
	if err != nil {
		fail(err.Error())
		return
	}
	a, err := s.github.DeployFromGitHub(ctx, owner.ID, ref, projectID)
	if err != nil {
		s.log.Warn("deploy from GitHub failed", slog.String("repository", ref.String()), slog.String("error", err.Error()))
		fail(githubSentence(err))
		return
	}
	auditTarget(ctx, a.Name)
	http.Redirect(w, r, "/apps/"+a.Name, http.StatusSeeOther)
}

func (s *Server) githubDeployData(r *http.Request, project, query string) NewAppGitHubData {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	d := NewAppGitHubData{Query: strings.TrimSpace(query)}
	d.Project, d.ProjectName = s.namedProject(r, project)
	conns, err := s.github.GitHubConnections(ctx, owner.ID)
	if err != nil {
		s.log.Error("GitHub connections", slog.String("error", err.Error()))
		d.Error = "Could not read the team's GitHub connections."
		return d
	}
	for _, c := range conns {
		if c.Usable() {
			d.Connected = true
		}
	}
	if !d.Connected {
		return d
	}
	repos, problems, err := s.github.GitHubRepos(ctx, owner.ID)
	if err != nil {
		s.log.Error("list GitHub repositories", slog.String("error", err.Error()))
		d.Error = "Could not read the repositories."
	}
	d.Problems = problems
	q := strings.ToLower(d.Query)
	for _, repo := range repos {
		if q == "" || strings.Contains(strings.ToLower(repo.FullName), q) {
			d.Repos = append(d.Repos, repo)
		}
	}
	return d
}

// namedProject is a project's slug and name as a page shows them, or both
// empty for the default project or one that cannot be found: a link to a
// project deleted since is still a deploy, into the default one.
func (s *Server) namedProject(r *http.Request, slug string) (string, string) {
	slug = strings.TrimSpace(slug)
	if slug == "" || slug == app.DefaultProjectSlug || s.apps == nil {
		return "", ""
	}
	owner := identity.MustFromContext(r.Context())
	p, err := s.apps.Project(r.Context(), owner.ID, slug)
	if err != nil {
		return "", ""
	}
	return p.Slug, p.Name
}

// projectID is the project an app is created in: the one named, or the
// default one when none is.
func (s *Server) projectID(ctx context.Context, ownerID, slug string) (uuid.UUID, error) {
	if slug == "" || slug == app.DefaultProjectSlug {
		return uuid.Nil, nil
	}
	p, err := s.apps.Project(ctx, ownerID, slug)
	if errors.Is(err, app.ErrProjectNotFound) {
		return uuid.Nil, errors.New("that project no longer exists; choose another")
	}
	if err != nil {
		return uuid.Nil, err
	}
	return p.ID, nil
}

// attachProjects gives the settings the team's projects, to move the app
// between. Read only for the settings tab, and left out on failure: the
// rest of the page does not need it.
func (s *Server) attachProjects(ctx context.Context, d *AppDetailData) {
	if d.Tab != "settings" || s.apps == nil {
		return
	}
	projects, err := s.apps.Projects(ctx, d.App.OwnerID)
	if err != nil {
		s.log.Warn("list projects", slog.String("error", err.Error()))
		return
	}
	d.Projects = projects
}

// appMoveProject moves an app to another of the team's projects.
func (s *Server) appMoveProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	slug := strings.TrimSpace(r.FormValue("project"))
	p, err := s.apps.MoveToProject(ctx, owner.ID, name, slug)
	switch {
	case errors.Is(err, app.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, app.ErrProjectNotFound):
		s.flashErr(w, r, "That project no longer exists.")
	case err != nil:
		s.log.Error("move app to project", slog.String("app", name), slog.String("error", err.Error()))
		s.flashErr(w, r, "Could not move the app.")
	default:
		s.flashOK(w, r, "Moved to "+p.Name+".")
	}
	http.Redirect(w, r, "/apps/"+name+"/settings#project", http.StatusSeeOther)
}
