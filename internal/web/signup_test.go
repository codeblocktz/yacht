package web

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/identity"
)

func openSignupServer(t *testing.T, accounts Accounts, mailer *fakeMailer) http.Handler {
	t.Helper()
	return testServer(t, Options{
		Accounts:        accounts,
		Mailer:          mailer,
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: "web-signin",
		OpenSignup:      true,
		WakerBrand:      "Harbour",
		Identity: identity.ProviderFunc(func(context.Context, *http.Request) (identity.Owner, error) {
			return identity.Owner{}, identity.ErrUnauthenticated
		}),
	})
}

// With sign-up open a stranger is mailed a link that makes their account, and
// a registered person their sign-in link — and the visitor still cannot tell
// which: same status, same body, same headers.
func TestOpenSignupMailsAStrangerWithoutSayingSo(t *testing.T) {
	mailer := &fakeMailer{}
	h := openSignupServer(t, &fakeAccounts{}, mailer)

	registered := postSignIn(h, "known@example.test", "198.51.100.7:3000")
	stranger := postSignIn(h, "nobody@example.test", "198.51.100.8:3000")

	if registered.Code != http.StatusOK || stranger.Code != http.StatusOK {
		t.Fatalf("status: registered = %d, stranger = %d, want 200 both", registered.Code, stranger.Code)
	}
	if registered.Body.String() != stranger.Body.String() {
		t.Fatal("the page differs between a registered address and a stranger")
	}
	if !maps.EqualFunc(registered.Result().Header, stranger.Result().Header, slices.Equal) {
		t.Fatal("the headers differ between a registered address and a stranger")
	}

	sent := mailer.messages()
	if len(sent) != 2 {
		t.Fatalf("mail sent = %d, want one to each address", len(sent))
	}
	if !strings.Contains(sent[0].TextBody, "https://yacht.test/auth/") {
		t.Errorf("the registered address got no sign-in link:\n%s", sent[0].TextBody)
	}
	if sent[1].To != "nobody@example.test" ||
		!strings.Contains(sent[1].TextBody, "https://yacht.test/sign-up/") {
		t.Errorf("the stranger got no sign-up link: %+v", sent[1])
	}
	for _, m := range sent {
		if !strings.Contains(m.Subject, "Harbour") || strings.Contains(m.Subject+m.TextBody, "Yacht") {
			t.Errorf("mail not in the wrapper's name: %q\n%s", m.Subject, m.TextBody)
		}
	}
}

// Closed, the page says nothing of creating accounts, and a sign-up link
// already in a mailbox stops working.
func TestClosedSignupRefusesASignupLink(t *testing.T) {
	h := signInServer(t, &fakeAccounts{}, &fakeMailer{})

	if body := get(t, h, "/sign-in").Body.String(); strings.Contains(body, "creates your account") {
		t.Error("the sign-in page offers sign-up on an install where it is closed")
	}
	if code := get(t, h, "/sign-up/anything").Code; code != http.StatusUnauthorized {
		t.Fatalf("GET /sign-up/... with sign-up closed = %d, want 401", code)
	}
}

// ------------------------------------------------------------- end to end

func newOpenSignupHarness(t *testing.T, teamID, ownerEmail string) *liveHarness {
	t.Helper()
	return newLiveHarnessWith(t, teamID, ownerEmail, []string{"ops@web.test"},
		func(o *Options) { o.OpenSignup = true })
}

// signUp asks for a link as a stranger and follows the sign-up link mailed.
func (h *liveHarness) signUp(t *testing.T, email string) *http.Cookie {
	t.Helper()
	before := len(h.mailer.messages())
	if code := postSignIn(h.handler, email, "198.51.100.2:2000").Code; code != http.StatusOK {
		t.Fatalf("POST /sign-in for %s = %d, want 200", email, code)
	}
	sent := h.mailer.messages()
	if len(sent) != before+1 {
		t.Fatalf("mails sent = %d, want one more than %d", len(sent), before)
	}
	body := sent[len(sent)-1].TextBody
	i := strings.Index(body, "https://yacht.test/sign-up/")
	if i < 0 {
		t.Fatalf("no sign-up link in the mail:\n%s", body)
	}
	link := body[i:]
	if j := strings.IndexAny(link, " \r\n"); j >= 0 {
		link = link[:j]
	}
	rec := h.follow(t, link)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("following the sign-up link = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("signing up gave no session")
	}
	return c
}

// A stranger who signs up is signed in, to a team of their own — never the one
// the install runs as, even when they are the first person through the door.
func TestSignupGivesAStrangerATeamOfTheirOwn(t *testing.T) {
	h := newOpenSignupHarness(t, "web-signup", "owner@web.test")
	h.installedApp(t, "the-installs-app")

	c := h.signUp(t, "stranger@web.test")

	if n := h.owners(t, h.teamID); n != 0 {
		t.Fatalf("owners of the install's team = %d after a stranger signed up, want 0", n)
	}
	body := h.getAs(t, "/apps", c).Body.String()
	if strings.Contains(body, "the-installs-app") {
		t.Fatal("a stranger who signed up can see the install's own apps")
	}

	var teams int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE u.email = 'stranger@web.test' AND m.role = 'owner'`).Scan(&teams); err != nil {
		t.Fatalf("count teams: %v", err)
	}
	if teams != 1 {
		t.Fatalf("the stranger owns %d teams, want their own one", teams)
	}

	// Signing in again later is an ordinary sign-in link to the same team.
	again := h.signIn(t, "stranger@web.test")
	if again.Code != http.StatusSeeOther {
		t.Fatalf("signing in again = %d, want 303", again.Code)
	}
	if teams := h.ownedBy(t, "stranger@web.test"); teams != 1 {
		t.Fatalf("signing in again gave %d teams, want still one", teams)
	}
}

// The configured owner still inherits the install's team, however many
// strangers came first.
func TestSignupKeepsTheInstallsTeamForItsOwner(t *testing.T) {
	h := newOpenSignupHarness(t, "web-signup-owner", "owner@web.test")
	h.installedApp(t, "the-installs-app")

	h.signUp(t, "early@web.test")
	// The configured owner is made on asking, as on any install, so theirs is
	// an ordinary sign-in link rather than a sign-up one.
	rec := h.signIn(t, "owner@web.test")
	c := sessionCookie(rec)
	if c == nil {
		t.Fatalf("the configured owner got no session: %d", rec.Code)
	}

	body := h.getAs(t, "/apps", c).Body.String()
	if !strings.Contains(body, "the-installs-app") {
		t.Fatal("the configured owner did not inherit the install's team")
	}
	owner, err := h.accounts.EnsureUser(context.Background(), "owner@web.test", "")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if role, err := h.accounts.RoleIn(context.Background(), owner.ID, h.teamID); err != nil || role != account.RoleOwner {
		t.Fatalf("owner's role in the install's team = %q, %v", role, err)
	}
	early, _ := h.accounts.EnsureUser(context.Background(), "early@web.test", "")
	if _, err := h.accounts.RoleIn(context.Background(), early.ID, h.teamID); !errors.Is(err, account.ErrNotAMember) {
		t.Fatalf("the early stranger joined the install's team: %v", err)
	}
}

func (h *liveHarness) ownedBy(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE u.email = $1 AND m.role = 'owner'`, email).Scan(&n); err != nil {
		t.Fatalf("count teams: %v", err)
	}
	return n
}
