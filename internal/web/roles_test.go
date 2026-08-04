package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// rolelessAccounts resolves any cookie to a session that is signed in and holds
// no role at all.
//
// That is what makes the walk below a real test rather than a reading of the
// router: against this service every gated route must answer 403, whatever its
// minimum, and any route that answers something else ran its handler — which is
// precisely what "no role gate" means.
type rolelessAccounts struct {
	*fakeAccounts
	team string
}

func (a *rolelessAccounts) ResolveSession(context.Context, string) (account.Session, error) {
	return account.Session{
		ID: uuid.New(), UserID: uuid.New(), ActiveTeamID: a.team, Role: account.Role(""),
	}, nil
}

// gatedServer builds the server with its whole route table present — the
// account surface included, because the routes a role gate exists for are the
// ones that only appear when accounts are on.
func gatedServer(t *testing.T, accounts Accounts, team string) http.Handler {
	t.Helper()
	s, err := New(Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            newFakeApps(sampleApp(team, "probe")),
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        accounts,
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		// Present so the join routes are on the table the walk below covers.
		// A route only reachable in some configurations is exactly the one
		// nobody remembers to gate.
		Joiner: &fakeJoiner{configured: true},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

// TestEveryMutatingRouteHasARoleGate walks the router and fails if a POST route
// is reachable without a role requirement.
//
// A route added next year inherits the gate only if the gate is on the group —
// this is what proves it still is. The route table is enumerated rather than
// listed, so a route nobody thought to write down here is still checked.
func TestEveryMutatingRouteHasARoleGate(t *testing.T) {
	const team = "web-roles"
	h := gatedServer(t, &rolelessAccounts{fakeAccounts: &fakeAccounts{}, team: team}, team)

	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatal("the handler cannot be walked, so nothing here proves anything")
	}

	// Every entry is a route that cannot have a role, with the reason it cannot.
	allowed := map[string]string{
		"/sign-in": "there is no session to read a role from yet — someone with " +
			"no session is exactly who needs this form",
		"/sign-in/password": "the same: this is how a session begins, so there " +
			"is none to read a role from. It is a separate path from /sign-in " +
			"because the CSRF origin check exempts that one by name, and a " +
			"password must not travel under that exemption",
		"/sign-out": "a browser whose session has already been revoked must still " +
			"be able to clear its cookie; a gate here would deny it exactly that",
		"/sign-out-everywhere": "the same, and it acts only on the cookie " +
			"presented, so it grants nothing a role could withhold",
	}

	// A signed-in browser holding no role in the team it is acting as. Whatever
	// a gated route asks for, this is below it.
	probe := func(route string) *httptest.ResponseRecorder {
		path := pathParam.ReplaceAllString(route, "probe")
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "probe-session"})
		req.Header.Set("Origin", "https://yacht.test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	seen := map[string]bool{}
	var ungated, posts []string

	err := chi.Walk(routes, func(
		method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler,
	) error {
		if method != http.MethodPost {
			return nil
		}
		posts = append(posts, route)

		if _, ok := allowed[route]; ok {
			seen[route] = true
			// An allowlisted route has to work without a role, or it is not an
			// exception to the rule — it is a route that is broken quietly.
			if code := probe(route).Code; code == http.StatusForbidden {
				t.Errorf("POST %s is allowlisted as needing no role and answered 403", route)
			}
			return nil
		}
		if code := probe(route).Code; code != http.StatusForbidden {
			ungated = append(ungated, fmt.Sprintf("%s (answered %d)", route, code))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(ungated) > 0 {
		t.Errorf("POST routes a roleless session could reach: %v\n"+
			"the gate belongs on the group, so that a route added later is covered "+
			"whether or not its author thought about it", ungated)
	}

	// A walk that found nothing would pass every assertion above while proving
	// nothing at all.
	if len(posts) < 8 {
		t.Fatalf("walked %d POST routes (%v) — the table cannot be that small, "+
			"so the walk is not seeing the router", len(posts), posts)
	}
	for route, why := range allowed {
		if !seen[route] {
			t.Errorf("allowlisted %q (%s) is not a POST route any more — an "+
				"allowlist that outlives its route hides the next ungated one", route, why)
		}
	}
}

// pathParam matches chi's {name} and * placeholders, so a walked pattern can be
// turned back into a request.
var pathParam = regexp.MustCompile(`\{[^}]*\}|\*`)

// A gate that only ever answered 403 would pass the walk above while making the
// dashboard unusable, so the same probe with a role that is high enough has to
// get through.
func TestTheGateLetsAnOwnerThrough(t *testing.T) {
	const team = "web-roles-open"
	accounts := &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: account.RoleOwner}
	h := gatedServer(t, accounts, team)

	req := httptest.NewRequest(http.MethodPost, "/apps/probe/redeploy", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "probe-session"})
	req.Header.Set("Origin", "https://yacht.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /apps/probe/redeploy as an owner = %d, want 303", rec.Code)
	}
}

type roledAccounts struct {
	*fakeAccounts
	team string
	role account.Role
}

func (a *roledAccounts) ResolveSession(context.Context, string) (account.Session, error) {
	return account.Session{
		ID: uuid.New(), UserID: uuid.New(), ActiveTeamID: a.team, Role: a.role,
	}, nil
}

// ------------------------------------------------------- the gates in action
//
// Against the real account service and a real database: what is being claimed
// is that a role in a team decides what a request may do, and a role only
// exists because a membership row does.

// roleTeam is one team with a person in it at each role, each holding a real
// session cookie.
type roleTeam struct {
	*liveHarness
	appName string

	owner, admin, member       *http.Cookie
	ownerID, adminID, memberID uuid.UUID
}

func newRoleTeam(t *testing.T, teamID, appName string) *roleTeam {
	t.Helper()
	ctx := context.Background()

	h := newLiveHarness(t, teamID)
	h.installedApp(t, appName)

	rt := &roleTeam{liveHarness: h, appName: appName}

	// The first person in inherits the configured team, which is how a real
	// install acquires its first owner.
	owner := h.user(t, "owner@web.test")
	rt.ownerID = owner.ID
	rt.owner = sessionCookie(h.signIn(t, "owner@web.test"))
	if rt.owner == nil {
		t.Fatal("the first person to sign in got no session")
	}
	if role, err := h.accounts.RoleIn(ctx, owner.ID, teamID); err != nil || role != account.RoleOwner {
		t.Fatalf("the first sign-in produced role %q (%v), want owner — the rest "+
			"of this test would be measuring the wrong thing", role, err)
	}

	for _, role := range []account.Role{account.RoleAdmin, account.RoleMember} {
		email := string(role) + "@web.test"
		u := h.user(t, email)
		if err := h.accounts.SetRole(ctx, owner.ID, teamID, u.ID, role); err != nil {
			t.Fatalf("SetRole %s: %v", role, err)
		}
		c := sessionCookie(h.signIn(t, email))
		if c == nil {
			t.Fatalf("the %s got no session", role)
		}
		if role == account.RoleAdmin {
			rt.admin, rt.adminID = c, u.ID
		} else {
			rt.member, rt.memberID = c, u.ID
		}
	}
	return rt
}

func (rt *roleTeam) appExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := rt.apps.Get(context.Background(), rt.teamID, name)
	if err == nil {
		return true
	}
	if errors.Is(err, app.ErrNotFound) {
		return false
	}
	t.Fatalf("get app: %v", err)
	return false
}

