package web

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
)

// ------------------------------------------------------------ the switcher
//
// Against the real account service and a real database throughout. Switching
// team is an authorization decision — it changes the owner every later query is
// scoped by — so what has to be proved is that the session moved, or did not,
// and no fake can say that.

// postFormAs sends a form the way the switcher's own markup does.
func (h *liveHarness) postFormAs(
	t *testing.T, path string, c *http.Cookie, form url.Values,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// activeTeam reads the team a cookie's session is acting as, from the database
// rather than from anything the response said about itself.
func (h *liveHarness) activeTeam(t *testing.T, c *http.Cookie) string {
	t.Helper()
	sess, err := h.accounts.ResolveSession(context.Background(), c.Value)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	return sess.ActiveTeamID
}

// team creates a team owned by someone, with an app in it.
func (h *liveHarness) team(t *testing.T, id, name string, owner account.User, appName string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.accounts.CreateTeam(ctx, id, name, owner.ID); err != nil {
		t.Fatalf("create team %s: %v", id, err)
	}
	if appName == "" {
		return
	}
	if _, err := h.apps.Create(ctx, id, app.CreateInput{
		Name: appName, Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("create app %s in %s: %v", appName, id, err)
	}
}

// The one that matters: switching is an authorization decision, not a
// preference. A crafted POST must not move a session into someone else's team.
func TestCannotSwitchIntoATeamYouAreNotIn(t *testing.T) {
	h := newLiveHarness(t, "web-switch-outsider")
	h.installedApp(t, "insider-app")
	h.user(t, "insider@web.test")
	h.user(t, "outsider@web.test")

	// The insider takes the configured team, so the outsider gets one of their
	// own — which is the position anyone signing in to a running install is in.
	if code := h.signIn(t, "insider@web.test").Code; code != http.StatusSeeOther {
		t.Fatalf("the insider's sign-in = %d, want 303", code)
	}
	c := sessionCookie(h.signIn(t, "outsider@web.test"))
	if c == nil {
		t.Fatal("the outsider got no session")
	}
	mine := h.activeTeam(t, c)
	if mine == h.teamID {
		t.Fatalf("the outsider is already acting as %s — there is nothing left to "+
			"switch into", h.teamID)
	}

	rec := h.postFormAs(t, "/teams/switch", c, url.Values{"team": {h.teamID}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /teams/switch into a team they are not in = %d, want 403\n%s",
			rec.Code, rec.Body.String())
	}

	// The status is the smaller half of it. The session must not have moved.
	if now := h.activeTeam(t, c); now != mine {
		t.Fatalf("the session is now acting as %q, want %q — a crafted form moved "+
			"it into a team its holder is not a member of", now, mine)
	}
	// ...and the apps of that team are still not on their screen, which is what
	// the switch would have bought them.
	if body := h.getAs(t, "/apps", c).Body.String(); strings.Contains(body, "insider-app") {
		t.Fatalf("the outsider can see the other team's apps:\n%s", body)
	}
}

// The end-to-end proof that owner_id scoping follows the session: the same
// cookie, the same person, and a different set of apps on the page.
func TestSwitchingChangesWhichAppsAreVisible(t *testing.T) {
	h := newLiveHarness(t, "web-switch-apps")
	h.installedApp(t, "first-app")
	u := h.user(t, "both@web.test")

	c := sessionCookie(h.signIn(t, "both@web.test"))
	if c == nil {
		t.Fatal("no session cookie")
	}
	const second = "web-switch-apps-two"
	h.team(t, second, "Second", u, "second-app")

	body := h.getAs(t, "/apps", c).Body.String()
	if !strings.Contains(body, "first-app") || strings.Contains(body, "second-app") {
		t.Fatalf("before switching, /apps must show only the first team's apps:\n%s", body)
	}

	rec := h.postFormAs(t, "/teams/switch", c, url.Values{"team": {second}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /teams/switch = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want / — the page they were on belongs to the "+
			"team they just left", loc)
	}
	if now := h.activeTeam(t, c); now != second {
		t.Fatalf("active team = %q, want %q", now, second)
	}

	// The same cookie, unchanged: what moved is the session, not the browser.
	body = h.getAs(t, "/apps", c).Body.String()
	if !strings.Contains(body, "second-app") {
		t.Errorf("after switching, /apps does not show the new team's apps:\n%s", body)
	}
	if strings.Contains(body, "first-app") {
		t.Errorf("after switching, /apps still shows the old team's apps — owner_id "+
			"scoping did not follow the session:\n%s", body)
	}
}

// A switcher offering a team you are not in is either a broken control or an
// invitation to try the request the test above forbids.
func TestSwitcherListsOnlyYourTeams(t *testing.T) {
	h := newLiveHarness(t, "web-switch-list")
	h.installedApp(t, "list-app")
	u := h.user(t, "lister@web.test")
	stranger := h.user(t, "stranger@web.test")

	c := sessionCookie(h.signIn(t, "lister@web.test"))
	if c == nil {
		t.Fatal("no session cookie")
	}
	h.team(t, "web-switch-list-mine", "My Other Team", u, "")
	h.team(t, "web-switch-list-theirs", "Somebody Elses Team", stranger, "")

	body := h.getAs(t, "/apps", c).Body.String()

	// The control is really on the page, or the absences below prove nothing.
	if !strings.Contains(body, `action="/teams/switch"`) {
		t.Fatalf("no team switcher on the page:\n%s", body)
	}
	for _, want := range []string{
		`value="web-switch-list"`, `value="web-switch-list-mine"`, "My Other Team",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the switcher is missing %q — a team the person belongs to is "+
				"not offered", want)
		}
	}
	for _, unwanted := range []string{"web-switch-list-theirs", "Somebody Elses Team"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the switcher offers %q, which belongs to somebody else:\n%s",
				unwanted, body)
		}
	}
}

