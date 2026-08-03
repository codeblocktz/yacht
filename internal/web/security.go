package web

import (
	"net/http"
	"net/url"
	"strings"
)

// securityHeaders applies browser protections to every response. Individual
// handlers may replace Cache-Control — assets do, because their fingerprinted
// URLs are deliberately immutable — while dynamic responses retain no-store.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'self'",
			"base-uri 'self'",
			"connect-src 'self'",
			"form-action 'self'",
			"frame-ancestors 'none'",
			"img-src 'self' data:",
			"object-src 'none'",
			// The current layout contains small pre-paint and interaction scripts,
			// and charts use inline sizing. Moving those into nonce-bearing assets
			// can tighten these two directives without weakening today's UI.
			"script-src 'self' 'unsafe-inline'",
			"style-src 'self' 'unsafe-inline'",
		}, "; "))
		next.ServeHTTP(w, r)
	})
}

// csrfProtect rejects browser mutation requests originating outside the
// configured dashboard origin. SameSite cookies alone are insufficient when a
// user-controlled app and the dashboard share a registrable parent domain.
// Requests without browser origin metadata remain valid only when they carry no
// session cookie, preserving CLI sign-in while refusing ambiguous authenticated
// mutations.
func (s *Server) csrfProtect(next http.Handler) http.Handler {
	want := originOf(s.baseURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) || want == "" {
			next.ServeHTTP(w, r)
			return
		}

		got := strings.TrimSpace(r.Header.Get("Origin"))
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("Referer"))
		}
		_, sessionErr := r.Cookie(SessionCookie)
		if (got == "" && sessionErr == nil) || (got != "" && originOf(got) != want) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}
