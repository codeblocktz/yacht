package web

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func postPassword(h http.Handler, email, password, remoteAddr string) *httptest.ResponseRecorder {
	form := url.Values{"email": {email}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/sign-in/password",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://yacht.test")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The single most important test here.
//
// An address nobody holds, an address with no password, and an address whose
// password is wrong must produce the same status, the same bytes and the same
// headers. Any difference at all is a user-list disclosure: somebody can walk a
// list of addresses and learn which have accounts and which of those have chosen
// a password, without ever signing in.
func TestPasswordSignInDoesNotRevealWhetherAnAddressExists(t *testing.T) {
	h := signInServer(t, &fakeAccounts{}, &fakeMailer{})

	cases := []struct {
		name, email string
	}{
		{"an address nobody holds", "nobody@example.com"},
		{"an address with no password", "known@example.com"},
		{"the wrong password", "known-pw@example.com"},
	}

	// The form puts the submitted address back so nobody has to retype it, so
	// the bodies differ by exactly that string. It is the submitter's own input
	// and tells them nothing they did not just type, so it is replaced with a
	// fixed token rather than compared. Every other byte still has to match —
	// which is what would catch a stray "no password set" or a different
	// autofocus.
	normalise := func(body, email string) string {
		return strings.ReplaceAll(body, email, "<<ADDRESS>>")
	}

	var first, firstName string
	var firstHeader http.Header
	for _, tc := range cases {
		rec := postPassword(h, tc.email, "guess", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tc.name, rec.Code)
		}
		got := normalise(rec.Body.String(), tc.email)
		if first == "" {
			first, firstName, firstHeader = got, tc.name, rec.Header()
			continue
		}
		if got != first {
			t.Errorf("%s answers differently from %q — the body distinguishes them",
				tc.name, firstName)
		}
		if !maps.EqualFunc(rec.Header(), firstHeader, slices.Equal) {
			t.Errorf("%s answers differently from %q — the headers distinguish them",
				tc.name, firstName)
		}
	}
}

// The same claim, in time rather than in bytes.
//
// The service does a dummy verify for an address it does not know so the two
// paths cost the same, and the floor covers what is left. Both are the kind of
// thing that gets removed later as obviously redundant, so the measurement is
// here to notice.
func TestPasswordSignInResponseTimeDoesNotLeakExistence(t *testing.T) {
	// Stands in for the Argon2id cost, which the fake does not pay. If the floor
	// were removed, a known address would take this much longer than an unknown
	// one — which is exactly what is being ruled out.
	h := signInServer(t, &fakeAccounts{verifyDelay: 120 * time.Millisecond}, &fakeMailer{})

	// Interleaved, and a fresh address and client each iteration so that neither
	// rate limiter refuses part way through and turns a 401 into a 429.
	var unknown, known []time.Duration
	for i := range 4 {
		addr := "10.0.0." + string(rune('a'+i)) + ":1234"

		start := time.Now()
		rec := postPassword(h, "nobody-"+string(rune('a'+i))+"@example.com", "guess", addr)
		unknown = append(unknown, time.Since(start))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unknown address answered %d, so this is not measuring the path it means to", rec.Code)
		}

		start = time.Now()
		rec = postPassword(h, "known-pw-"+string(rune('a'+i))+"@example.com", "guess", addr)
		known = append(known, time.Since(start))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("known address answered %d", rec.Code)
		}
	}

	ratio := float64(median(known)) / float64(median(unknown))
	if ratio > 1.5 || ratio < 0.66 {
		t.Errorf("a known address took %v against %v for an unknown one (ratio %.2f) — "+
			"a stopwatch can tell them apart", median(known), median(unknown), ratio)
	}
}

