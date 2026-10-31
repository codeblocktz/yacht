package web

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A focused page is the signed-in person's, with one thing to do on it: no
// sidebar and no top bar, but the brand, the way back and the page.
func TestRenderFocusedDropsTheChromeAndKeepsTheWayBack(t *testing.T) {
	var srv *Server
	srv, err := New(Options{
		Identity:     identity.NewSingleOwner(identity.Owner{ID: "owner-1", DisplayName: "Eric"}),
		Orchestrator: orchestrator.NewNoop(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Extra: ExtraRoutes{Member: func(r chi.Router) {
			r.Get("/billing/checkout", func(w http.ResponseWriter, r *http.Request) {
				srv.RenderFocused(w, r, http.StatusOK, "Checkout",
					Crumb{Label: "Back to billing", Href: "/billing"}, templ.Raw("<p>pay here</p>"))
			})
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, srv.Handler(), "/billing/checkout")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "pay here") {
		t.Fatalf("GET /billing/checkout = %d", rec.Code)
	}
	for _, chrome := range []string{`id="sidebar"`, `class="topbar"`} {
		if strings.Contains(body, chrome) {
			t.Errorf("a focused page draws %s", chrome)
		}
	}
	for _, want := range []string{`class="focus-bar"`, `href="/billing"`, "Back to billing", "<title>Checkout"} {
		if !strings.Contains(body, want) {
			t.Errorf("a focused page lacks %s", want)
		}
	}
}
