package web

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// FormRefusal is a form on a service's Settings that the service refused:
// which one, why, and what was typed into it, so the form is drawn again with
// the reason beside it and the text still in it. The Variables tab keeps its
// own (VariablesView.Failed), for the Raw Editor's line.
type FormRefusal struct {
	// Form is which form, one of the settingsForm names; empty puts the
	// reason at the top of the tab, for a form that has no place of its own.
	Form   string
	Reason string
	typed  url.Values
}

// The Settings forms a refusal is drawn beside.
const (
	settingsFormDomain   = "domain"          // Add custom domain
	settingsFormPublic   = "public-domain"   // Edit domain or port
	settingsFormGenerate = "generate-domain" // Generate Domain
	settingsFormReplicas = "replicas"
	settingsFormVolume   = "volume"  // Add a volume
	settingsFormVolumes  = "volumes" // a volume's Grow or Delete
)

// On says this refusal is form's.
func (f FormRefusal) On(form string) bool { return f.Reason != "" && f.Form == form }

// Typed is what was typed into field of form, when form is the one refused,
// and otherwise fallback: the value the form is drawn with on any visit.
func (f FormRefusal) Typed(form, field, fallback string) string {
	if !f.On(form) {
		return fallback
	}
	return f.typed.Get(field)
}

// swapRefusal is what a Settings form needs to be answered in place: posted
// through htmx, a refusal (422) swaps the tab alone where it was, so the
// address stays the tab's and a reload posts nothing. forms.js lets the 422
// swap for a form marked data-swap-refusal. Without htmx the form posts as
// before and gets the whole canvas.
func swapRefusal(action string) templ.Attributes {
	return templ.Attributes{
		"hx-post":           action,
		"hx-target":         "#settings-tab",
		"hx-swap":           "outerHTML",
		"data-swap-refusal": true,
	}
}

// settingsRefused answers a refused Settings form with the Settings tab, as
// any visit draws it on the canvas, with the reason beside the form and what
// was typed into it still there. A request from htmx gets the tab alone.
//
// It replaces an answer that drew the app's old page of its own: no canvas,
// no panel, and the field emptied (CONFIG-15), which is what CONFIG-1 was for
// the Variables tab.
func (s *Server) settingsRefused(
	w http.ResponseWriter, r *http.Request, appName, tab, form string, status int, cause error,
) {
	if errors.Is(cause, app.ErrNotFound) {
		http.Error(w, cause.Error(), http.StatusNotFound)
		return
	}
	_ = r.ParseForm()
	refused := func(d *AppDetailData) {
		d.Refused = FormRefusal{
			Form:   form,
			Reason: strings.TrimPrefix(cause.Error(), "app: "),
			typed:  r.PostForm,
		}
	}

	if r.Header.Get("HX-Request") != "true" {
		s.renderCanvasStatus(w, r, status, "", appName, tab, refused)
		return
	}
	ctx := r.Context()
	a, err := s.apps.Get(ctx, identity.MustFromContext(ctx).ID, appName)
	if err != nil {
		http.Error(w, cause.Error(), status)
		return
	}
	d := s.appPanel(ctx, a, tab)
	s.attachHook(ctx, r, d)
	refused(d)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := appSettings(*d).Render(ctx, w); err != nil {
		s.log.Error("render settings tab", slog.String("error", err.Error()))
	}
}

// savedTo sends somebody on after a save. A save from htmx is told to load
// the page, rather than have the redirect's page swapped into the tab, so
// the canvas's changes bar and the flash are drawn as on any load.
func savedTo(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}
