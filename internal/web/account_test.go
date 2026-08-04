package web

import (
	"net/http"
	"strings"
	"testing"
)

// The account page shows the viewer their own account, and reaching it needs a
// session.
//
// The second half is the one worth pinning. Everything the page renders is read
// from the resolved cookie, so a route that answered without one would be
// rendering somebody — and the only question would be who.
func TestAccountPageNeedsASessionAndShowsTheViewersOwn(t *testing.T) {
	h := newLiveHarness(t, "web-account")
	h.user(t, "account@web.test")

	if got := h.getAs(t, "/account", nil).Code; got != http.StatusSeeOther {
		t.Errorf("GET /account without a cookie = %d, want %d (a redirect to sign in)",
			got, http.StatusSeeOther)
	}

	c := sessionCookie(h.signIn(t, "account@web.test"))
	res := h.getAs(t, "/account", c)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /account = %d, want 200", res.Code)
	}

	body := res.Body.String()
	if !strings.Contains(body, "account@web.test") {
		t.Error("the account page does not show the viewer their own address")
	}
	if !strings.Contains(body, `action="/sign-out-everywhere"`) {
		t.Error("the account page does not offer to sign out everywhere, " +
			"which is the control somebody looks for on the day a cookie is stolen")
	}
}

// A page carrying somebody's address must not be cached.
//
// securityHeaders sets no-store for everything, and the only handler that
// overrides it is the asset one. This pins that for a page where getting it
// wrong leaves an account behind on a shared machine.
func TestAccountPageStaysOutOfCaches(t *testing.T) {
	h := newLiveHarness(t, "web-acct-cache")
	h.user(t, "cache@web.test")
	c := sessionCookie(h.signIn(t, "cache@web.test"))

	res := h.getAs(t, "/account", c)
	if got := res.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
