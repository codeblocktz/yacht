package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Hooks is deploy on push: a webhook per app that a Git host calls.
type Hooks interface {
	GitHook(ctx context.Context, ownerID, name string) (app.Hook, error)
	EnableGitHook(ctx context.Context, ownerID, name string) (string, error)
	DisableGitHook(ctx context.Context, ownerID, name string) error
	DeliverPush(ctx context.Context, appID uuid.UUID, p app.Push) (string, error)
}

// hookPath is where every delivery arrives, followed by the app's id. The
// prefix is what csrfProtect exempts, so nothing but deliveries may live here.
const hookPath = "/hooks/apps/"

// maxPushBody caps a delivery. A push event is a few kilobytes; GitHub's own
// ceiling is 25 MB, and nothing about deploying needs that much read.
const maxPushBody = 1 << 20

// hookURL is the address a Git host is given, on the dashboard's public URL
// when there is one. Without one it is this request's own origin, which the
// page says may not be reachable from outside.
func (s *Server) hookURL(r *http.Request, appID uuid.UUID) string {
	base := s.baseURL
	if base == "" {
		base = requestOrigin(r)
	}
	return base + hookPath + appID.String() + "/push"
}

// hookDeliver takes one delivery from a Git host.
//
// The answer is plain text because the host shows it beside the delivery, and
// a person debugging a hook reads it there: "ignored a push to feature — this
// app builds main" is the whole diagnosis.
func (s *Server) hookDeliver(w http.ResponseWriter, r *http.Request) {
	appID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "unknown webhook", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPushBody))
	if err != nil {
		http.Error(w, "the delivery is too large", http.StatusRequestEntityTooLarge)
		return
	}
	push := app.Push{
		Event:     firstHeader(r, "X-GitHub-Event", "X-Gitea-Event", "X-Gogs-Event", "X-Gitlab-Event"),
		Signature: firstHeader(r, "X-Hub-Signature-256", "X-Gitea-Signature"),
		Token:     r.Header.Get("X-Gitlab-Token"),
		Body:      body,
	}

	result, err := s.hooks.DeliverPush(r.Context(), appID, push)
	switch {
	case errors.Is(err, app.ErrHookUnauthorized):
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	case errors.Is(err, app.ErrNotGitApp):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	case err != nil:
		s.log.Error("webhook delivery", slog.String("app_id", appID.String()),
			slog.String("error", err.Error()))
		http.Error(w, "the delivery could not be processed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, result+"\n")
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// hookEnable turns deploy on push on, or issues a new secret.
//
// The secret is rendered straight into this response rather than carried
// through a redirect. It is shown once — the page never reads it back — so a
// redirect would lose it, and a cookie would be one more place it was stored.
func (s *Server) hookEnable(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")

	secret, err := s.hooks.EnableGitHook(r.Context(), owner.ID, name)
	if err != nil {
		if errors.Is(err, app.ErrNoSecretKey) {
			err = errors.New("deploy on push needs YACHT_SECRET_KEY set, to seal the webhook's secret")
		}
		s.appActionFailed(w, r, name, "settings", err)
		return
	}
	s.renderCanvasWith(w, r, "", name, "settings", func(d *AppDetailData) {
		d.HookSecret = secret
		d.Notice = "Deploy on push is on. Copy the secret now — it is not shown again."
	})
}

func (s *Server) hookDisable(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")

	if err := s.hooks.DisableGitHook(r.Context(), owner.ID, name); err != nil {
		s.appActionFailed(w, r, name, "settings", err)
		return
	}
	s.flashOK(w, r, "Deploy on push is off. The Git host's webhook will be refused from now on.")
	http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
}

// attachHook adds deploy on push to the panel, for an app built from a
// repository on an install that offers it.
func (s *Server) attachHook(ctx context.Context, r *http.Request, d *AppDetailData) {
	if s.hooks == nil || d.App.Source != app.SourceGit {
		return
	}
	hook, err := s.hooks.GitHook(ctx, d.App.OwnerID, d.App.Name)
	if err != nil {
		s.log.Error("read webhook", slog.String("app", d.App.Name), slog.String("error", err.Error()))
		return
	}
	d.HooksOn = true
	d.Hook = hook
	d.HookURL = s.hookURL(r, d.App.ID)
	d.HookURLPublic = s.baseURL != ""
}
