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

// Changing a password ends the person's other browsers and keeps this one.
//
// Both halves matter, and the second is the one that gets broken. Revoking
// nothing leaves a stolen session running after the person has done the one
// thing they know to do about it; revoking everything signs them out of the
// browser they are looking at, which reads as the change having failed rather
// than having worked.
func TestChangingAPasswordSignsOutOtherBrowsersButNotThisOne(t *testing.T) {
	h := newLiveHarness(t, "web-pw-revoke")
	const email = "revoke@web.test"
	h.user(t, email)

	first := sessionCookie(h.signIn(t, email))
	if code := h.postFormAs(t, "/account/password", first, url.Values{
		"password": {"the first passphrase"}, "confirm": {"the first passphrase"},
	}).Code; code != http.StatusSeeOther {
		t.Fatalf("setting the first password = %d", code)
	}

	other := sessionCookie(h.signIn(t, email))

	if code := h.postFormAs(t, "/account/password", first, url.Values{
		"current_password": {"the first passphrase"},
		"password":         {"the second passphrase"},
		"confirm":          {"the second passphrase"},
	}).Code; code != http.StatusSeeOther {
		t.Fatalf("changing the password = %d, want 303", code)
	}

	if got := h.getAs(t, "/apps", other).Code; got == http.StatusOK {
		t.Error("another browser still works after the password was changed")
	}
	if got := h.getAs(t, "/apps", first).Code; got != http.StatusOK {
		t.Errorf("the browser that made the change answered %d, want 200 — "+
			"it signed the person out of the page they were standing on", got)
	}
}

// Removing a password puts the account back to links, ends other sessions, and
// leaves the person able to get in.
//
// The last part is why removal is allowed at all: it is only safe because the
// emailed link never stopped working. If it did, this would be a way to lock
// yourself out with one click.
func TestRemovingAPasswordGoesBackToLinksAndStillLetsYouIn(t *testing.T) {
	h := newLiveHarness(t, "web-pw-remove")
	const email = "remove@web.test"
	const password = "a perfectly good passphrase"
	h.user(t, email)

	c := sessionCookie(h.signIn(t, email))
	if code := h.postFormAs(t, "/account/password", c, url.Values{
		"password": {password}, "confirm": {password},
	}).Code; code != http.StatusSeeOther {
		t.Fatalf("setting a password = %d", code)
	}
	other := sessionCookie(h.signIn(t, email))

	// This session is fresh, so no current password is asked for.
	if code := h.postFormAs(t, "/account/password/remove", c, url.Values{}).Code; code != http.StatusSeeOther {
		t.Fatalf("removing the password = %d, want 303", code)
	}

	if got := h.getAs(t, "/apps", other).Code; got == http.StatusOK {
		t.Error("another browser still works after the password was removed")
	}
	if got := postPassword(h.handler, email, password, "198.51.100.8:1").Code; got != http.StatusUnauthorized {
		t.Errorf("the removed password still signs in: %d", got)
	}

	// And the link still works, end to end through the mail.
	linkCookie := sessionCookie(h.signIn(t, email))
	if got := h.getAs(t, "/apps", linkCookie).Code; got != http.StatusOK {
		t.Errorf("the emailed link stopped working after a password was removed: %d", got)
	}

	body := h.getAs(t, "/account", linkCookie).Body.String()
	if !strings.Contains(body, "Add a password") {
		t.Error("after removal the page does not offer to add one again")
	}
	if strings.Contains(body, `action="/account/password/remove"`) {
		t.Error("the page still offers to remove a password that is gone")
	}
}

// An old session must produce the current password to remove one.
func TestRemovingAPasswordFromAnOldSessionNeedsTheCurrentOne(t *testing.T) {
	h := newLiveHarness(t, "web-pw-rm-stale")
	const email = "rmstale@web.test"
	const password = "a perfectly good passphrase"
	u := h.user(t, email)

	c := sessionCookie(h.signIn(t, email))
	if code := h.postFormAs(t, "/account/password", c, url.Values{
		"password": {password}, "confirm": {password},
	}).Code; code != http.StatusSeeOther {
		t.Fatalf("setting a password = %d", code)
	}
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET authenticated_at = now() - interval '1 hour' WHERE user_id = $1`,
		u.ID); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	if code := h.postFormAs(t, "/account/password/remove", c, url.Values{
		"current_password": {"not the password"},
	}).Code; code != http.StatusUnprocessableEntity {
		t.Errorf("removing with a wrong current password = %d, want 422", code)
	}
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_credentials WHERE user_id = $1`, u.ID).Scan(&n); err != nil {
		t.Fatalf("count credentials: %v", err)
	}
	if n != 1 {
		t.Fatal("the password was removed despite the refusal")
	}

	if code := h.postFormAs(t, "/account/password/remove", c, url.Values{
		"current_password": {password},
	}).Code; code != http.StatusSeeOther {
		t.Errorf("removing with the correct current password = %d, want 303", code)
	}
}

// Forgetting a password is recovered by asking for a link, and that is the
// whole recovery flow.
//
// This is the test that stands in for a password_resets table, a
// /forgot-password route, a second mail body and a TTL knob. A magic link
// already grants everything a reset token would — same mailbox, same relay,
// strictly more access — so the session it opens is proof enough to set a new
// password, and the current-password box may be left empty.
//
// If this ever stops passing, the argument for having no reset token stops
// holding, and the missing subsystem has to come back.
func TestAForgottenPasswordIsReplacedByAskingForALink(t *testing.T) {
	h := newLiveHarness(t, "web-pw-forgot")
	const email = "forgot@web.test"
	h.user(t, email)

	// Somebody sets a password, then forgets it.
	setup := sessionCookie(h.signIn(t, email))
	if code := h.postFormAs(t, "/account/password", setup, url.Values{
		"password": {"the forgotten passphrase"},
		"confirm":  {"the forgotten passphrase"},
	}).Code; code != http.StatusSeeOther {
		t.Fatalf("setting the first password = %d", code)
	}

	// The sign-in page tells them what to do about it, without a route of its
	// own and without ever asking whether the address has a password.
	if !strings.Contains(get(t, h.handler, "/sign-in").Body.String(), "forgotten it") {
		t.Error("the sign-in page does not say how to get back in without the password")
	}

	// They ask for a link and follow it. The session that opens is fresh.
	c := sessionCookie(h.signIn(t, email))

	// The account page says the current password may be left blank.
	if !strings.Contains(h.getAs(t, "/account", c).Body.String(), "forgotten it") {
		t.Error("the account page does not say the current password can be left blank")
	}

	// And it can be.
	if code := h.postFormAs(t, "/account/password", c, url.Values{
		"current_password": {""},
		"password":         {"the replacement passphrase"},
		"confirm":          {"the replacement passphrase"},
	}).Code; code != http.StatusSeeOther {
		t.Fatalf("replacing a forgotten password from a fresh session = %d, want 303", code)
	}

	if got := postPassword(h.handler, email, "the replacement passphrase", "198.51.100.20:1").Code; got != http.StatusSeeOther {
		t.Errorf("the replacement password does not sign in: %d", got)
	}
	if got := postPassword(h.handler, email, "the forgotten passphrase", "198.51.100.21:1").Code; got != http.StatusUnauthorized {
		t.Errorf("the forgotten password still signs in: %d", got)
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
