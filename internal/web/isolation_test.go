package web

// Tenant isolation, written by attacking the finished surface rather than by
// following the plan. Each of these names a way in that the feature's own
// tests did not think to close: reaching another team's rows by id, carrying a
// role across a team switch, and letting sign-in adopt a token the caller
// chose.
//
// They are here rather than thrown away because each is a property that could
// be broken by a change that looks unrelated — a query losing its owner_id
// predicate, a role read from the wrong place, a cookie written before the
// session is minted.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
)

// cross-team IDOR. Admin of team A, acting on team B's objects by id.
// The queries are scoped by owner_id; this proves it through HTTP.
func TestIsolationCrossTeamMemberRemoval(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t, "web-adv-a")

	// Team A, where the attacker is owner.
	h.user(t, "attacker-adv@web.test")
	attackerCookie := sessionCookie(h.signIn(t, "attacker-adv@web.test"))

	// Team B, entirely separate, with a victim member.
	bOwner := h.user(t, "b-owner-adv@web.test")
	if _, err := h.accounts.CreateTeam(ctx, "web-adv-b", "B", bOwner.ID); err != nil {
		t.Fatalf("create team B: %v", err)
	}
	victim := h.user(t, "b-victim-adv@web.test")
	if err := h.accounts.SetRole(ctx, bOwner.ID, "web-adv-b", victim.ID, account.RoleMember); err != nil {
		t.Fatalf("SetRole in B: %v", err)
	}

	// The attacker, whose session is on team A, names team B's member by id.
	rec := h.postFormAs(t, "/team/members/"+victim.ID.String()+"/remove", attackerCookie, url.Values{})
	t.Logf("cross-team remove -> %d", rec.Code)

	if role, err := h.accounts.RoleIn(ctx, victim.ID, "web-adv-b"); err != nil || role != account.RoleMember {
		t.Fatalf("EXPLOIT: a member of team B was removed by an owner of team A (role=%q err=%v)", role, err)
	}
}

// cross-team invitation revocation by id.
func TestIsolationCrossTeamInvitationRevoke(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t, "web-adv2-a")

	attacker := h.user(t, "attacker2-adv@web.test")
	_ = attacker
	attackerCookie := sessionCookie(h.signIn(t, "attacker2-adv@web.test"))

	bOwner := h.user(t, "b2-owner-adv@web.test")
	if _, err := h.accounts.CreateTeam(ctx, "web-adv2-b", "B", bOwner.ID); err != nil {
		t.Fatalf("create team B: %v", err)
	}
	if _, err := h.accounts.Invite(ctx, bOwner.ID, "web-adv2-b", "pending2@web.test",
		account.RoleMember, time.Hour); err != nil {
		t.Fatalf("invite in B: %v", err)
	}
	pending, err := h.accounts.ListPendingInvitations(ctx, "web-adv2-b")
	if err != nil || len(pending) != 1 {
		t.Fatalf("team B invitations = %v (%v)", pending, err)
	}

	rec := h.postFormAs(t,
		"/team/invitations/"+pending[0].ID.String()+"/revoke", attackerCookie, url.Values{})
	t.Logf("cross-team revoke -> %d", rec.Code)

	after, err := h.accounts.ListPendingInvitations(ctx, "web-adv2-b")
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != 1 {
		t.Fatal("EXPLOIT: an owner of team A revoked team B's invitation")
	}
}

// does the role follow a team switch? Owner of A, mere member of B.
// After switching to B they must lose admin powers, not carry A's role over.
func TestIsolationRoleFollowsTheTeamSwitch(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t, "web-adv3-a")

	person := h.user(t, "dual-adv@web.test")
	cookie := sessionCookie(h.signIn(t, "dual-adv@web.test"))

	// A second team where the same person is only a member.
	bOwner := h.user(t, "b3-owner-adv@web.test")
	if _, err := h.accounts.CreateTeam(ctx, "web-adv3-b", "B", bOwner.ID); err != nil {
		t.Fatalf("create team B: %v", err)
	}
	if err := h.accounts.SetRole(ctx, bOwner.ID, "web-adv3-b", person.ID, account.RoleMember); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	if rec := h.postFormAs(t, "/teams/switch", cookie,
		url.Values{"team": {"web-adv3-b"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("switch = %d, want 303", rec.Code)
	}
	if got := h.activeTeam(t, cookie); got != "web-adv3-b" {
		t.Fatalf("active team = %q, want web-adv3-b", got)
	}

	// As a mere member of B, inviting must be refused.
	rec := h.postFormAs(t, "/team/invite", cookie,
		url.Values{"email": {"sneak-adv@web.test"}, "role": {"admin"}})
	t.Logf("invite as member-of-B (owner of A) -> %d", rec.Code)
	if rec.Code == http.StatusSeeOther {
		t.Fatal("EXPLOIT: owner of team A kept administrative power after switching into team B")
	}

	// And removing team B's owner must be refused.
	rec = h.postFormAs(t, "/team/members/"+bOwner.ID.String()+"/remove", cookie, url.Values{})
	if role, err := h.accounts.RoleIn(ctx, bOwner.ID, "web-adv3-b"); err != nil || role != account.RoleOwner {
		t.Fatalf("EXPLOIT: team B's owner removed by a member (status %d, role %q)", rec.Code, role)
	}
}

// the team page must show only your own team's people.
func TestIsolationTeamPageDoesNotLeakAnotherTeam(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t, "web-adv4-a")

	h.user(t, "a4-owner-adv@web.test")
	cookie := sessionCookie(h.signIn(t, "a4-owner-adv@web.test"))

	bOwner := h.user(t, "b4-secret-adv@web.test")
	if _, err := h.accounts.CreateTeam(ctx, "web-adv4-b", "B", bOwner.ID); err != nil {
		t.Fatalf("create team B: %v", err)
	}

	body := h.getAs(t, "/team", cookie).Body.String()
	if strings.Contains(body, "b4-secret-adv@web.test") {
		t.Fatal("EXPLOIT: the team page listed a member of another team")
	}
}

