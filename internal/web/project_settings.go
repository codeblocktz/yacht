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
	s.renderProjectSettingsWith(w, r, status, fail, nil)
}

func (s *Server) renderProjectSettingsWith(
	w http.ResponseWriter, r *http.Request, status int, fail string, decorate func(*ProjectSettingsData),
) {
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
	role, ok := s.roleOf(r)
	roleAdmin := ok && role.AtLeast(account.RoleAdmin)
	_, admin := s.apps.(projectAdmin)
	d := ProjectSettingsData{Project: p, Apps: len(apps), Error: fail, CanAdmin: admin && roleAdmin, RoleAdmin: roleAdmin}
	d.Usage = s.projectUsage(ctx, owner.ID, p, apps)
	s.attachSharedVariables(r, owner.ID, &d)
	_, d.PROn = s.apps.(prEnvironments)
	if es, ok := s.environments(); ok {
		d.EnvironmentsOn = true
		if envs, err := es.Environments(ctx, owner.ID, p.Slug); err == nil {
			d.Environments = envs
		} else {
			s.log.Warn("list environments", slog.String("error", err.Error()))
		}
	}
	if decorate != nil {
		decorate(&d)
	}
	s.renderStatus(w, r, status, ProjectSettings(d))
}

// environmentCreate makes an environment, empty or a copy of another, and
// shows what became of each service it copied.
func (s *Server) environmentCreate(w http.ResponseWriter, r *http.Request) {
	es, ok := s.environments()
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug := chi.URLParam(r, "slug")
	from := strings.TrimSpace(r.FormValue("from"))
	switch from {
	case "-":
		from = ""
	case "":
		from = app.DefaultEnvironment
	}
	env, copies, err := es.CreateEnvironment(ctx, owner.ID, slug, r.FormValue("name"), from)
	if err != nil && env.Slug == "" {
		s.renderProjectSettingsWith(w, r, http.StatusUnprocessableEntity, err.Error(), nil)
		return
	}
	if err != nil {
		s.log.Warn("copy environment", slog.String("environment", env.Slug), slog.String("error", err.Error()))
	}
	rememberEnv(w, slug, env.Slug)
	if len(copies) == 0 {
		s.flashOK(w, r, env.Name+" created.")
		http.Redirect(w, r, envHref(slug, env.Slug), http.StatusSeeOther)
		return
	}
	s.renderProjectSettingsWith(w, r, http.StatusOK, "", func(d *ProjectSettingsData) {
		d.Copied, d.CopiedEnv = copies, env.Name
	})
}

// environmentDelete deletes an environment and its services.
func (s *Server) environmentDelete(w http.ResponseWriter, r *http.Request) {
	es, ok := s.environments()
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug, env := chi.URLParam(r, "slug"), chi.URLParam(r, "env")
	if err := es.DeleteEnvironment(ctx, owner.ID, slug, env); err != nil {
		s.renderProjectSettingsWith(w, r, http.StatusUnprocessableEntity, err.Error(), nil)
		return
	}
	rememberEnv(w, slug, app.DefaultEnvironment)
	s.flashOK(w, r, env+" deleted.")
	http.Redirect(w, r, "/projects/"+slug+"/settings#environments", http.StatusSeeOther)
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

// prEnvironments is the PR Environments toggle, asserted like the rest.
type prEnvironments interface {
	SetPREnvironments(ctx context.Context, ownerID, projectSlug string, enabled bool) error
}

// projectPREnvironments turns a project's pull-request environments on or off.
func (s *Server) projectPREnvironments(w http.ResponseWriter, r *http.Request) {
	pe, ok := s.apps.(prEnvironments)
	if !ok {
		http.NotFound(w, r)
		return
	}
	owner := identity.MustFromContext(r.Context())
	slug := chi.URLParam(r, "slug")
	enabled := formChecked(r, "enabled")
	if err := pe.SetPREnvironments(r.Context(), owner.ID, slug, enabled); err != nil {
		s.renderProjectSettingsWith(w, r, http.StatusUnprocessableEntity, err.Error(), nil)
		return
	}
	if enabled {
		s.flashOK(w, r, "PR Environments on. The GitHub App has to send pull request events for them to start.")
	} else {
		s.flashOK(w, r, "PR Environments off.")
	}
	http.Redirect(w, r, "/projects/"+slug+"/settings#environments", http.StatusSeeOther)
}