// GET must not switch anyone's team: a prefetched or crawled link would, and
// landing in another team without asking is both confusing and a way to make
// somebody act in a team they did not choose.
func TestSwitchRejectsGET(t *testing.T) {
	h := newLiveHarness(t, "web-switch-get")
	h.installedApp(t, "get-app")
	u := h.user(t, "getter@web.test")

	c := sessionCookie(h.signIn(t, "getter@web.test"))
	if c == nil {
		t.Fatal("no session cookie")
	}
	const second = "web-switch-get-two"
	h.team(t, second, "Second", u, "")
	before := h.activeTeam(t, c)

	rec := h.getAs(t, "/teams/switch?team="+second, c)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /teams/switch = %d, want 405", rec.Code)
	}
	if now := h.activeTeam(t, c); now != before {
		t.Fatalf("a GET moved the session from %q to %q", before, now)
	}
}

// ------------------------------------------------------ team management
//
// Against the real account service and a real database throughout. Every claim
// here — that a session dies with its membership, that a revoked invitation
// stops working, that the last owner cannot be removed — is a fact about rows
// in three tables, and a fake asked the same questions would only repeat the
// answers it was written with.

// invite sends the management page's own invitation form.
func (h *liveHarness) invite(
	t *testing.T, c *http.Cookie, email, role string,
) *httptest.ResponseRecorder {
	t.Helper()
	return h.postFormAs(t, "/team/invite", c, url.Values{
		"email": {email}, "role": {role},
	})
}

// invitationToken pulls the token out of the invitation mail, which is the only
// place it exists — the same position the invited person is in.
func invitationToken(t *testing.T, body string) string {
	t.Helper()
	const prefix = "https://yacht.test/invitations/"
	i := strings.Index(body, prefix)
	if i < 0 {
		t.Fatalf("no invitation link in the mail:\n%s", body)
	}
	token := body[i+len(prefix):]
	if j := strings.IndexAny(token, " \r\n"); j >= 0 {
		token = token[:j]
	}
	if token == "" {
		t.Fatalf("the invitation link carries no token:\n%s", body)
	}
	return token
}