// The split is at the line where an action stops being undoable by
// redeploying: a member deploys and scales, and cannot delete.
func TestMemberCanDeployButNotDelete(t *testing.T) {
	rt := newRoleTeam(t, "web-role-member", "member-app")

	for _, path := range []string{
		"/apps/" + rt.appName + "/redeploy",
		"/apps/" + rt.appName + "/scale",
	} {
		if code := rt.postAs(t, path, rt.member).Code; code != http.StatusSeeOther {
			t.Errorf("POST %s as a member = %d, want 303 — a member who cannot "+
				"deploy has no role left", path, code)
		}
	}

	del := "/apps/" + rt.appName + "/delete"
	if code := rt.postAs(t, del, rt.member).Code; code != http.StatusForbidden {
		t.Errorf("POST %s as a member = %d, want 403", del, code)
	}
	// The refusal is not cosmetic: the app is still there afterwards.
	if !rt.appExists(t, rt.appName) {
		t.Fatal("the member deleted the app the gate said they could not")
	}
}

// Admin: everything a redeploy cannot undo, and the people who can do it.
func TestAdminCanDeleteAppsAndInvite(t *testing.T) {
	rt := newRoleTeam(t, "web-role-admin", "admin-app")

	del := "/apps/" + rt.appName + "/delete"
	// With the name typed back — deleting checks it on the server, so a
	// request without it proves nothing about what an admin may do.
	confirm := url.Values{"confirm": {rt.appName}}
	if code := rt.postFormAs(t, del, rt.admin, confirm).Code; code != http.StatusSeeOther {
		t.Fatalf("POST %s as an admin = %d, want 303", del, code)
	}
	if rt.appExists(t, rt.appName) {
		t.Error("the admin's delete answered 303 and deleted nothing")
	}

	// Inviting, revoking an invitation and removing a member are the same
	// authority: they decide who is in the team at all.
	for _, path := range []string{
		"/team/invite",
		"/team/invitations/" + uuid.New().String() + "/revoke",
		"/team/members/" + rt.memberID.String() + "/remove",
	} {
		if code := rt.postAs(t, path, rt.member).Code; code != http.StatusForbidden {
			t.Errorf("POST %s as a member = %d, want 403 — a member must not be "+
				"able to change who is in the team", path, code)
		}
		if code := rt.postAs(t, path, rt.admin).Code; code == http.StatusForbidden {
			t.Errorf("POST %s as an admin = 403 — this is what an admin is for", path)
		}
	}
}

