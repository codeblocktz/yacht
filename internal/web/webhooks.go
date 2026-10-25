package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/webhook"
)

// Webhooks is what Project Settings › Webhooks needs of the webhook service.
type Webhooks interface {
	Configured() bool
	Create(ctx context.Context, ownerID string, projectID uuid.UUID, actor uuid.UUID, in webhook.Input) (webhook.Webhook, string, error)
	List(ctx context.Context, ownerID string, projectID uuid.UUID) ([]webhook.Webhook, error)
	Get(ctx context.Context, ownerID string, id uuid.UUID) (webhook.Webhook, error)
	Delete(ctx context.Context, ownerID string, id uuid.UUID) (webhook.Webhook, error)
	Test(ctx context.Context, ownerID string, id uuid.UUID) (webhook.Delivery, error)
	Deliveries(ctx context.Context, ownerID string, webhookID uuid.UUID, limit int) ([]webhook.Delivery, error)
}

var _ Webhooks = (*webhook.Service)(nil)

// mountWebhooks is the admin's: a webhook hands an outside system a running
// account of the project, and its URL is often its credential.
func (s *Server) mountWebhooks(r chi.Router) {
	r.Post("/projects/{slug}/webhooks", s.webhookCreate)
	r.Get("/projects/{slug}/webhooks/{id}", s.webhookDetail)
	r.Post("/projects/{slug}/webhooks/{id}/delete", s.webhookDelete)
	r.Post("/projects/{slug}/webhooks/{id}/test", s.webhookTest)
}

// NewWebhook is a webhook just made, with its signing secret, shown on the
// page that made it and never again.
type NewWebhook struct {
	URL    string
	Secret string
}

// ProjectWebhooksData is Project Settings › Webhooks.
type ProjectWebhooksData struct {
	Hooks []webhook.Webhook
	// Recent is each webhook's latest delivery, by id, for a line under it.
	Recent     map[uuid.UUID]webhook.Delivery
	New        *NewWebhook
	Configured bool
	Events     []webhook.EventInfo
}

func (s *Server) projectWebhooks(r *http.Request, p app.Project, made *NewWebhook) *ProjectWebhooksData {
	if s.webhooks == nil {
		return nil
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	d := &ProjectWebhooksData{
		New: made, Configured: s.webhooks.Configured(), Events: webhook.Events,
		Recent: map[uuid.UUID]webhook.Delivery{},
	}
	hooks, err := s.webhooks.List(ctx, owner.ID, p.ID)
	if err != nil {
		s.log.Warn("list webhooks", slog.String("error", err.Error()))
	}
	d.Hooks = hooks
	for _, h := range hooks {
		if ds, err := s.webhooks.Deliveries(ctx, owner.ID, h.ID, 1); err == nil && len(ds) > 0 {
			d.Recent[h.ID] = ds[0]
		}
	}
	return d
}

// projectFor is the project a webhook route names, in the requesting team.
func (s *Server) projectFor(w http.ResponseWriter, r *http.Request) (app.Project, bool) {
	owner := identity.MustFromContext(r.Context())
	p, err := s.apps.Project(r.Context(), owner.ID, chi.URLParam(r, "slug"))
	if err != nil {
		http.NotFound(w, r)
		return app.Project{}, false
	}
	return p, true
}

// webhookOf is a webhook the route names, which must be the project's: an
// id from another project, or another team, is a 404.
func (s *Server) webhookOf(w http.ResponseWriter, r *http.Request, p app.Project) (webhook.Webhook, bool) {
	owner := identity.MustFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return webhook.Webhook{}, false
	}
	h, err := s.webhooks.Get(r.Context(), owner.ID, id)
	if err != nil || h.ProjectID != p.ID {
		http.NotFound(w, r)
		return webhook.Webhook{}, false
	}
	return h, true
}

