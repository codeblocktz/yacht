package web

import (
	"context"
	"net/http"
	"net/url"
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

// The whole feature, end to end, the way a person meets it.
//
// Sign in by link, add a password from the account page, sign out, sign in with
// the password. Every step through the real handlers and a real database,
// because what is being proved is that the two ways in produce the same session
// — and no fake can say that.
func TestAPasswordAddedFromTheAccountPageSignsYouIn(t *testing.T) {
	h := newLiveHarness(t, "web-pw-flow")
	const email = "flow@web.test"
	const password = "a perfectly good passphrase"
	h.user(t, email)

	c := sessionCookie(h.signIn(t, email))

	// Before: the page offers to add one.
	body := h.getAs(t, "/account", c).Body.String()
	if !strings.Contains(body, "Add a password") {
		t.Fatal("the account page does not offer to add a password")
	}

	res := h.postFormAs(t, "/account/password", c, url.Values{
		"password": {password}, "confirm": {password},
	})
	if res.Code != http.StatusSeeOther {
		t.Fatalf("POST /account/password = %d, want 303\n%s", res.Code, res.Body.String())
	}

	// After: the page now offers to change it, and says both ways in work.
	body = h.getAs(t, "/account", c).Body.String()
	if !strings.Contains(body, "Change your password") {
		t.Error("after setting one, the page still offers to add a password")
	}
	if strings.Contains(body, password) {
		t.Error("the account page renders the password back into the HTML")
	}

	// And the password is a way in, producing a session that actually works.
	signedIn := postPassword(h.handler, email, password, "198.51.100.5:1000")
	if signedIn.Code != http.StatusSeeOther {
		t.Fatalf("signing in with the new password = %d, want 303", signedIn.Code)
	}
	pwCookie := sessionCookie(signedIn)
	if pwCookie == nil {
		t.Fatal("signing in with a password set no session cookie")
	}
	if got := h.getAs(t, "/apps", pwCookie).Code; got != http.StatusOK {
		t.Errorf("GET /apps with a password session = %d, want 200 — "+
			"the cookie was set but resolves to nothing", got)
	}

	// The wrong one is still refused.
	if got := postPassword(h.handler, email, "not the password", "198.51.100.6:1").Code; got != http.StatusUnauthorized {
		t.Errorf("a wrong password answered %d, want 401", got)
	}
}

// An account with no password must refuse every password, including the empty
// one.
//
// The failure this prevents is a zero value comparing equal to something: a
// person who never chose a password being signed in by anybody who leaves the
// field blank.
func TestPasswordSignInIsRefusedWhenNoPasswordIsSet(t *testing.T) {
	h := newLiveHarness(t, "web-pw-none")
	const email = "nopassword@web.test"
	h.user(t, email)

	for _, guess := range []string{"", "password", "a perfectly good passphrase"} {
		rec := postPassword(h.handler, email, guess, "198.51.100.7:1")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("password %q against an account with none = %d, want 401",
				guess, rec.Code)
		}
		if sessionCookie(rec) != nil {
			t.Errorf("password %q opened a session on an account with none", guess)
		}
	}
}

// A session that has not proved itself recently cannot add a password.
//
// This is the left-open-laptop escalation, at the web layer: a cookie somebody
// walked away from turning itself into a permanent credential. The status alone
// would pass against a handler that wrote first and complained after, so the
// database is checked as well.
func TestAddingAPasswordNeedsARecentSignIn(t *testing.T) {
	h := newLiveHarness(t, "web-pw-stale")
	const email = "stale@web.test"
	u := h.user(t, email)
	c := sessionCookie(h.signIn(t, email))

	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET authenticated_at = now() - interval '1 hour'
		 WHERE user_id = $1`, u.ID); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	const password = "a perfectly good passphrase"
	res := h.postFormAs(t, "/account/password", c, url.Values{
		"password": {password}, "confirm": {password},
	})
	if res.Code != http.StatusUnprocessableEntity {
		t.Errorf("adding a password from an old session = %d, want 422", res.Code)
	}

	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_credentials WHERE user_id = $1`, u.ID).Scan(&n); err != nil {
		t.Fatalf("count credentials: %v", err)
	}
	if n != 0 {
		t.Error("a password was written despite the refusal")
	}

	// The page says what to do about it rather than only refusing.
	if !strings.Contains(h.getAs(t, "/account", c).Body.String(), "Sign in again") {
		t.Error("the account page does not tell a stale session how to proceed")
	}
}

// Two boxes that do not match must not write either of them.
func TestAMistypedConfirmationDoesNotSetAPassword(t *testing.T) {
	h := newLiveHarness(t, "web-pw-typo")
	const email = "typo@web.test"
	u := h.user(t, email)
	c := sessionCookie(h.signIn(t, email))

	res := h.postFormAs(t, "/account/password", c, url.Values{
		"password": {"a perfectly good passphrase"},
		"confirm":  {"a perfectly good passphrasee"},
	})
	if res.Code != http.StatusUnprocessableEntity {
		t.Errorf("mismatched confirmation = %d, want 422", res.Code)
	}

	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_credentials WHERE user_id = $1`, u.ID).Scan(&n); err != nil {
		t.Fatalf("count credentials: %v", err)
	}
	if n != 0 {
		t.Error("a password was written despite the two boxes disagreeing")
	}
}

// Adding a first password must not sign the person out of their other browsers.
//
// Gaining a way in is not a change of trust, and revoking here reads as the
// feature having broken the sessions somebody already had.
func TestAddingAFirstPasswordKeepsOtherSessions(t *testing.T) {
	h := newLiveHarness(t, "web-pw-keep")
	const email = "keep@web.test"
	h.user(t, email)

	other := sessionCookie(h.signIn(t, email))
	c := sessionCookie(h.signIn(t, email))

	const password = "a perfectly good passphrase"
	if code := h.postFormAs(t, "/account/password", c, url.Values{
		"password": {password}, "confirm": {password},
	}).Code; code != http.StatusSeeOther {
		t.Fatalf("POST /account/password = %d, want 303", code)
	}

	if got := h.getAs(t, "/apps", other).Code; got != http.StatusOK {
		t.Errorf("another browser answered %d after a first password was added, want 200", got)
	}
}

// The policy is the service's, and the page says what it refused.
func TestTheAccountPageExplainsARefusedPassword(t *testing.T) {
	h := newLiveHarness(t, "web-pw-policy")
	const email = "policy@web.test"
	h.user(t, email)
	c := sessionCookie(h.signIn(t, email))

	res := h.postFormAs(t, "/account/password", c, url.Values{
		"password": {"short"}, "confirm": {"short"},
	})
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a short password = %d, want 422", res.Code)
	}
	if !strings.Contains(res.Body.String(), "too short") {
		t.Error("the page does not say the password was too short")
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
