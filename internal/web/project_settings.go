package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// projectAdmin is what the project settings page needs beyond the Apps
// interface. Asserted rather than added to it, so a wrapping application's
// own implementation keeps compiling; without it the page shows and does
// not offer the changes.
type projectAdmin interface {
	RenameProject(ctx context.Context, ownerID, slug, name string) (app.Project, error)
	DeleteProject(ctx context.Context, ownerID, slug string) error
}

// projectSettings draws a project's settings.
func (s *Server) projectSettings(w http.ResponseWriter, r *http.Request) {
	s.renderProjectSettings(w, r, http.StatusOK, "")
}

func (s *Server) renderProjectSettings(w http.ResponseWriter, r *http.Request, status int, fail string) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	if s.apps == nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.apps.Project(ctx, owner.ID, chi.URLParam(r, "slug"))
	if err != nil {
		if errors.Is(err, app.ErrProjectNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("get project", slog.String("error", err.Error()))
		http.Error(w, "could not load the project", http.StatusInternalServerError)
		return
	}
	apps, err := s.apps.ListInProject(ctx, owner.ID, p.ID)
	if err != nil {
		s.log.Warn("list the project's apps", slog.String("error", err.Error()))
	}
	_, admin := s.apps.(projectAdmin)
	if role, ok := s.roleOf(r); !ok || !role.AtLeast(account.RoleAdmin) {
		admin = false
	}
	s.renderStatus(w, r, status, ProjectSettings(ProjectSettingsData{
		Project: p, Apps: len(apps), Error: fail, CanAdmin: admin,
	}))
}

// projectRename changes a project's display name.
func (s *Server) projectRename(w http.ResponseWriter, r *http.Request) {
	pa, ok := s.apps.(projectAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	owner := identity.MustFromContext(r.Context())
	slug := chi.URLParam(r, "slug")
	if _, err := pa.RenameProject(r.Context(), owner.ID, slug, r.FormValue("name")); err != nil {
		s.renderProjectSettings(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.flashOK(w, r, "Renamed.")
	http.Redirect(w, r, "/projects/"+slug+"/settings", http.StatusSeeOther)
}

// projectDelete deletes a project and every app on it, once its name has
// been typed back — checked here, because a dialog is JavaScript and the
// form posts without it.
func (s *Server) projectDelete(w http.ResponseWriter, r *http.Request) {
	pa, ok := s.apps.(projectAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug := chi.URLParam(r, "slug")
	p, err := s.apps.Project(ctx, owner.ID, slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.TrimSpace(r.FormValue("confirm")) != p.Name {
		s.renderProjectSettings(w, r, http.StatusUnprocessableEntity,
			"Type the project's name to confirm — deleting it deletes every app on it.")
		return
	}
	if err := pa.DeleteProject(ctx, owner.ID, slug); err != nil {
		s.log.Error("delete project", slog.String("project", slug), slog.String("error", err.Error()))
		s.renderProjectSettings(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.flashOK(w, r, p.Name+" deleted.")
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}
