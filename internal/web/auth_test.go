package web

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/account"
)

func TestSessionCookieFlags(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://yacht.test/", nil)

	setSessionCookie(w, r, "token-value", time.Hour)

	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cs))
	}
	c := cs[0]

	if c.Name != SessionCookie || c.Value != "token-value" {
		t.Fatalf("cookie = %s=%s", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Error("HttpOnly must be set — script-readable session cookies are stealable by XSS")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("SameSite=Lax is the CSRF defence; without it every form needs a token")
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	// Secure on a plain-HTTP request means the browser silently drops the
	// cookie and sign-in appears to do nothing, with nothing to explain why.
	if c.Secure {
		t.Error("Secure must not be set on a plain-HTTP request")
	}
}

func TestSessionCookieIsSecureOverTLS(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "https://yacht.test/", nil)
	r.TLS = &tls.ConnectionState{}

	setSessionCookie(w, r, "token-value", time.Hour)

	if !w.Result().Cookies()[0].Secure {
		t.Error("Secure must be set when the request arrived over TLS")
	}
}

// A reverse proxy terminates TLS and forwards plain HTTP. Without honouring
// the header, every proxied install loses the Secure flag.
func TestSessionCookieHonoursForwardedProto(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://yacht.test/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")

	setSessionCookie(w, r, "token-value", time.Hour)

	if !w.Result().Cookies()[0].Secure {
		t.Error("Secure must be set when X-Forwarded-Proto says https")
	}
}

func TestClearSessionCookieExpiresIt(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://yacht.test/", nil)

	clearSessionCookie(w, r)

	c := w.Result().Cookies()[0]
	if c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("cleared cookie = %q MaxAge=%d, want empty with negative MaxAge", c.Value, c.MaxAge)
	}
}

// The cleared cookie only replaces the live one if it carries the same
// attributes; a mismatched Path leaves the original in place and the person
// stays signed in after clicking sign out.
func TestClearSessionCookieKeepsTheOtherFlags(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://yacht.test/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")

	clearSessionCookie(w, r)

	c := w.Result().Cookies()[0]
	if c.Name != SessionCookie {
		t.Errorf("name = %q, want %q", c.Name, SessionCookie)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || !c.Secure {
		t.Errorf("cleared cookie flags = HttpOnly:%v SameSite:%v Secure:%v, want all set",
			c.HttpOnly, c.SameSite, c.Secure)
	}
}

// The session provider reads the cookie by name. If the two names ever drift
// apart, sign-in writes a cookie that resolution ignores.
func TestSessionCookieNameMatchesTheProvider(t *testing.T) {
	if SessionCookie != account.DefaultCookieName {
		t.Fatalf("web.SessionCookie = %q, account.DefaultCookieName = %q — a session written under one name cannot be resolved under the other",
			SessionCookie, account.DefaultCookieName)
	}
}
