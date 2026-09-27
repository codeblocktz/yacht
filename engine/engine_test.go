package engine

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The wrapper story, end to end: an application composes the engine with its
// own brand, its own routes inside the engine's gates, and its own migration
// step, and gets the engine's dashboard around all of it.
func TestAWrapperComposesTheEngineWithItsOwnChromeAndRoutes(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run engine composition tests")
	}
	ctx := context.Background()

	migrated := false
	// Declared before New so the route closures below can reach the engine;
	// they run at request time, by which point it is built.
	var e *Engine
	var err error
	e, err = New(ctx, Config{
		DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
		OwnerID: "kilicore-test", OwnerName: "Kilicore Test", MaxConcurrentBuilds: 2,
	}, Overrides{
		Version: "v9.9.9-test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		// No cluster in a test; the engine's own fallback would also do this,
		// after trying and logging a failure to connect.
		Orchestrator: orchestrator.NewNoop(),
		AfterMigrate: func(_ context.Context, pool *pgxpool.Pool) error {
			migrated = pool != nil
			return nil
		},
		// The engine's chrome, adjusted rather than replaced: the brand and
		// one extra navigation group, everything else as the engine draws it.
		Slots: SlotProviderFunc(func(ctx context.Context, r *http.Request) Slots {
			s := DefaultSlots{}.Slots(ctx, r)
			s.BrandName, s.BrandHref = "Kilicore", "/"
			s.Nav = append(s.Nav, NavGroup{Heading: "Account", Items: []NavItem{
				{Label: "Billing", Href: "/billing", Active: strings.HasPrefix(r.URL.Path, "/billing")},
			}})
			// An install-wide page of its own goes into the engine's Admin
			// group, found by its heading, rather than a second group beside it.
			for i := range s.Nav {
				if s.Nav[i].Heading == AdminNavHeading {
					s.Nav[i].Items = append(s.Nav[i].Items, NavItem{Label: "Plans", Href: "/admin/plans"})
				}
			}
			return s
		}),
		Extra: ExtraRoutes{
			Operator: func(r chi.Router) {
				r.Get("/admin/plans", func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, "plans")
				})
			},
			Member: func(r chi.Router) {
				r.Get("/billing", func(w http.ResponseWriter, r *http.Request) {
					owner := MustOwnerFromContext(r.Context())
					page := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
						_, err := io.WriteString(w, "<h1>Billing for "+owner.ID+"</h1>")
						return err
					})
					// Through the engine's renderer, so the page sits inside
					// the same layout as the engine's own.
					e.Server.Render(w, r, page)
				})
			},
			Owner: func(r chi.Router) {
				r.Post("/billing/plan", func(w http.ResponseWriter, r *http.Request) {
					e.Server.FlashOK(w, r, "Plan changed.")
					http.Redirect(w, r, "/billing", http.StatusSeeOther)
				})
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	if !migrated {
		t.Error("AfterMigrate did not run")
	}
	h := e.Handler()

	home := get(t, h, "/")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Kilicore") {
		t.Fatalf("GET / = %d, want the wrapper's brand on the engine's own page", home.Code)
	}
	if !strings.Contains(home.Body.String(), `href="/billing"`) {
		t.Error("the wrapper's navigation is not in the sidebar")
	}
	// One principal and no accounts: that principal runs the install, so the
	// Admin group is theirs, with the engine's pages and the wrapper's in it.
	for _, href := range []string{`href="/admin/teams"`, `href="/admin/capacity"`, `href="/admin/plans"`} {
		if !strings.Contains(home.Body.String(), href) {
			t.Errorf("the Admin group is missing %s", href)
		}
	}
	for _, path := range []string{"/admin/teams", "/admin/capacity", "/admin/plans"} {
		if rec := get(t, h, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}

	billing := get(t, h, "/billing")
	body := billing.Body.String()
	if billing.Code != http.StatusOK {
		t.Fatalf("GET /billing = %d, want 200", billing.Code)
	}
	if !strings.Contains(body, "Billing for kilicore-test") {
		t.Error("the wrapper's page did not see the owner the engine resolved")
	}
	if !strings.Contains(body, "Kilicore") || !strings.Contains(body, "Overview") {
		t.Error("the wrapper's page is not inside the engine's layout")
	}

	// An owner route is behind the engine's CSRF check like any other post.
	req := httptest.NewRequest(http.MethodPost, "/billing/plan", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("origin-less POST to a wrapper route = %d, want 403 from the engine's CSRF gate", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/billing/plan", nil)
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("POST to a wrapper's owner route = %d, want 303", rec.Code)
	}
}

// The engine with no overrides is exactly cmd/yacht.
func TestTheEngineRunsWithNoOverrides(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run engine composition tests")
	}
	e, err := New(context.Background(), Config{
		DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
		OwnerID: "owner-local", OwnerName: "Local", MaxConcurrentBuilds: 2,
	}, Overrides{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Orchestrator: orchestrator.NewNoop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	if rec := get(t, e.Handler(), "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Yacht") {
		t.Fatalf("GET / = %d, want the engine's own brand", rec.Code)
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}
