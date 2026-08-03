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

func TestCSRFAcceptsSameOrigin(t *testing.T) {
	s := &Server{baseURL: "https://yacht.example.com"}
	called := false
	h := s.csrfProtect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "https://yacht.example.com/apps/web/redeploy", nil)
	req.Header.Set("Origin", "https://yacht.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("called=%v status=%d, want true/204", called, rec.Code)
	}
}

func TestCSRFAcceptsOriginlessCookieFreeSignIn(t *testing.T) {
	s := &Server{baseURL: "https://yacht.example.com"}
	called := false
	h := s.csrfProtect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "https://yacht.example.com/sign-in", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("called=%v status=%d, want true/204", called, rec.Code)
	}
}

func TestCSRFUsesRequestOriginWithoutConfiguredBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string
		want   int
	}{
		{name: "same origin", origin: "http://yacht.test", want: http.StatusNoContent},
		{name: "foreign origin", origin: "https://hostile.example.com", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			h := s.csrfProtect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodPost, "http://yacht.test/apps/web/delete", nil)
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
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

func TestCSRFRejectsOriginlessCookieFreeMutationOutsideSignIn(t *testing.T) {
	s := &Server{baseURL: "https://yacht.example.com"}
	called := false
	h := s.csrfProtect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "https://yacht.example.com/apps/web/delete", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("originless cookie-free request reached a non-sign-in mutation")
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
