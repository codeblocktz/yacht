package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/notify"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store"
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

// The rest of the interface exists so this fake satisfies it, and refuses so
// that a test which reaches the callback through it fails loudly instead of
// quietly proving nothing. The callback is exercised against the real service,
// where consuming a link and claiming a team mean something.
func (f *fakeAccounts) ConsumeMagicLink(context.Context, string) (account.User, error) {
	return account.User{}, account.ErrTokenInvalid
}

func (f *fakeAccounts) TeamsFor(context.Context, uuid.UUID) ([]account.Membership, error) {
	return nil, errNoFakeAccountsBackend
}

func (f *fakeAccounts) BootstrapOwner(context.Context, string, string, account.User) error {
	return errNoFakeAccountsBackend
}

func (f *fakeAccounts) CreateSession(
	context.Context, uuid.UUID, string, string, string, time.Duration,
) (string, error) {
	return "", errNoFakeAccountsBackend
}

func (f *fakeAccounts) ResolveSession(context.Context, string) (account.Session, error) {
	return account.Session{}, account.ErrSessionInvalid
}

func (f *fakeAccounts) SwitchTeam(context.Context, uuid.UUID, string) error {
	return errNoFakeAccountsBackend
}

func (f *fakeAccounts) RevokeSession(context.Context, string) error {
	return errNoFakeAccountsBackend
}

func (f *fakeAccounts) RevokeAllSessions(context.Context, uuid.UUID) error {
	return errNoFakeAccountsBackend
}

// Team management is exercised against the real service and a real database in
// team_test.go — the claims there are facts about rows in three tables, and a
// fake asked the same questions would only repeat the answers it was written
// with. These exist so fakeAccounts still satisfies the interface for the
// sign-in tests, which have no business touching a team.

func (f *fakeAccounts) InvitationFor(context.Context, string) (account.Invitation, error) {
	return account.Invitation{}, account.ErrTokenInvalid
}

func (f *fakeAccounts) AcceptInvitation(
	context.Context, string, uuid.UUID,
) (string, account.Role, error) {
	return "", "", account.ErrTokenInvalid
}

func (f *fakeAccounts) ListMembers(context.Context, string) ([]account.Member, error) {
	return nil, errNoFakeAccountsBackend
}

func (f *fakeAccounts) ListPendingInvitations(
	context.Context, string,
) ([]account.Invitation, error) {
	return nil, errNoFakeAccountsBackend
}

func (f *fakeAccounts) Invite(
	context.Context, uuid.UUID, string, string, account.Role, time.Duration,
) (string, error) {
	return "", errNoFakeAccountsBackend
}

func (f *fakeAccounts) RevokeInvitation(
	context.Context, uuid.UUID, string, uuid.UUID,
) error {
	return errNoFakeAccountsBackend
}

func (f *fakeAccounts) SetRole(
	context.Context, uuid.UUID, string, uuid.UUID, account.Role,
) error {
	return errNoFakeAccountsBackend
}

func (f *fakeAccounts) RemoveMember(context.Context, uuid.UUID, string, uuid.UUID) error {
	return errNoFakeAccountsBackend
}

var errNoFakeAccountsBackend = errors.New("fakeAccounts holds no teams or sessions")

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
		Accounts:        accounts,
		Mailer:          mailer,
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: "web-signin",
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
		Accounts:        &fakeAccounts{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: "web-signin",
		Logger:          slog.New(slog.NewTextHandler(&logged, nil)),
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
		Accounts:        &fakeAccounts{},
		Mailer:          &fakeMailer{err: errors.New("relay refused")},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: "web-signin",
		Logger:          slog.New(slog.NewTextHandler(&logged, nil)),
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
		Orchestrator:    newFailingOrchestrator(),
		Identity:        identity.NewSingleOwner(identity.Owner{ID: "x"}),
		Accounts:        &fakeAccounts{},
		BootstrapTeamID: "owner-local",
	})
	if err == nil {
		t.Fatal("expected accounts without a BaseURL to be refused")
	}
}

// Without the configured team id the first person to sign in gets a fresh empty
// team, and the apps the install already deployed belong to an owner nobody can
// sign in as. Silently, which is why it is refused at construction.
func TestAccountsRequireABootstrapTeam(t *testing.T) {
	_, err := New(Options{
		Orchestrator: newFailingOrchestrator(),
		Identity:     identity.NewSingleOwner(identity.Owner{ID: "x"}),
		Accounts:     &fakeAccounts{},
		BaseURL:      "https://yacht.test",
	})
	if err == nil {
		t.Fatal("expected accounts without a bootstrap team to be refused")
	}
}