func (s *Server) webhookCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	p, ok := s.projectFor(w, r)
	if !ok {
		return
	}
	_ = r.ParseForm()
	in := webhook.Input{URL: r.FormValue("url"), Events: r.Form["events"]}
	names, values := r.Form["header_name"], r.Form["header_value"]
	for i := range names {
		if i < len(values) {
			in.Headers = append(in.Headers, webhook.Header{Name: names[i], Value: values[i]})
		}
	}
	var actor uuid.UUID
	if s.accounts != nil {
		if sess, err := s.accounts.ResolveSession(ctx, sessionToken(r)); err == nil {
			actor = sess.UserID
		}
	}
	h, secret, err := s.webhooks.Create(ctx, owner.ID, p.ID, actor, in)
	if err != nil {
		markAuditFailed(ctx)
		var bad webhook.InputError
		status, msg := http.StatusInternalServerError, "The webhook could not be saved."
		switch {
		case errors.As(err, &bad):
			status, msg = http.StatusUnprocessableEntity, bad.Reason
		case errors.Is(err, webhook.ErrNoKey):
			status, msg = http.StatusNotImplemented, "This install has no secret key to seal a webhook's secret with."
		default:
			s.log.Error("create webhook", slog.String("error", err.Error()))
		}
		s.renderProjectSettingsWith(w, r, status, msg, projectExtras{})
		return
	}
	s.renderProjectSettingsWith(w, r, http.StatusOK, "", projectExtras{webhook: &NewWebhook{URL: h.URL, Secret: secret}})
}

func (s *Server) webhookDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectFor(w, r)
	if !ok {
		return
	}
	h, ok := s.webhookOf(w, r, p)
	if !ok {
		return
	}
	owner := identity.MustFromContext(r.Context())
	if _, err := s.webhooks.Delete(r.Context(), owner.ID, h.ID); err != nil {
		s.flashErr(w, r, "The webhook could not be deleted.")
	} else {
		s.flashOK(w, r, "Webhook deleted.")
	}
	http.Redirect(w, r, "/projects/"+p.Slug+"/settings#webhooks", http.StatusSeeOther)
}

// webhookTest is the Test Webhook button: a test event, sent now, with what
// came back said on the page.
func (s *Server) webhookTest(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectFor(w, r)
	if !ok {
		return
	}
	h, ok := s.webhookOf(w, r, p)
	if !ok {
		return
	}
	owner := identity.MustFromContext(r.Context())
	d, err := s.webhooks.Test(r.Context(), owner.ID, h.ID)
	switch {
	case err != nil:
		s.log.Error("test webhook", slog.String("error", err.Error()))
		s.flashErr(w, r, "The test could not be sent.")
	case d.Status == webhook.StatusSucceeded:
		s.flashOK(w, r, "Test delivered — answered "+strconv.Itoa(*d.LastStatus)+".")
	case d.LastStatus != nil:
		// Not marked as a failed action: the test was sent, and what the
		// receiver said is the answer the button asked for.
		s.flashWarn(w, r, "Test sent, and answered "+strconv.Itoa(*d.LastStatus)+". "+d.LastError)
	default:
		s.flashWarn(w, r, "Test sent, and got no answer: "+d.LastError)
	}
	http.Redirect(w, r, "/projects/"+p.Slug+"/webhooks/"+h.ID.String(), http.StatusSeeOther)
}

// WebhookDetailData is one webhook's deliveries log.
type WebhookDetailData struct {
	Project    app.Project
	Hook       webhook.Webhook
	Deliveries []webhook.Delivery
}

func (s *Server) webhookDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectFor(w, r)
	if !ok {
		return
	}
	h, ok := s.webhookOf(w, r, p)
	if !ok {
		return
	}
	owner := identity.MustFromContext(r.Context())
	ds, err := s.webhooks.Deliveries(r.Context(), owner.ID, h.ID, 50)
	if err != nil {
		s.log.Error("list webhook deliveries", slog.String("error", err.Error()))
	}
	s.render(w, r, WebhookDetailPage(WebhookDetailData{Project: p, Hook: h, Deliveries: ds}))
}