// Touching anyone who can administer is ownership: an admin allowed to appoint
// an admin could appoint an owner next, which is every permission the team has.
func TestOnlyOwnerCanManageAdminsOrDeleteTheTeam(t *testing.T) {
	rt := newRoleTeam(t, "web-role-owner", "owner-app")

	// /team/delete is deliberately absent from the router rather than stubbed —
	// it cascades to every app row while leaving the workloads running — so
	// there is no route here to gate.
	for _, path := range []string{
		"/team/members/" + rt.adminID.String() + "/role",
	} {
		for name, c := range map[string]*http.Cookie{
			"a member": rt.member, "an admin": rt.admin,
		} {
			if code := rt.postAs(t, path, c).Code; code != http.StatusForbidden {
				t.Errorf("POST %s as %s = %d, want 403", path, name, code)
			}
		}
		if code := rt.postAs(t, path, rt.owner).Code; code == http.StatusForbidden {
			t.Errorf("POST %s as the owner = 403 — the owner is who this is for", path)
		}
	}
}

// The gate must read the role from the session, never from a form field, a
// query parameter or a header the caller controls.
func TestRoleCannotBeAssertedByTheClient(t *testing.T) {
	rt := newRoleTeam(t, "web-role-claim", "claim-app")
	del := "/apps/" + rt.appName + "/delete"

	claims := url.Values{
		"role": {"owner"}, "Role": {"owner"}, "member_role": {"owner"},
		"team": {rt.teamID}, "user_id": {rt.ownerID.String()},
	}
	headers := map[string]string{
		"X-Role":             "owner",
		"X-Yacht-Role":       "owner",
		"X-Forwarded-Role":   "owner",
		"X-Yacht-Owner":      rt.teamID,
		"X-Yacht-User":       rt.ownerID.String(),
		"Authorization":      "Bearer owner",
		"X-Forwarded-Proto":  "https",
		"X-Yacht-Session-Id": rt.owner.Value,
	}

	rec := rt.postClaiming(t, del+"?role=owner&admin=true", rt.member, claims, headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST %s as a member claiming to be an owner = %d, want 403 — the "+
			"role came from something the caller sent", del, rec.Code)
	}
	if !rt.appExists(t, rt.appName) {
		t.Fatal("a member deleted an app by saying they were an owner")
	}

	// ...and asserting a role with no session at all buys nothing either.
	rec = rt.postClaiming(t, del, nil, claims, headers)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST %s with no session but every claim set = %d, want 401",
			del, rec.Code)
	}
	if !rt.appExists(t, rt.appName) {
		t.Fatal("an unauthenticated caller deleted an app by claiming a role")
	}
}

// postClaiming sends everything a caller could use to assert a role: form
// fields, query parameters and headers.
func (rt *roleTeam) postClaiming(
	t *testing.T, path string, c *http.Cookie, form url.Values, headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Origin", "https://yacht.test")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)
	return rec
}