// ------------------------------------------------- the magic-link callback
//
// These run against the real account service, the real app service and a real
// database. What has to hold — that the first person to follow a link ends up
// looking at the apps the install already had — is a fact about three packages
// and a transaction, and no fake can prove it.

// liveHarness is the whole sign-in path assembled: mail, callback, session
// cookie, and the dashboard the cookie resolves against.
type liveHarness struct {
	handler  http.Handler
	accounts *account.Service
	apps     *app.Service
	mailer   *fakeMailer
	pool     *pgxpool.Pool
	teamID   string
}

// newLiveHarness wires the engine against the test database, with teamID
// standing in for YACHT_OWNER_ID.
//
// Addresses end in @web.test and team ids begin with web-, because `go test
// ./...` runs the account, app and store packages against this same database at
// the same time and each purges what it recognises.
func newLiveHarness(t *testing.T, teamID string) *liveHarness {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run the sign-in callback tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	purge := func() {
		// A person who could not have the configured team gets one named after
		// themselves, so it is found through the same address filter — and has
		// to go before the user it is derived from.
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id IN (
			SELECT 'user-' || u.id::text FROM users u
			WHERE lower(u.email) LIKE '%@web.test')`); err != nil {
			t.Errorf("purge personal teams: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id LIKE 'web-%'`); err != nil {
			t.Errorf("purge teams: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM users WHERE lower(email) LIKE '%@web.test'`); err != nil {
			t.Errorf("purge users: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	accounts := account.NewService(pool, log)
	apps := app.NewService(pool, orchestrator.NewNoop(), log, app.Options{})
	mailer := &fakeMailer{}

	h := testServer(t, Options{
		Apps:     apps,
		Accounts: accounts,
		// The provider under test end to end: the cookie the callback sets is
		// the cookie the dashboard resolves an owner from.
		Identity:          accounts.Provider(SessionCookie),
		Mailer:            mailer,
		BaseURL:           "https://yacht.test",
		BootstrapTeamID:   teamID,
		BootstrapTeamName: "Local",
		SessionTTL:        time.Hour,
	})

	return &liveHarness{
		handler: h, accounts: accounts, apps: apps,
		mailer: mailer, pool: pool, teamID: teamID,
	}
}

// installedApp puts the configured team and an app in it, which is the state an
// install that has been running on a single owner is in when accounts go on.
func (h *liveHarness) installedApp(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	if err := h.apps.EnsureOwner(ctx, h.teamID, "Local", ""); err != nil {
		t.Fatalf("EnsureOwner: %v", err)
	}
	if _, err := h.apps.Create(ctx, h.teamID, app.CreateInput{
		Name: name, Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("create the app the install already had: %v", err)
	}
}

func (h *liveHarness) user(t *testing.T, email string) account.User {
	t.Helper()
	u, err := h.accounts.EnsureUser(context.Background(), email, "")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	return u
}

// signIn walks the whole path: ask for a link, take it out of the mail, follow
// it. The token exists nowhere else, which is exactly the person's position.
func (h *liveHarness) signIn(t *testing.T, email string) *httptest.ResponseRecorder {
	t.Helper()
	before := len(h.mailer.messages())

	if code := postSignIn(h.handler, email, "198.51.100.1:2000").Code; code != http.StatusOK {
		t.Fatalf("POST /sign-in for %s = %d, want 200", email, code)
	}
	sent := h.mailer.messages()
	if len(sent) != before+1 {
		t.Fatalf("mails sent = %d, want one more than %d — no link to follow",
			len(sent), before)
	}
	return h.follow(t, linkIn(t, sent[len(sent)-1].TextBody))
}

func (h *liveHarness) follow(t *testing.T, link string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, link, nil))
	return rec
}

func (h *liveHarness) getAs(
	t *testing.T, path string, c *http.Cookie,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *liveHarness) postAs(
	t *testing.T, path string, c *http.Cookie,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *liveHarness) owners(t *testing.T, teamID string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM memberships WHERE owner_id = $1 AND role = 'owner'`,
		teamID).Scan(&n); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	return n
}

// linkIn pulls the sign-in URL out of the mail body.
func linkIn(t *testing.T, body string) string {
	t.Helper()
	const prefix = "https://yacht.test/auth/"
	i := strings.Index(body, prefix)
	if i < 0 {
		t.Fatalf("no sign-in link in the mail:\n%s", body)
	}
	link := body[i:]
	if j := strings.IndexAny(link, " \r\n"); j >= 0 {
		link = link[:j]
	}
	return link
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	return nil
}

func TestValidTokenCreatesASessionAndRedirects(t *testing.T) {
	h := newLiveHarness(t, "web-valid")
	h.installedApp(t, "already-here")
	h.user(t, "alice@web.test")

	rec := h.signIn(t, "alice@web.test")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("following the link = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}

	c := sessionCookie(rec)
	if c == nil || c.Value == "" {
		t.Fatal("no session cookie — the link proved who they are and gave them nothing")
	}
	// The session is real, not merely written: the dashboard resolves an owner
	// from this cookie and answers.
	if code := h.getAs(t, "/apps", c).Code; code != http.StatusOK {
		t.Fatalf("GET /apps with the new session = %d, want 200", code)
	}
}

// The cookie must carry the flags from the cookie helper, on the one response
// that ever sets it.
func TestCallbackSetsTheSessionCookie(t *testing.T) {
	h := newLiveHarness(t, "web-cookie")
	h.user(t, "cookie@web.test")

	c := sessionCookie(h.signIn(t, "cookie@web.test"))
	if c == nil {
		t.Fatal("the callback set no session cookie")
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
	// The mailed link is https, so this one arrived over TLS.
	if !c.Secure {
		t.Error("Secure must be set when the link was followed over TLS")
	}
	if c.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want the session TTL", c.MaxAge)
	}

	// ...and the same callback on a plain-HTTP install must not set it, or the
	// browser drops the cookie and signing in appears to do nothing at all.
	raw, _, existed, err := h.accounts.IssueMagicLink(
		context.Background(), "cookie@web.test", time.Minute)
	if err != nil || !existed {
		t.Fatalf("IssueMagicLink: %v (existed=%v)", err, existed)
	}
	plain := sessionCookie(h.follow(t, "http://yacht.test/auth/"+raw))
	if plain == nil {
		t.Fatal("the callback set no session cookie over plain HTTP")
	}
	if plain.Secure {
		t.Error("Secure must not be set on a plain-HTTP request")
	}
}

// A link sits in a mailbox forever. One that still worked the second time would
// be a permanent key to the account.
func TestConsumedTokenIsRejected(t *testing.T) {
	h := newLiveHarness(t, "web-consumed")
	h.user(t, "spent@web.test")

	if code := postSignIn(h.handler, "spent@web.test", "198.51.100.2:2000").Code; code != http.StatusOK {
		t.Fatalf("POST /sign-in = %d, want 200", code)
	}
	sent := h.mailer.messages()
	if len(sent) != 1 {
		t.Fatalf("mails = %d, want 1", len(sent))
	}
	link := linkIn(t, sent[0].TextBody)

	if code := h.follow(t, link).Code; code != http.StatusSeeOther {
		t.Fatalf("first use = %d, want 303", code)
	}

	again := h.follow(t, link)
	if again.Code == http.StatusSeeOther {
		t.Fatal("the link worked twice")
	}
	if again.Code != http.StatusUnauthorized {
		t.Errorf("second use = %d, want 401", again.Code)
	}
	if c := sessionCookie(again); c != nil && c.Value != "" {
		t.Fatal("a spent link still handed out a session")
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	h := newLiveHarness(t, "web-expired")
	h.user(t, "stale@web.test")

	// Issued directly, because the only way to hold an expired link is to have
	// been given it before it expired.
	raw, _, existed, err := h.accounts.IssueMagicLink(
		context.Background(), "stale@web.test", -time.Minute)
	if err != nil || !existed {
		t.Fatalf("IssueMagicLink: %v (existed=%v)", err, existed)
	}

	rec := h.follow(t, "https://yacht.test/auth/"+raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("following an expired link = %d, want 401", rec.Code)
	}
	if c := sessionCookie(rec); c != nil && c.Value != "" {
		t.Fatal("an expired link handed out a session")
	}
}

// The spec's third lockout guard, dropped between spec and plan in sub-project
// B. Without it an existing single-owner install switches accounts on and the
// apps already deployed under YACHT_OWNER_ID belong to a team nobody can sign
// in to.
func TestFirstSignInInheritsTheConfiguredOwner(t *testing.T) {
	h := newLiveHarness(t, "web-inherit")
	h.installedApp(t, "legacy-app")
	first := h.user(t, "first@web.test")

	rec := h.signIn(t, "first@web.test")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("following the link = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("no session cookie")
	}

	// The proof that owner_id scoping followed the session: the app the install
	// already had is on the page, so it is still reachable by somebody.
	body := h.getAs(t, "/apps", c).Body.String()
	if !strings.Contains(body, "legacy-app") {
		t.Fatalf("the first person to sign in cannot see the app the install "+
			"already deployed:\n%s", body)
	}

	role, err := h.accounts.RoleIn(context.Background(), first.ID, h.teamID)
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != account.RoleOwner {
		t.Fatalf("role in %s = %q, want owner", h.teamID, role)
	}
}

// ...and only the first. The second person must not be handed ownership of the
// existing team just for signing in.
func TestSecondSignInDoesNotInheritOwnership(t *testing.T) {
	h := newLiveHarness(t, "web-second")
	h.installedApp(t, "private-app")
	h.user(t, "first@web.test")
	second := h.user(t, "second@web.test")

	if code := h.signIn(t, "first@web.test").Code; code != http.StatusSeeOther {
		t.Fatalf("the first sign-in = %d, want 303", code)
	}

	rec := h.signIn(t, "second@web.test")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the second sign-in = %d, want 303", rec.Code)
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("no session cookie for the second person")
	}

	if _, err := h.accounts.RoleIn(context.Background(), second.ID, h.teamID); !errors.Is(
		err, account.ErrNotAMember) {
		t.Fatalf("signing in joined the second person to the configured team: %v", err)
	}
	if n := h.owners(t, h.teamID); n != 1 {
		t.Fatalf("owners of %s = %d, want 1", h.teamID, n)
	}

	// And the apps of the team they were never invited to are not on their
	// screen, which is what the ownership would have bought them.
	body := h.getAs(t, "/apps", c).Body.String()
	if strings.Contains(body, "private-app") {
		t.Fatalf("the second person to sign in can see the first team's apps:\n%s", body)
	}
}

// ------------------------------------------------------------- signing out
//
// Against the real service, because the claim being made is that the session
// stops working — not that a handler was called.

func TestSignOutClearsTheCookieAndRevokesTheSession(t *testing.T) {
	h := newLiveHarness(t, "web-signout")
	h.installedApp(t, "signout-app")
	h.user(t, "out@web.test")

	// Two browsers, the same person. Signing out of one must be signing out of
	// one: a sign-out that quietly ended every session would be indistinguishable
	// here from a correct one without the second.
	here := sessionCookie(h.signIn(t, "out@web.test"))
	elsewhere := sessionCookie(h.signIn(t, "out@web.test"))
	if here == nil || elsewhere == nil {
		t.Fatal("signing in twice did not produce two session cookies")
	}
	if here.Value == elsewhere.Value {
		t.Fatal("both sign-ins produced the same session — there is only one browser to test with")
	}
	if code := h.getAs(t, "/apps", here).Code; code != http.StatusOK {
		t.Fatalf("GET /apps before signing out = %d, want 200", code)
	}

	rec := h.postAs(t, "/sign-out", here)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /sign-out = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/sign-in" {
		t.Errorf("Location = %q, want /sign-in", loc)
	}

	cleared := sessionCookie(rec)
	if cleared == nil {
		t.Fatal("sign-out did not clear the session cookie")
	}
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("cleared cookie = %q MaxAge=%d, want empty with a negative MaxAge",
			cleared.Value, cleared.MaxAge)
	}

	// The cookie being dropped is the browser's side of it and the browser is
	// not the threat. What matters is that the value, kept by anyone who copied
	// it, no longer resolves to anything.
	if code := h.getAs(t, "/apps", here).Code; code != http.StatusUnauthorized {
		t.Fatalf("GET /apps with the signed-out session = %d, want 401 — the token still works",
			code)
	}
	if code := h.getAs(t, "/apps", elsewhere).Code; code != http.StatusOK {
		t.Fatalf("GET /apps on the other browser = %d, want 200 — signing out of one "+
			"browser signed out of all of them", code)
	}
}

// Without this a leaked session cannot be revoked at all: it runs until it
// expires and the person it belongs to can do nothing about it.
func TestSignOutEverywhereInvalidatesEveryDevice(t *testing.T) {
	h := newLiveHarness(t, "web-signout-all")
	h.installedApp(t, "everywhere-app")
	h.user(t, "all@web.test")
	h.user(t, "bystander@web.test")

	here := sessionCookie(h.signIn(t, "all@web.test"))
	phone := sessionCookie(h.signIn(t, "all@web.test"))
	stolen := sessionCookie(h.signIn(t, "all@web.test"))
	if here == nil || phone == nil || stolen == nil {
		t.Fatal("three sign-ins did not produce three session cookies")
	}
	for _, c := range []*http.Cookie{here, phone, stolen} {
		if code := h.getAs(t, "/apps", c).Code; code != http.StatusOK {
			t.Fatalf("GET /apps before signing out everywhere = %d, want 200", code)
		}
	}

	// Somebody else, signed in at the same time. Revoking one person's sessions
	// must not be an install-wide sign-out.
	other := sessionCookie(h.signIn(t, "bystander@web.test"))
	if other == nil {
		t.Fatal("the bystander did not get a session")
	}

	rec := h.postAs(t, "/sign-out-everywhere", here)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /sign-out-everywhere = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if cleared := sessionCookie(rec); cleared == nil || cleared.Value != "" {
		t.Fatal("sign-out-everywhere did not clear this browser's cookie")
	}

	for name, c := range map[string]*http.Cookie{
		"this browser": here, "the phone": phone, "the stolen cookie": stolen,
	} {
		if code := h.getAs(t, "/apps", c).Code; code != http.StatusUnauthorized {
			t.Errorf("GET /apps with %s = %d, want 401 — the session survived", name, code)
		}
	}
	if code := h.getAs(t, "/apps", other).Code; code != http.StatusOK {
		t.Errorf("GET /apps as the bystander = %d, want 200 — one person signing out "+
			"everywhere signed out the whole install", code)
	}
}

// GET must not sign anyone out: a prefetched or crawled link would. Browsers
// and mail clients fetch links nobody clicked, and a sign-out that answers them
// is a sign-out anyone can trigger with an <img> tag.
func TestSignOutRejectsGET(t *testing.T) {
	h := newLiveHarness(t, "web-signout-get")
	h.installedApp(t, "get-app")
	h.user(t, "getter@web.test")

	c := sessionCookie(h.signIn(t, "getter@web.test"))
	if c == nil {
		t.Fatal("no session cookie")
	}

	for _, path := range []string{"/sign-out", "/sign-out-everywhere"} {
		rec := h.getAs(t, path, c)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, rec.Code)
		}
		if cleared := sessionCookie(rec); cleared != nil && cleared.Value == "" {
			t.Errorf("GET %s cleared the cookie", path)
		}
		if code := h.getAs(t, "/apps", c).Code; code != http.StatusOK {
			t.Fatalf("GET %s ended the session: /apps then answered %d, want 200", path, code)
		}
	}
}

// A session can already be dead before its owner clicks sign out — removed from
// the team, or revoked from another browser. Sign-out is not authenticated, so
// that the cookie can still be cleared; refusing here would leave a browser
// holding a dead cookie with no way to drop it.
func TestSignOutWithADeadSessionStillClearsTheCookie(t *testing.T) {
	h := newLiveHarness(t, "web-signout-dead")
	h.user(t, "dead@web.test")

	c := sessionCookie(h.signIn(t, "dead@web.test"))
	if c == nil {
		t.Fatal("no session cookie")
	}
	if err := h.accounts.RevokeSession(context.Background(), c.Value); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	for _, path := range []string{"/sign-out", "/sign-out-everywhere"} {
		rec := h.postAs(t, path, c)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("POST %s with a dead session = %d, want 303\n%s",
				path, rec.Code, rec.Body.String())
		}
		if cleared := sessionCookie(rec); cleared == nil || cleared.Value != "" {
			t.Errorf("POST %s did not clear the dead cookie", path)
		}
	}

	// ...and with no cookie at all, which is what a second click on a stale page
	// sends.
	rec := h.postAs(t, "/sign-out", nil)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("POST /sign-out with no cookie = %d, want 303", rec.Code)
	}
}