// Guessing at somebody's password must not spend their magic-link budget.
//
// Shared counters would let an attacker lock a victim out of the method they
// actually use by hammering the one they do not — a denial of service smuggled
// in through a rate limiter. This is the test that pins the separate key
// prefixes.
func TestPasswordAttemptsDoNotSpendTheMagicLinkBudget(t *testing.T) {
	h := signInServer(t, &fakeAccounts{}, &fakeMailer{})
	const victim = "known-victim@example.com"

	// Well past the per-address password limit.
	for range passwordAttemptsPerAddress + 2 {
		postPassword(h, victim, "guess", "203.0.113.9:1000")
	}

	if got := postSignIn(h, victim, "203.0.113.9:1000").Code; got != http.StatusOK {
		t.Errorf("asking for a sign-in link after failed password guesses = %d, want 200 — "+
			"guessing at a password locked somebody out of the link they rely on", got)
	}
}

// Parallel guessing is what the limits are for.
func TestPasswordSignInIsRateLimited(t *testing.T) {
	h := signInServer(t, &fakeAccounts{}, &fakeMailer{})

	// The address limit holds even as the client changes, because an address is
	// the thing an attacker cannot avoid naming.
	const target = "known-pw-limited@example.com"
	var limited bool
	for i := range passwordAttemptsPerAddress + 1 {
		rec := postPassword(h, target, "guess", "198.51.100."+string(rune('1'+i))+":9000")
		if rec.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("an address took more than its allowance of guesses from changing clients")
	}

	// And one attacker must not be able to lock the whole install out.
	if got := postPassword(h, "known-pw-other@example.com", fakePassword, "192.0.2.7:1").Code; got == http.StatusTooManyRequests {
		t.Error("an unrelated address was refused because somebody else was being guessed at")
	}
}

// Both ways in are offered to everybody, with no knowledge of any address.
//
// Anything that decided what to render after learning whether an address has a
// password would be an enumeration oracle. This asserts that no such decision
// exists to be made: the page is the same page before anybody types anything.
func TestSignInPageOffersBothMethodsToEveryone(t *testing.T) {
	h := signInServer(t, &fakeAccounts{}, &fakeMailer{})

	body := get(t, h, "/sign-in").Body.String()
	for _, want := range []string{
		`action="/sign-in/password"`,
		`action="/sign-in"`,
		`name="password"`,
		`autocomplete="current-password"`,
		`autocomplete="username"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the sign-in page is missing %s", want)
		}
	}
}

// A submitted password must not come back in the HTML.
//
// no-store does not reach a screenshot, a printout, or a page pasted into a bug
// report. The data struct has no field for it, and this is what keeps it that
// way.
func TestSignInFormDoesNotEchoTheSubmittedPassword(t *testing.T) {
	h := signInServer(t, &fakeAccounts{}, &fakeMailer{})
	const distinctive = "hunter2-swordfish-correct-battery"

	body := postPassword(h, "known-pw@example.com", distinctive, "").Body.String()
	if strings.Contains(body, distinctive) {
		t.Error("the submitted password was rendered back into the page")
	}
}

// The regression that matters most.
//
// csrfProtect exempts origin-less posts on /sign-in by name so a CLI can ask for
// a link. A password travelling under that exemption would be login CSRF: an
// origin-less cross-site post signs the victim's browser into the attacker's
// account, and everything they type next lands somewhere it can be read.
//
// If somebody later adds this path to originlessSignIn, this fails.
func TestCSRFRefusesAPasswordSignInWithoutOriginMetadata(t *testing.T) {
	fake := &fakeAccounts{}
	h := signInServer(t, fake, &fakeMailer{})

	form := url.Values{"email": {"known-pw@example.com"}, "password": {fakePassword}}
	req := httptest.NewRequest(http.MethodPost, "/sign-in/password",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No Origin, no Referer, no session cookie — the exact shape the /sign-in
	// carve-out was written for.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("an origin-less password post answered %d, want 403", rec.Code)
	}
	if len(fake.asking()) != 0 {
		t.Error("the handler ran despite the request being cross-origin — " +
			"the password reached the service before the check")
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), SessionCookie) {
		t.Error("an origin-less password post set a session cookie")
	}
}
