package web

import (
	"net/http"
	"strings"
	"time"
)

// SessionCookie is the name of the cookie carrying a session token.
//
// It matches account.DefaultCookieName because the session provider resolves
// requests by reading a cookie of that name: a mismatch would write a cookie
// nothing reads.
const SessionCookie = "yacht_session"

// requestIsTLS reports whether the request reached us over TLS.
//
// X-Forwarded-Proto is honoured because the common deployment terminates TLS
// at a reverse proxy and forwards plain HTTP. Reading only r.TLS would drop
// the Secure flag on exactly the installs that have a certificate.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setSessionCookie writes the session cookie.
//
// Secure is conditional on purpose. Setting it unconditionally makes the
// browser drop the cookie on a plain-HTTP install, so sign-in appears to
// succeed and then does nothing, with nothing on screen explaining why.
//
// SameSite=Lax is the CSRF defence, which is why no form in this codebase
// carries a token: a cross-site POST does not get the cookie attached.
func setSessionCookie(w http.ResponseWriter, r *http.Request, raw string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

// clearSessionCookie expires the session cookie.
//
// The attributes repeat those of setSessionCookie because a browser only
// replaces a cookie whose name, path and domain match: clearing with a
// different Path would leave the live cookie in place and sign-out would do
// nothing.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: requestIsTLS(r), MaxAge: -1,
	})
}
