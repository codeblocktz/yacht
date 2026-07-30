package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func testServer(t *testing.T, opts Options) http.Handler {
	t.Helper()
	if opts.Orchestrator == nil {
		opts.Orchestrator = orchestrator.NewNoop()
	}
	if opts.Identity == nil {
		opts.Identity = identity.NewSingleOwner(identity.Owner{
			ID: "owner-1", DisplayName: "Eric",
		})
	}
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{Identity: identity.NewSingleOwner(identity.Owner{ID: "x"})}); err == nil {
		t.Error("expected an error when no Orchestrator is supplied")
	}
	if _, err := New(Options{Orchestrator: orchestrator.NewNoop()}); err == nil {
		t.Error("expected an error when no identity Provider is supplied")
	}
}

func TestOverviewRendersWithDefaultChrome(t *testing.T) {
	h := testServer(t, Options{})
	rec := get(t, h, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{"Overview", "Yacht", "Apps", "Settings", "Eric"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// The point of the whole seam: an application wrapping the engine changes the
// entire shell — brand, navigation, header, sidebar, banner — by supplying a
// SlotProvider. No file in this package is edited, and nothing is forked.
func TestSlotProviderReplacesChrome(t *testing.T) {
	custom := SlotProviderFunc(func(_ context.Context, _ *http.Request) Slots {
		return Slots{
			Title:     "Acme Cloud",
			BrandName: "Acme Cloud",
			BrandHref: "/dashboard",
			Nav: []NavGroup{
				{Items: []NavItem{
					{Label: "Projects", Href: "/projects", Active: true},
					{Label: "Usage", Href: "/usage"},
				}},
			},
			SidebarTop:    templ.Raw(`<div id="org-switcher">Acme Ltd</div>`),
			HeaderTools:   templ.Raw(`<span id="balance">TZS 42,000</span>`),
			SidebarFooter: templ.Raw(`<a id="support" href="/support">Support</a>`),
			Banner:        templ.Raw(`<div id="notice">Balance is low.</div>`),
		}
	})

	h := testServer(t, Options{Slots: custom})
	body := get(t, h, "/").Body.String()

	// Injected chrome is present.
	for _, want := range []string{
		"Acme Cloud", "org-switcher", "Acme Ltd",
		"balance", "TZS 42,000", "support", "notice", "Projects", "Usage",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("custom chrome missing %q", want)
		}
	}

	// Engine defaults are gone, not merely appended to.
	for _, unwanted := range []string{`>Overview</a>`, `href="/settings"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("default chrome %q survived a custom SlotProvider", unwanted)
		}
	}

	// The page itself still renders — chrome and content are independent.
	if !strings.Contains(body, "Signed in as") {
		t.Error("page content should be unaffected by the chrome")
	}
}

// Every slot must be optional, or a wrapping application is forced to supply
// components it does not want.
func TestSlotsAreOptional(t *testing.T) {
	bare := SlotProviderFunc(func(context.Context, *http.Request) Slots {
		return Slots{Title: "Bare", BrandName: "Bare", BrandHref: "/"}
	})

	h := testServer(t, Options{Slots: bare})
	rec := get(t, h, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Bare") {
		t.Error("brand missing")
	}
	// Empty slots render nothing rather than an empty wrapper.
	//
	// Matches the rendered markup, not the bare class name: the stylesheet
	// declares these classes, so a substring search for "slot-banner" alone
	// finds the CSS rule and reports a failure that is not real.
	for _, marker := range []string{
		`class="slot slot-header-tools"`,
		`class="slot slot-banner"`,
		`class="slot slot-sidebar-top"`,
	} {
		if strings.Contains(body, marker) {
			t.Errorf("unused slot should not render a wrapper: found %s", marker)
		}
	}
}

func TestAuthenticatedRoutesRequireIdentity(t *testing.T) {
	denied := identity.ProviderFunc(func(context.Context, *http.Request) (identity.Owner, error) {
		return identity.Owner{}, identity.ErrUnauthenticated
	})
	h := testServer(t, Options{Identity: denied})

	for _, path := range []string{"/", "/apps", "/settings"} {
		if code := get(t, h, path).Code; code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, code)
		}
	}
}

// Health must not require credentials — a load balancer has none.
func TestHealthIsUnauthenticated(t *testing.T) {
	denied := identity.ProviderFunc(func(context.Context, *http.Request) (identity.Owner, error) {
		return identity.Owner{}, identity.ErrUnauthenticated
	})
	h := testServer(t, Options{Identity: denied})

	rec := get(t, h, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
}

func TestHealthReportsClusterFailure(t *testing.T) {
	h := testServer(t, Options{Orchestrator: newFailingOrchestrator()})

	rec := get(t, h, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "degraded") {
		t.Errorf("body = %q, want it to report degraded", rec.Body.String())
	}
}

// A broken cluster must still render a usable page saying so, rather than
// erroring out — that page is where a self-hoster fixes their kubeconfig.
func TestOverviewShowsClusterError(t *testing.T) {
	h := testServer(t, Options{Orchestrator: newFailingOrchestrator()})

	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Cluster unreachable") {
		t.Error("expected the page to report the cluster as unreachable")
	}
	if !strings.Contains(body, "no route to cluster") {
		t.Error("expected the underlying error to be surfaced to the operator")
	}
	// The records themselves are still real, so the page must still render
	// rather than becoming an error page.
	if !strings.Contains(body, "Overview") {
		t.Error("page content should still render when the cluster is down")
	}
}

func TestSettingsShowsOwner(t *testing.T) {
	h := testServer(t, Options{})
	body := get(t, h, "/settings").Body.String()
	if !strings.Contains(body, "owner-1") {
		t.Error("settings should show the owner id")
	}
}

func TestDefaultSlotsMarkActiveNav(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	s := DefaultSlots{}.Slots(context.Background(), req)

	var checked bool
	for _, group := range s.Nav {
		for _, item := range group.Items {
			if item.Href == "/apps" {
				checked = true
				if !item.Active {
					t.Error("/apps nav item should be active on the apps page")
				}
			}
			if item.Href == "/" && item.Active {
				t.Error("overview should not be active on the apps page")
			}
		}
	}
	if !checked {
		t.Fatal("no /apps nav item found")
	}
}

// failingOrchestrator stands in for an unreachable cluster: it behaves
// normally except that Ping always fails.
//
// The embedded Noop is initialised rather than left nil so that a future test
// calling any other method gets sensible behaviour instead of a confusing nil
// dereference.
type failingOrchestrator struct{ *orchestrator.Noop }

func newFailingOrchestrator() failingOrchestrator {
	return failingOrchestrator{Noop: orchestrator.NewNoop()}
}

func (failingOrchestrator) Ping(context.Context) error {
	return errors.New("no route to cluster")
}