// lastMail is the message the action under test caused, or a failure saying it
// caused none.
func (h *liveHarness) lastMail(t *testing.T, before int) string {
	t.Helper()
	sent := h.mailer.messages()
	if len(sent) != before+1 {
		t.Fatalf("mails sent = %d, want one more than %d", len(sent), before)
	}
	return sent[len(sent)-1].TextBody
}

// invitationAction finds the id in a revoke form, which is how the page offers
// an invitation to be withdrawn.
var invitationAction = regexp.MustCompile(
	`/team/invitations/([0-9a-fA-F-]{36})/revoke`)

func onlyInvitationID(t *testing.T, body string) string {
	t.Helper()
	found := invitationAction.FindAllStringSubmatch(body, -1)
	if len(found) != 1 {
		t.Fatalf("found %d revoke forms on the page, want exactly 1:\n%s", len(found), body)
	}
	return found[0][1]
}

func TestTeamPageListsMembersAndPendingInvitations(t *testing.T) {
	rt := newRoleTeam(t, "web-team-page", "team-app")

	if rec := rt.invite(t, rt.owner, "invitee@web.test", "member"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /team/invite = %d, want 303\n%s", rec.Code, rec.Body.String())
	}

	rec := rt.getAs(t, "/team", rt.owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /team = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Everyone in the team, and the one person who has been asked but has not
	// arrived. A page missing either is a page that cannot be acted on.
	for _, want := range []string{
		"owner@web.test", "admin@web.test", "member@web.test", "invitee@web.test",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the team page does not list %q:\n%s", want, body)
		}
	}

	// The rows are real controls rather than text: each member can be acted on
	// by id, and the invitation can be withdrawn.
	for _, want := range []string{
		`action="/team/members/` + rt.memberID.String() + `/remove"`,
		`action="/team/members/` + rt.adminID.String() + `/role"`,
		`action="/team/invite"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the team page is missing %q:\n%s", want, body)
		}
	}
	if len(invitationAction.FindAllString(body, -1)) != 1 {
		t.Errorf("the pending invitation has no revoke form:\n%s", body)
	}

	// A member may read who they work with. What they may change is a separate
	// question, and the gates answer it.
	if code := rt.getAs(t, "/team", rt.member).Code; code != http.StatusOK {
		t.Errorf("GET /team as a member = %d, want 200", code)
	}
}

func TestInviteSendsMailAndShowsPending(t *testing.T) {
	rt := newRoleTeam(t, "web-team-invite", "invite-app")
	before := len(rt.mailer.messages())

	rec := rt.invite(t, rt.admin, "newcomer@web.test", "member")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /team/invite as an admin = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/team" {
		t.Errorf("Location = %q, want /team", loc)
	}

	sent := rt.mailer.messages()
	if len(sent) != before+1 {
		t.Fatalf("mails sent = %d, want one more than %d — an invitation nobody "+
			"receives is not an invitation", len(sent), before)
	}
	msg := sent[len(sent)-1]
	if msg.To != "newcomer@web.test" {
		t.Errorf("the invitation went to %q", msg.To)
	}
	// Built from the configured base URL, never from the Host header: a link
	// built from a header is one an attacker can point at their own server and
	// have Yacht mail to a real person.
	if !strings.Contains(msg.TextBody, "https://yacht.test/invitations/") {
		t.Errorf("no invitation link in the mail:\n%s", msg.TextBody)
	}

	if body := rt.getAs(t, "/team", rt.admin).Body.String(); !strings.Contains(
		body, "newcomer@web.test") {
		t.Errorf("the invitation was sent but is not shown as pending, so there is "+
			"nothing to revoke:\n%s", body)
	}
}

// An admin must not be able to mint an owner — that is ownership. An admin who
// could invite an owner could invite themselves back in with every permission
// the team has.
func TestAdminCannotInviteAnOwner(t *testing.T) {
	rt := newRoleTeam(t, "web-team-mint", "mint-app")
	before := len(rt.mailer.messages())

	rec := rt.invite(t, rt.admin, "usurper@web.test", "owner")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an admin inviting an owner = %d, want 403\n%s", rec.Code, rec.Body.String())
	}
	if got := rt.mailer.messages(); len(got) != before {
		t.Errorf("mails sent = %d, want %d — the refused invitation was posted anyway",
			len(got), before)
	}
	if body := rt.getAs(t, "/team", rt.owner).Body.String(); strings.Contains(
		body, "usurper@web.test") {
		t.Fatalf("the refused invitation exists:\n%s", body)
	}

	// The refusal is about who is asking, not about the role being unavailable:
	// the owner may hand out ownership.
	if rec := rt.invite(t, rt.owner, "usurper@web.test", "owner"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the owner inviting an owner = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
}

// Revoking is the only way to take back an invitation sent to the wrong
// address: the mail itself cannot be recalled, so the token has to stop working.
func TestRevokedInvitationDisappearsAndStopsWorking(t *testing.T) {
	rt := newRoleTeam(t, "web-team-revoke", "revoke-app")
	dropped := rt.user(t, "dropped@web.test")
	kept := rt.user(t, "kept@web.test")

	before := len(rt.mailer.messages())
	if rec := rt.invite(t, rt.owner, "dropped@web.test", "member"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /team/invite = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	droppedToken := invitationToken(t, rt.lastMail(t, before))

	id := onlyInvitationID(t, rt.getAs(t, "/team", rt.owner).Body.String())
	rec := rt.postFormAs(t, "/team/invitations/"+id+"/revoke", rt.admin, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST revoke = %d, want 303\n%s", rec.Code, rec.Body.String())
	}

	body := rt.getAs(t, "/team", rt.owner).Body.String()
	if strings.Contains(body, "dropped@web.test") {
		t.Errorf("the revoked invitation is still on the page:\n%s", body)
	}
	if found := invitationAction.FindAllString(body, -1); len(found) != 0 {
		t.Errorf("revoke forms still on the page: %v", found)
	}

	// The page is the smaller half of it. The token in the mail must be dead.
	if _, _, err := rt.accounts.AcceptInvitation(
		context.Background(), droppedToken, dropped.ID,
	); !errors.Is(err, account.ErrTokenInvalid) {
		t.Fatalf("accepting the revoked invitation: %v, want ErrTokenInvalid — the "+
			"mail is still a way into the team", err)
	}

	// ...and an invitation that was not revoked still works, so the failure above
	// is the revocation rather than invitations never working at all.
	before = len(rt.mailer.messages())
	if rec := rt.invite(t, rt.owner, "kept@web.test", "member"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /team/invite = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	keptToken := invitationToken(t, rt.lastMail(t, before))
	team, role, err := rt.accounts.AcceptInvitation(context.Background(), keptToken, kept.ID)
	if err != nil {
		t.Fatalf("accepting the invitation that was left alone: %v", err)
	}
	if team != rt.teamID || role != account.RoleMember {
		t.Fatalf("accepted into %q as %q, want %q as member", team, role, rt.teamID)
	}
}

// The HTTP-level proof that a session cannot outlive the membership it depends
// on: the removed person's cookie must stop working on their next request, not
// when it expires.
func TestRemovingAMemberEndsTheirSession(t *testing.T) {
	rt := newRoleTeam(t, "web-team-remove", "remove-app")

	if code := rt.getAs(t, "/apps", rt.member).Code; code != http.StatusOK {
		t.Fatalf("GET /apps as the member before removal = %d, want 200", code)
	}

	rec := rt.postFormAs(t, "/team/members/"+rt.memberID.String()+"/remove", rt.owner, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST remove = %d, want 303\n%s", rec.Code, rec.Body.String())
	}

	if rec := rt.getAs(t, "/apps", rt.member); !deniedGET(t, rec, "web-") {
		t.Fatalf("GET /apps as the removed member = %d — their cookie still "+
			"opens the team they were taken out of", rec.Code)
	}
	if body := rt.getAs(t, "/team", rt.owner).Body.String(); strings.Contains(
		body, "member@web.test") {
		t.Errorf("the removed member is still listed:\n%s", body)
	}
	if _, err := rt.accounts.RoleIn(
		context.Background(), rt.memberID, rt.teamID,
	); !errors.Is(err, account.ErrNotAMember) {
		t.Fatalf("role after removal: %v, want ErrNotAMember", err)
	}
}

// A team with no owner cannot be administered by anyone, ever again — there is
// no route back that does not go through the database.
func TestCannotRemoveOrDemoteTheLastOwner(t *testing.T) {
	rt := newRoleTeam(t, "web-team-lastowner", "last-app")
	ctx := context.Background()

	stillOwner := func(what string) {
		t.Helper()
		role, err := rt.accounts.RoleIn(ctx, rt.ownerID, rt.teamID)
		if err != nil || role != account.RoleOwner {
			t.Fatalf("after %s the only owner holds %q (%v), want owner — the team "+
				"can no longer be administered by anybody", what, role, err)
		}
	}

	demote := "/team/members/" + rt.ownerID.String() + "/role"
	if rec := rt.postFormAs(t, demote, rt.owner, url.Values{
		"role": {"member"},
	}); rec.Code < 400 {
		t.Errorf("the last owner demoted themselves: %d\n%s", rec.Code, rec.Body.String())
	}
	stillOwner("a self-demotion")

	remove := "/team/members/" + rt.ownerID.String() + "/remove"
	if rec := rt.postFormAs(t, remove, rt.owner, nil); rec.Code < 400 {
		t.Errorf("the last owner removed themselves: %d\n%s", rec.Code, rec.Body.String())
	}
	stillOwner("a self-removal")
	if code := rt.getAs(t, "/apps", rt.owner).Code; code != http.StatusOK {
		t.Errorf("GET /apps as the owner = %d, want 200 — the refused removal took "+
			"their session with it", code)
	}

	// The refusal is about the team keeping an owner, not about owners being
	// unchangeable: with a second one appointed, the same demotion goes through.
	if rec := rt.postFormAs(t, "/team/members/"+rt.adminID.String()+"/role", rt.owner,
		url.Values{"role": {"owner"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("appointing a second owner = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if rec := rt.postFormAs(t, demote, rt.owner, url.Values{
		"role": {"member"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("demoting one of two owners = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if role, err := rt.accounts.RoleIn(ctx, rt.ownerID, rt.teamID); err != nil ||
		role != account.RoleMember {
		t.Fatalf("role after the demotion = %q (%v), want member", role, err)
	}
}

// A management page that prints the invitation link lets anyone who can read it
// impersonate the invitee — including whoever is looking over their shoulder.
func TestPendingInvitationDoesNotLeakItsToken(t *testing.T) {
	rt := newRoleTeam(t, "web-team-token", "token-app")

	before := len(rt.mailer.messages())
	if rec := rt.invite(t, rt.owner, "quiet@web.test", "member"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /team/invite = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	token := invitationToken(t, rt.lastMail(t, before))

	body := rt.getAs(t, "/team", rt.owner).Body.String()
	// The invitation really is on the page, or the absences below prove nothing.
	if !strings.Contains(body, "quiet@web.test") {
		t.Fatalf("the invitation is not on the page at all:\n%s", body)
	}

	hash := account.HashToken(token)
	for what, secret := range map[string]string{
		"the raw token":       token,
		"its hash, hex":       hex.EncodeToString(hash),
		"its hash, base64":    base64.StdEncoding.EncodeToString(hash),
		"its hash, base64url": base64.RawURLEncoding.EncodeToString(hash),
	} {
		if strings.Contains(body, secret) {
			t.Errorf("the team page renders %s — anyone who can see the page can "+
				"accept the invitation as its recipient", what)
		}
	}
}

// ------------------------------------------------- accepting an invitation
//
// Task 7 built the page that sends invitation links. Without this, those links
// go nowhere — so these tests are the difference between the feature working
// and the feature mailing a 404.

// TestAcceptingAnInvitationWhileSignedInAddsTheTeam is the straightforward
// path: the person is already known, so the token only has to be spent.
func TestAcceptingAnInvitationWhileSignedInAddsTheTeam(t *testing.T) {
	h := newLiveHarness(t, "web-inv-signedin")
	owner := h.user(t, "owner-inv-in@web.test")
	h.team(t, "web-inv-signedin", "Team", owner, "")

	ownerCookie := sessionCookie(h.signIn(t, "owner-inv-in@web.test"))

	before := len(h.mailer.messages())
	if rec := h.invite(t, ownerCookie, "joiner-in@web.test", "member"); rec.Code != http.StatusSeeOther {
		t.Fatalf("invite = %d, want 303", rec.Code)
	}
	token := invitationToken(t, h.lastMail(t, before))

	// The invited person signs in first, on their own address.
	h.user(t, "joiner-in@web.test")
	joinerCookie := sessionCookie(h.signIn(t, "joiner-in@web.test"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/invitations/"+token, nil)
	req.AddCookie(joinerCookie)
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("accept = %d, want 303 — body: %s", rec.Code, rec.Body.String())
	}

	joiner, err := h.accounts.EnsureUser(context.Background(), "joiner-in@web.test", "")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	role, err := h.accounts.RoleIn(context.Background(), joiner.ID, "web-inv-signedin")
	if err != nil {
		t.Fatalf("the invitation did not put them in the team: %v", err)
	}
	if role != account.RoleMember {
		t.Fatalf("role = %q, want member", role)
	}
}

// TestAcceptingAnInvitationWhileSignedOutProvesTheAddressFirst is the one that
// matters.
//
// Acceptance is bound to the address the invitation names, so the token cannot
// be treated as proof of who is holding it. A signed-out visitor must be sent
// a sign-in link to the invited address — which means an intercepted or
// forwarded invitation is useless to anyone who cannot read that mailbox.
func TestAcceptingAnInvitationWhileSignedOutProvesTheAddressFirst(t *testing.T) {
	h := newLiveHarness(t, "web-inv-signedout")
	owner := h.user(t, "owner-inv-out@web.test")
	h.team(t, "web-inv-signedout", "Team", owner, "")

	ownerCookie := sessionCookie(h.signIn(t, "owner-inv-out@web.test"))

	before := len(h.mailer.messages())
	h.invite(t, ownerCookie, "joiner-out@web.test", "member")
	token := invitationToken(t, h.lastMail(t, before))

	// Follow the link with no cookie at all.
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/invitations/"+token, nil))

	if rec.Code == http.StatusSeeOther {
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "sign-in") {
			t.Fatalf("a signed-out visitor was redirected to %q, not to sign in", loc)
		}
	} else if rec.Code != http.StatusOK {
		t.Fatalf("accept while signed out = %d, want 200 or a redirect to sign-in", rec.Code)
	}

	// Nobody joined on the strength of holding the token.
	joiner := h.user(t, "joiner-out@web.test")
	if _, err := h.accounts.RoleIn(
		context.Background(), joiner.ID, "web-inv-signedout",
	); !errors.Is(err, account.ErrNotAMember) {
		t.Fatal("holding the token alone put somebody in the team")
	}
}

// A stranger who gets hold of the link cannot use it, because acceptance is
// bound to the invited address rather than to whoever presents the token.
func TestAStrangerCannotSpendSomebodyElsesInvitation(t *testing.T) {
	h := newLiveHarness(t, "web-inv-stranger")
	owner := h.user(t, "owner-inv-str@web.test")
	h.team(t, "web-inv-stranger", "Team", owner, "")

	ownerCookie := sessionCookie(h.signIn(t, "owner-inv-str@web.test"))

	before := len(h.mailer.messages())
	h.invite(t, ownerCookie, "intended-str@web.test", "admin")
	token := invitationToken(t, h.lastMail(t, before))

	h.user(t, "stranger-str@web.test")
	strangerCookie := sessionCookie(h.signIn(t, "stranger-str@web.test"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/invitations/"+token, nil)
	req.AddCookie(strangerCookie)
	h.handler.ServeHTTP(rec, req)

	stranger, _ := h.accounts.EnsureUser(context.Background(), "stranger-str@web.test", "")
	if _, err := h.accounts.RoleIn(
		context.Background(), stranger.ID, "web-inv-stranger",
	); !errors.Is(err, account.ErrNotAMember) {
		t.Fatalf("a stranger spent an invitation addressed to somebody else (status %d)", rec.Code)
	}
}

// A spent invitation is spent. Otherwise the link in a mailbox stays a way in
// long after it was used.
func TestAcceptedInvitationCannotBeReplayed(t *testing.T) {
	h := newLiveHarness(t, "web-inv-replay")
	owner := h.user(t, "owner-inv-rep@web.test")
	h.team(t, "web-inv-replay", "Team", owner, "")

	ownerCookie := sessionCookie(h.signIn(t, "owner-inv-rep@web.test"))

	before := len(h.mailer.messages())
	h.invite(t, ownerCookie, "joiner-rep@web.test", "member")
	token := invitationToken(t, h.lastMail(t, before))

	h.user(t, "joiner-rep@web.test")
	joinerCookie := sessionCookie(h.signIn(t, "joiner-rep@web.test"))

	accept := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/invitations/"+token, nil)
		req.AddCookie(joinerCookie)
		h.handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := accept(); code != http.StatusSeeOther {
		t.Fatalf("first accept = %d, want 303", code)
	}
	if code := accept(); code == http.StatusSeeOther {
		t.Fatal("the same invitation was accepted twice")
	}
}

// TestInvitationsDieWithTheirInviter closes a hole found by sub-project B's
// adversarial review: an admin who is removed from a team left behind live
// invitations for any address they controlled — a self-service way back in.
func TestInvitationsDieWithTheirInviter(t *testing.T) {
	h := newLiveHarness(t, "web-inv-inviter")
	ctx := context.Background()

	owner := h.user(t, "owner-inv-inv@web.test")
	h.team(t, "web-inv-inviter", "Team", owner, "")

	admin := h.user(t, "admin-inv-inv@web.test")
	if err := h.accounts.SetRole(ctx, owner.ID, "web-inv-inviter", admin.ID, account.RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	adminCookie := sessionCookie(h.signIn(t, "admin-inv-inv@web.test"))

	before := len(h.mailer.messages())
	if rec := h.invite(t, adminCookie, "accomplice@web.test", "member"); rec.Code != http.StatusSeeOther {
		t.Fatalf("admin invite = %d, want 303", rec.Code)
	}
	token := invitationToken(t, h.lastMail(t, before))

	// The admin leaves.
	if err := h.accounts.RemoveMember(ctx, owner.ID, "web-inv-inviter", admin.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	// Their outstanding invitation must have gone with them.
	h.user(t, "accomplice@web.test")
	accompliceCookie := sessionCookie(h.signIn(t, "accomplice@web.test"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/invitations/"+token, nil)
	req.AddCookie(accompliceCookie)
	h.handler.ServeHTTP(rec, req)

	accomplice, _ := h.accounts.EnsureUser(ctx, "accomplice@web.test", "")
	if _, err := h.accounts.RoleIn(ctx, accomplice.ID, "web-inv-inviter"); !errors.Is(err, account.ErrNotAMember) {
		t.Fatal("an invitation issued by a since-removed admin was still redeemable")
	}
}
