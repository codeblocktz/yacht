package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func TestSecurityHeadersProtectDynamicResponses(t *testing.T) {
	s := &Server{slots: DefaultSlots{}}
	h := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))

	wants := map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
		"X-Frame-Options":        "DENY",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
	}
	for name, want := range wants {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
}

func TestAssetHandlerCanOverrideDynamicNoStore(t *testing.T) {
	s := &Server{}
	h := s.securityHeaders(assetHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/css/app.css", nil))

	if got := rec.Header().Get("Cache-Control"); got == "no-store" || got == "" {
		t.Errorf("asset Cache-Control = %q, want public caching", got)
	}
}

func TestCSRFRejectsForeignBrowserOrigins(t *testing.T) {
	s := &Server{baseURL: "https://yacht.example.com"}
	called := false
	h := s.csrfProtect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "https://yacht.example.com/apps/web/delete", nil)
	req.Header.Set("Origin", "https://hostile.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("foreign-origin request reached the mutating handler")
	}
}

func TestCSRFAcceptsSameOriginAndNonBrowserRequests(t *testing.T) {
	for _, origin := range []string{"https://yacht.example.com", ""} {
		t.Run(origin, func(t *testing.T) {
			s := &Server{baseURL: "https://yacht.example.com"}
			called := false
			h := s.csrfProtect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodPost, "https://yacht.example.com/apps/web/redeploy", nil)
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if !called || rec.Code != http.StatusNoContent {
				t.Fatalf("called=%v status=%d, want true/204", called, rec.Code)
			}
		})
	}
}

func TestCSRFRejectsSessionCookieWithoutOriginMetadata(t *testing.T) {
	s := &Server{baseURL: "https://yacht.example.com"}
	called := false
	h := s.csrfProtect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "https://yacht.example.com/apps/web/delete", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "session"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("originless cookie request reached the mutating handler")
	}
}

func TestRenderStatusSetsHTMLTypeBeforeWritingStatus(t *testing.T) {
	s := &Server{slots: DefaultSlots{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	page := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "<!doctype html><p>refused</p>")
		return err
	})

	s.renderStatus(rec, req, http.StatusUnprocessableEntity, page)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
}