// Signing in must mint a fresh token, never adopt one the caller already
// presented. A server that reuses a presented value lets an attacker fix a
// session id in advance and then wait for the victim to authenticate it.
func TestIsolationSignInDoesNotAdoptAPresentedToken(t *testing.T) {
	h := newLiveHarness(t, "web-iso5")
	h.user(t, "fixation-iso@web.test")

	planted := &http.Cookie{Name: SessionCookie, Value: "attacker-chosen-token-value"}

	before := len(h.mailer.messages())
	req := httptest.NewRequest(http.MethodPost, "/sign-in",
		strings.NewReader(url.Values{"email": {"fixation-iso@web.test"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(planted)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	sent := h.mailer.messages()
	if len(sent) != before+1 {
		t.Fatalf("no sign-in mail: %d", len(sent))
	}
	c := sessionCookie(h.follow(t, linkIn(t, sent[len(sent)-1].TextBody)))
	if c == nil {
		t.Fatal("the callback issued no cookie")
	}
	if c.Value == planted.Value {
		t.Fatal("sign-in adopted the token the caller presented — session fixation")
	}
}

// TestEveryPageIsReachableByClicking is the other half of
// TestEveryNavTargetResolves.
//
// That one proves every advertised page exists. This one proves every page is
// advertised — a page nobody can navigate to is one only its author knows
// about, which is how team management shipped and stayed invisible.
func TestEveryPageIsReachableByClicking(t *testing.T) {
	h := newLiveHarness(t, "web-nav")
	h.user(t, "nav@web.test")
	c := sessionCookie(h.signIn(t, "nav@web.test"))

	body := h.getAs(t, "/", c).Body.String()

	// Every top-level page a person is meant to use. Sub-pages of an app are
	// reached from the app, so they are not listed here.
	for _, href := range []string{
		"/", "/apps", "/deployments",
		"/cluster/nodes", "/cluster/pods", "/cluster/volumes", "/cluster/events",
		"/team", "/settings",
	} {
		if !strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("no link to %s — the page exists but nothing points at it", href)
		}
	}
}

// TestFailedActionsDoNotReportSuccess.
//
// Scale, redeploy and delete logged their failures and redirected as though
// they had worked. The person saw the page they expected, the workload was
// unchanged, and the only record was a log line nobody was reading — which is
// how a broken deploy strategy went unnoticed through six passing tests.
func TestFailedActionsDoNotReportSuccess(t *testing.T) {
	srv := testServer(t, Options{Apps: &failingApps{}})

	for _, path := range []string{
		"/apps/web/scale", "/apps/web/redeploy", "/apps/web/delete",
	} {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader("replicas=2"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code == http.StatusSeeOther {
			t.Errorf("POST %s failed and answered 303 — the caller is told it worked", path)
		}
	}
}

// failingApps refuses everything, so a handler that swallows an error is
// visible as a success the caller should not have been given.
type failingApps struct{ fakeApps }

func (f *failingApps) Scale(context.Context, string, string, int32) (app.App, error) {
	return app.App{}, errors.New("nope")
}
func (f *failingApps) Redeploy(context.Context, string, string) error {
	return errors.New("nope")
}
func (f *failingApps) Delete(context.Context, string, string) error {
	return errors.New("nope")
}
func (f *failingApps) Get(context.Context, string, string) (app.App, error) {
	return app.App{Name: "web"}, nil
}
