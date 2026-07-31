package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/notify"
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

// ------------------------------------------------------------- sign-in

// fakeAccounts stands in for the account service, so the sign-in surface can be
// tested without a database — the same reason Apps is an interface.
//
// Any address beginning "known" is registered. issueDelay stands for the extra
// INSERT that issuing a link for a registered address costs and an unregistered
// one does not: it is the difference the timing floor has to hide.
type fakeAccounts struct {
	mu         sync.Mutex
	issueDelay time.Duration
	err        error
	asked      []string
}

func (f *fakeAccounts) IssueMagicLink(
	_ context.Context, email string, _ time.Duration,
) (string, account.User, bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, email)
	err := f.err
	delay := f.issueDelay
	f.mu.Unlock()

	if err != nil {
		return "", account.User{}, false, err
	}
	if !strings.HasPrefix(email, "known") {
		return "", account.User{}, false, nil
	}
	time.Sleep(delay)
	return "raw-token-for-" + email, account.User{Email: email}, true, nil
}

func (f *fakeAccounts) asking() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

type fakeMailer struct {
	mu    sync.Mutex
	delay time.Duration
	err   error
	sent  []notify.Message
}

func (m *fakeMailer) Send(_ context.Context, msg notify.Message) error {
	m.mu.Lock()
	delay, err := m.delay, m.err
	m.sent = append(m.sent, msg)
	m.mu.Unlock()

	time.Sleep(delay)
	return err
}

func (m *fakeMailer) messages() []notify.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sent)
}

func signInServer(t *testing.T, accounts Accounts, mailer notify.Mailer) http.Handler {
	t.Helper()
	return testServer(t, Options{
		Accounts: accounts,
		Mailer:   mailer,
		BaseURL:  "https://yacht.test",
		// Nobody reaching the sign-in page has a session, so the provider that
		// would resolve one refuses everything.
		Identity: identity.ProviderFunc(func(context.Context, *http.Request) (identity.Owner, error) {
			return identity.Owner{}, identity.ErrUnauthenticated
		}),
	})
}

func postSignIn(h http.Handler, email, remoteAddr string) *httptest.ResponseRecorder {
	body := url.Values{"email": {email}}.Encode()
	r := httptest.NewRequest(http.MethodPost, "/sign-in", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if remoteAddr != "" {
		r.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// The whole point of the page: someone with no session must be able to reach
// the one form that can give them one.
func TestSignInPageRendersOutsideAuth(t *testing.T) {
	h := signInServer(t, &fakeAccounts{}, &fakeMailer{})

	rec := get(t, h, "/sign-in")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sign-in = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`method="post"`, `action="/sign-in"`, `name="email"`} {
		if !strings.Contains(body, want) {
			t.Errorf("sign-in page missing %q", want)
		}
	}
}

// A sign-in form that reveals which addresses are registered is a user-list
// disclosure. Status, body and headers must be byte-identical.
func TestSignInDoesNotRevealWhetherAnAddressExists(t *testing.T) {
	accounts := &fakeAccounts{}
	mailer := &fakeMailer{}
	h := signInServer(t, accounts, mailer)

	registered := postSignIn(h, "known@example.test", "198.51.100.7:3000")
	stranger := postSignIn(h, "nobody@example.test", "198.51.100.8:3000")

	if registered.Code != stranger.Code {
		t.Fatalf("status: registered = %d, unregistered = %d — the difference is the disclosure",
			registered.Code, stranger.Code)
	}
	if registered.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", registered.Code)
	}
	if registered.Body.String() != stranger.Body.String() {
		t.Fatalf("bodies differ:\nregistered:   %q\nunregistered: %q",
			registered.Body.String(), stranger.Body.String())
	}
	if !maps.EqualFunc(registered.Result().Header, stranger.Result().Header, slices.Equal) {
		t.Fatalf("headers differ:\nregistered:   %v\nunregistered: %v",
			registered.Result().Header, stranger.Result().Header)
	}

	// ...and the difference that does exist is invisible from outside: a link
	// goes to the registered address and nothing at all to the other.
	sent := mailer.messages()
	if len(sent) != 1 || sent[0].To != "known@example.test" {
		t.Fatalf("mail sent = %v, want exactly one to the registered address", sent)
	}
}

// The spec requires "same timing". A registered address does an extra INSERT,
// so without a floor the difference is measurable — B's adversarial review
// measured 3.5x.
func TestSignInResponseTimeDoesNotLeakExistence(t *testing.T) {
	const samples = 100

	// Together these are 240ms of work on the registered path and none on the
	// other: under the floor, so both are padded to it. The mail delay is the
	// larger half deliberately — a send left outside the floor would show up
	// here as most of a 200ms difference.
	accounts := &fakeAccounts{issueDelay: 40 * time.Millisecond}
	mailer := &fakeMailer{delay: 200 * time.Millisecond}
	h := signInServer(t, accounts, mailer)

	known := make([]time.Duration, samples)
	unknown := make([]time.Duration, samples)
	codes := make([]int, 2*samples)

	timed := func(email, ip string) (time.Duration, int) {
		start := time.Now()
		rec := postSignIn(h, email, ip)
		return time.Since(start), rec.Code
	}

	// Interleaved: each iteration times one of each, back to back, so a machine
	// that gets busy halfway through skews both sets alike. Iterations run
	// concurrently only because the floor makes each attempt cost 250ms, and
	// each carries its own address and client IP so the rate limiter — which is
	// blind to whether the address exists — stays out of the measurement.
	work := make(chan int)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				ip := fmt.Sprintf("10.1.%d.%d:5000", i/256, i%256)
				known[i], codes[2*i] = timed(fmt.Sprintf("known-%d@example.test", i), ip)
				unknown[i], codes[2*i+1] = timed(fmt.Sprintf("nobody-%d@example.test", i), ip)
			}
		}()
	}
	for i := range samples {
		work <- i
	}
	close(work)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("sample %d answered %d — the measurement is not of the sign-in path", i, code)
		}
	}

	hi, lo := median(known), median(unknown)
	if hi < lo {
		hi, lo = lo, hi
	}
	if ratio := float64(hi) / float64(lo); ratio > 1.5 {
		t.Fatalf("median registered = %v, unregistered = %v, ratio = %.2f — "+
			"the response time says whether the address exists",
			median(known), median(unknown), ratio)
	}
}

