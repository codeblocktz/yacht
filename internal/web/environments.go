package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
)

// environmentService is what the environment switcher and Project Settings ›
// Environments need from the app service. Asserted rather than added to
// Apps, so a wrapping application's own implementation keeps compiling;
// without it there is production and nothing to switch to.
type environmentService interface {
	Environments(ctx context.Context, ownerID, projectSlug string) ([]app.Environment, error)
	ListInEnvironment(ctx context.Context, ownerID string, p app.Project, env string) ([]app.App, error)
	HasEnvironment(ctx context.Context, ownerID string, p app.Project, env string) (bool, error)
	CreateEnvironment(ctx context.Context, ownerID, projectSlug, name, from string) (app.Environment, []app.EnvironmentCopy, error)
	DeleteEnvironment(ctx context.Context, ownerID, projectSlug, slug string) error
}

func (s *Server) environments() (environmentService, bool) {
	es, ok := s.apps.(environmentService)
	return es, ok
}

// envCookie remembers, per project, the environment somebody last looked
// at, as Railway does: coming back to a project's canvas, or adding a
// service to it, is in the environment they were working in.
func envCookie(projectSlug string) string { return "yacht_env_" + projectSlug }

// chosenEnv is the environment a request means for a project: the one it
// names, or the one remembered for the project, or production.
func chosenEnv(r *http.Request, projectSlug string) string {
	if env := strings.TrimSpace(r.URL.Query().Get("environment")); env != "" {
		return env
	}
	if env := strings.TrimSpace(r.FormValue("environment")); env != "" {
		return env
	}
	if c, err := r.Cookie(envCookie(projectSlug)); err == nil && c.Value != "" {
		return c.Value
	}
	return app.DefaultEnvironment
}

func rememberEnv(w http.ResponseWriter, projectSlug, env string) {
	http.SetCookie(w, &http.Cookie{
		Name: envCookie(projectSlug), Value: env, Path: "/",
		MaxAge: int((90 * 24 * time.Hour).Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// inEnvironment runs a create handler in the environment of the project it
// creates into, so the app is that environment's by whichever form made it.
func (s *Server) inEnvironment(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := strings.TrimSpace(r.FormValue("project"))
		if project == "" {
			project = app.DefaultProjectSlug
		}
		if env := chosenEnv(r, project); env != app.DefaultEnvironment {
			r = r.WithContext(app.WithEnvironment(r.Context(), env))
		}
		next(w, r)
	}
}

// envHref is a project's canvas in one of its environments.
func envHref(projectSlug, env string) string {
	return "/projects/" + projectSlug + "?environment=" + env
}