func median(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)/2]
}

// With no relay configured the link goes to the log, which is the only way back
// into an install whose mail broke after accounts were switched on. Sign-in
// must behave the same either way.
func TestSignInWithNoMailTransportStillSucceeds(t *testing.T) {
	var logged bytes.Buffer
	h := testServer(t, Options{
		Accounts: &fakeAccounts{},
		BaseURL:  "https://yacht.test",
		Logger:   slog.New(slog.NewTextHandler(&logged, nil)),
	})

	rec := postSignIn(h, "known@example.test", "203.0.113.5:4000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Check your mail") {
		t.Fatalf("body = %q, want the check-your-mail page", rec.Body.String())
	}
	if !strings.Contains(logged.String(), "https://yacht.test/auth/raw-token-for-known@example.test") {
		t.Fatalf("the sign-in link never reached the log, so there is no way in:\n%s",
			logged.String())
	}
}

// Delivery failure must not become an oracle of its own, so it changes nothing
// the visitor can see — and is logged, because otherwise nobody ever finds out
// mail is broken.
func TestSignInSurvivesAMailFailure(t *testing.T) {
	var logged bytes.Buffer
	h := testServer(t, Options{
		Accounts: &fakeAccounts{},
		Mailer:   &fakeMailer{err: errors.New("relay refused")},
		BaseURL:  "https://yacht.test",
		Logger:   slog.New(slog.NewTextHandler(&logged, nil)),
	})

	failed := postSignIn(h, "known@example.test", "203.0.113.9:4000")
	fine := postSignIn(h, "nobody@example.test", "203.0.113.10:4000")

	if failed.Code != fine.Code || failed.Body.String() != fine.Body.String() {
		t.Fatalf("a failed send is visible in the response: %d %q vs %d %q",
			failed.Code, failed.Body.String(), fine.Code, fine.Body.String())
	}
	if !strings.Contains(logged.String(), "relay refused") {
		t.Errorf("a send failure must reach the log:\n%s", logged.String())
	}
}

func TestSignInRejectsAMalformedAddress(t *testing.T) {
	accounts := &fakeAccounts{}
	mailer := &fakeMailer{}
	h := signInServer(t, accounts, mailer)

	rec := postSignIn(h, "not an address", "203.0.113.11:4000")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	// The status line is written after the content type, or the browser is
	// handed the form as plain text.
	if ct := rec.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/sign-in"`) {
		t.Error("a rejected address must come back to the form, not a dead end")
	}
	if strings.Contains(body, "Check your mail") {
		t.Error("a malformed address must not claim a link was sent")
	}
	if got := accounts.asking(); len(got) != 0 {
		t.Errorf("looked up %v — a malformed address must not reach the account service", got)
	}
	if got := mailer.messages(); len(got) != 0 {
		t.Errorf("sent %v, want nothing", got)
	}
}

// The floor makes each attempt cost a fixed 250ms, so the rate limit is what
// stops an enumeration run from simply being made in parallel.
func TestSignInIsRateLimited(t *testing.T) {
	h := signInServer(t, &fakeAccounts{}, &fakeMailer{})

	const same = "known@example.test"
	for i := range signInAttemptsPerAddress {
		if code := postSignIn(h, same, "192.0.2.20:5000").Code; code != http.StatusOK {
			t.Fatalf("attempt %d = %d, want 200", i+1, code)
		}
	}
	// Another client IP, so this is the address limit and not the IP one.
	if code := postSignIn(h, same, "192.0.2.21:5000").Code; code != http.StatusTooManyRequests {
		t.Errorf("attempt %d = %d, want 429", signInAttemptsPerAddress+1, code)
	}

	// The limit is per address and per client, not a global stop: everyone else
	// can still sign in.
	if code := postSignIn(h, "known-other@example.test", "192.0.2.22:5000").Code; code != http.StatusOK {
		t.Errorf("an unrelated address = %d, want 200 — one attacker must not lock out the install", code)
	}

	// ...and one client cannot spend the whole install's budget either. Run in
	// parallel, which is both what an enumerator would do and the only way this
	// does not cost a second per handful of attempts.
	flooder := "192.0.2.30:5000"
	codes := make([]int, signInAttemptsPerIP+1)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = postSignIn(h, fmt.Sprintf("known-flood-%d@example.test", i), flooder).Code
		}()
	}
	wg.Wait()

	if !slices.Contains(codes, http.StatusTooManyRequests) {
		t.Errorf("one client made %d attempts on %d addresses unchecked: %v",
			len(codes), len(codes), codes)
	}
}

// Magic links are built from the configured base URL and never from the Host
// header: a link built from a header is a link an attacker can point at their
// own server and have Yacht mail to a real person.
func TestAccountsRequireAConfiguredBaseURL(t *testing.T) {
	_, err := New(Options{
		Orchestrator: newFailingOrchestrator(),
		Identity:     identity.NewSingleOwner(identity.Owner{ID: "x"}),
		Accounts:     &fakeAccounts{},
	})
	if err == nil {
		t.Fatal("expected accounts without a BaseURL to be refused")
	}
}
