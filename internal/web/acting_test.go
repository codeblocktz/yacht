package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
)

// ------------------------------------------------- acting as a team, for real
//
// Against the real account service, the real session provider and a real
// database. What is being claimed is a property of all three together: that a
// column on a session row, a list of addresses in the configuration and the
// provider's check on every request add up to "only named operators act, and
// only while they are named".

// actingSetup is a role team whose admin is the install's only named operator,
// and a second team — a customer — with an app of its own for the operator to
// act on.
type actingSetup struct {
	*roleTeam
	custTeam, custApp string
	operator          *http.Cookie
}

const operatorEmail = "admin@web.test"

func newActingSetup(t *testing.T, prefix string, operators []string) *actingSetup {
	t.Helper()
	rt := newRoleTeamWith(t, prefix, prefix+"-own-app", operators)
	a := &actingSetup{
		roleTeam: rt, custTeam: prefix + "-cust", custApp: prefix + "-cust-app",
		operator: rt.admin,
	}
	ctx := context.Background()
	cust := rt.user(t, "cust-"+prefix+"@web.test")
	if _, err := rt.accounts.CreateTeam(ctx, a.custTeam, "Customer Co", cust.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := rt.apps.Create(ctx, a.custTeam, app.CreateInput{
		Name: a.custApp, Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("create the customer's app: %v", err)
	}
	// Settled for the reason installedApp gives: no worker runs here, and an
	// app still holding its first operation reads as a deploy in flight.
	if _, err := rt.pool.Exec(ctx, `
		UPDATE deployment_operations o
		SET status = 'succeeded', finished_at = now(),
		    claim_token = NULL, lease_expires_at = NULL
		FROM apps a
		WHERE o.app_id = a.id AND a.owner_id = $1
		  AND o.status IN ('queued', 'claimed', 'building', 'applying', 'verifying')`,
		a.custTeam); err != nil {
		t.Fatalf("settle the customer's operation: %v", err)
	}
	return a
}

func (a *actingSetup) act(t *testing.T) {
	t.Helper()
	rec := a.postAs(t, "/admin/teams/"+a.custTeam+"/act", a.operator)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("POST act as the named operator = %d → %q, want 303 → /\n%s",
			rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

// actingTeam is what the session row says, read directly: the state the
// provider and the gates are deciding about.
func (a *actingSetup) actingTeam(t *testing.T, c *http.Cookie) string {
	t.Helper()
	var team *string
	if err := a.pool.QueryRow(context.Background(),
		`SELECT acting_team_id FROM sessions WHERE token_hash = $1`,
		account.HashToken(c.Value)).Scan(&team); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if team == nil {
		return ""
	}
	return *team
}

// events is the durable record for the customer, oldest first.
func (a *actingSetup) events(t *testing.T) []string {
	t.Helper()
	rows, err := a.pool.Query(context.Background(), `
		SELECT operator_email || ' ' || action FROM impersonation_events
		WHERE owner_id = $1 ORDER BY at, action DESC`, a.custTeam)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// resolvedOwner is who the request resolves to, asked of the provider itself
// rather than read off a page.
func (a *actingSetup) resolvedOwner(t *testing.T, c *http.Cookie, operators []string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	p := a.accounts.Provider(SessionCookie).WithActingCheck(OperatorCheck(operators))
	owner, err := p.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return owner.ID
}

func TestANamedOperatorActsAsAnotherTeam(t *testing.T) {
	a := newActingSetup(t, "web-act-sees", []string{operatorEmail})

	before := a.getAs(t, "/", a.operator).Body.String()
	if strings.Contains(before, a.custApp) {
		t.Fatal("the operator sees the customer's app before acting — the test is measuring nothing")
	}

	a.act(t)

	home := a.getAs(t, "/", a.operator).Body.String()
	if !strings.Contains(home, a.custApp) {
		t.Error("acting as the customer, the overview does not show the customer's app")
	}
	if strings.Contains(home, a.appName) {
		t.Error("acting as the customer, the overview still shows the operator's own team's app")
	}
	// The Admin area stays: it is the person who is the operator, not the team.
	if !strings.Contains(home, `href="/admin/teams"`) {
		t.Error("acting as a team took the Admin area away from the operator")
	}

	if code := a.getAs(t, "/apps/"+a.custApp, a.operator).Code; code != http.StatusOK {
		t.Errorf("GET the customer's app while acting = %d, want 200", code)
	}
	if code := a.postAs(t, "/apps/"+a.custApp+"/redeploy", a.operator).Code; code != http.StatusSeeOther {
		t.Errorf("redeploy the customer's app while acting = %d, want 303", code)
	}
	// Owner, not member: support that cannot change settings fixes nothing.
	// Health is an admin's; it answering 403 would mean the role was not granted.
	rec := a.postFormAs(t, "/apps/"+a.custApp+"/health", a.operator, url.Values{"health_path": {"/healthz"}})
	if rec.Code == http.StatusForbidden {
		t.Errorf("an admin's action on the customer's app while acting = 403 — acting is the team's owner")
	}

	if got := a.resolvedOwner(t, a.operator, []string{operatorEmail}); got != a.custTeam {
		t.Errorf("the provider resolves the acting operator to %q, want %q", got, a.custTeam)
	}
	if got := a.events(t); len(got) != 1 || got[0] != operatorEmail+" start" {
		t.Errorf("recorded events = %v, want one start by %s", got, operatorEmail)
	}
}

// Only a named operator acts. A team's owner who is not one is refused, and on
// an install with nobody named — where every team owner is an operator — so is
// everybody, or every team owner could act as every other team.
func TestOnlyANamedOperatorMayAct(t *testing.T) {
	a := newActingSetup(t, "web-act-refused", []string{operatorEmail})
	for name, c := range map[string]*http.Cookie{"the team owner": a.owner, "a member": a.member} {
		if code := a.postAs(t, "/admin/teams/"+a.custTeam+"/act", c).Code; code != http.StatusForbidden {
			t.Errorf("POST act as %s, who is not a named operator = %d, want 403", name, code)
		}
		if got := a.actingTeam(t, c); got != "" {
			t.Errorf("%s was refused and is acting as %q anyway", name, got)
		}
	}

	none := newActingSetup(t, "web-act-unnamed", nil)
	rec := none.postAs(t, "/admin/teams/"+none.custTeam+"/act", none.owner)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST act as a team owner with no operators named = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "YACHT_OPERATORS") {
		t.Errorf("the refusal does not say what would make it work: %q", rec.Body.String())
	}
	if got := none.actingTeam(t, none.owner); got != "" {
		t.Errorf("with no operators named the owner is acting as %q", got)
	}
	if strings.Contains(none.getAs(t, "/", none.owner).Body.String(), none.custApp) {
		t.Error("with no operators named, a team owner reached another team's apps")
	}
	// A row planted by hand — an acting column from before the list was
	// emptied — is not permission either.
	if _, err := none.pool.Exec(context.Background(),
		`UPDATE sessions SET acting_team_id = $1 WHERE token_hash = $2`,
		none.custTeam, account.HashToken(none.owner.Value)); err != nil {
		t.Fatalf("plant acting: %v", err)
	}
	if strings.Contains(none.getAs(t, "/", none.owner).Body.String(), none.custApp) {
		t.Error("an acting column on the session was honoured with no operators named")
	}
	if got := none.actingTeam(t, none.owner); got != "" {
		t.Errorf("the refused acting was left on the session: %q", got)
	}
}

// Taking somebody off YACHT_OPERATORS ends their impersonation on their very
// next request: the provider re-asks, resolves them to their own team, and
// clears the acting — whether the list now names somebody else or nobody.
func TestRemovingAnOperatorEndsActingOnTheNextRequest(t *testing.T) {
	for name, after := range map[string][]string{
		"someone else named": {"someone-else@web.test"},
		"nobody named":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			prefix := "web-act-removed-a"
			if after == nil {
				prefix = "web-act-removed-b"
			}
			a := newActingSetup(t, prefix, []string{operatorEmail})
			a.act(t)

			restarted := a.restart(after)
			get := func(path string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.AddCookie(a.operator)
				rec := httptest.NewRecorder()
				restarted.ServeHTTP(rec, req)
				return rec
			}

			home := get("/")
			if home.Code != http.StatusOK {
				t.Fatalf("GET / after removal = %d, want 200 — they are still signed in to their own team", home.Code)
			}
			body := home.Body.String()
			if strings.Contains(body, a.custApp) {
				t.Error("taken off the operators, the person still sees the customer's app")
			}
			if !strings.Contains(body, a.appName) {
				t.Error("taken off the operators, the person is not back in their own team")
			}
			if strings.Contains(body, "data-acting-banner") {
				t.Error("the acting banner is still drawn after acting ended")
			}
			if got := a.actingTeam(t, a.operator); got != "" {
				t.Errorf("the session still says it is acting as %q", got)
			}
			if got := a.resolvedOwner(t, a.operator, after); got != a.teamID {
				t.Errorf("the provider resolves them to %q, want their own team %q", got, a.teamID)
			}
			// Put back on the list, they are not put back in the customer's
			// team: the acting ended, it was not merely hidden.
			if got := a.resolvedOwner(t, a.operator, []string{operatorEmail}); got != a.teamID {
				t.Errorf("restored to the list, the session resolves to %q — the acting was hidden rather than ended", got)
			}
			want := []string{operatorEmail + " start", operatorEmail + " stop"}
			if got := a.events(t); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("recorded events = %v, want %v", got, want)
			}
		})
	}
}

func TestStopActingPutsTheOperatorBack(t *testing.T) {
	a := newActingSetup(t, "web-act-stop", []string{operatorEmail})
	a.act(t)

	rec := a.postAs(t, "/acting/stop", a.operator)
	if want := "/admin/teams/" + a.custTeam; rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Fatalf("POST /acting/stop = %d → %q, want 303 → %s", rec.Code, rec.Header().Get("Location"), want)
	}
	if got := a.actingTeam(t, a.operator); got != "" {
		t.Errorf("stopped, the session still says it is acting as %q", got)
	}
	home := a.getAs(t, "/", a.operator).Body.String()
	if strings.Contains(home, a.custApp) || strings.Contains(home, "data-acting-banner") {
		t.Error("stopped, the operator is still shown the customer's team")
	}
	want := []string{operatorEmail + " start", operatorEmail + " stop"}
	if got := a.events(t); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("recorded events = %v, want %v", got, want)
	}

	// Stopping when not acting is harmless and records nothing more.
	if code := a.postAs(t, "/acting/stop", a.operator).Code; code != http.StatusSeeOther {
		t.Errorf("POST /acting/stop when not acting = %d, want 303", code)
	}
	if got := a.events(t); len(got) != 2 {
		t.Errorf("a stop with nothing to stop was recorded: %v", got)
	}

	// The team page shows the record.
	page := a.getAs(t, "/admin/teams/"+a.custTeam, a.operator).Body.String()
	for _, want := range []string{"Support access", "Started acting", "Stopped acting", operatorEmail, "Act as this team"} {
		if !strings.Contains(page, want) {
			t.Errorf("the team page does not show %q", want)
		}
	}
}

func TestSwitchingTeamEndsActing(t *testing.T) {
	a := newActingSetup(t, "web-act-switch", []string{operatorEmail})
	a.act(t)

	rec := a.postFormAs(t, "/teams/switch", a.operator, url.Values{"team": {a.teamID}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /teams/switch while acting = %d, want 303", rec.Code)
	}
	if got := a.actingTeam(t, a.operator); got != "" {
		t.Errorf("switched team, the session still says it is acting as %q", got)
	}
	if strings.Contains(a.getAs(t, "/", a.operator).Body.String(), a.custApp) {
		t.Error("switched to their own team, the operator is still shown the customer's")
	}
	want := []string{operatorEmail + " start", operatorEmail + " stop"}
	if got := a.events(t); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("recorded events = %v, want %v", got, want)
	}
}

func TestSigningOutEndsActingAndRecordsIt(t *testing.T) {
	a := newActingSetup(t, "web-act-signout", []string{operatorEmail})
	a.act(t)
	if code := a.postAs(t, "/sign-out", a.operator).Code; code != http.StatusSeeOther {
		t.Fatalf("POST /sign-out = %d, want 303", code)
	}
	want := []string{operatorEmail + " start", operatorEmail + " stop"}
	if got := a.events(t); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("recorded events = %v, want %v", got, want)
	}
}

// Every page says who is being acted as, with the way out beside it.
func TestTheActingBannerIsOnEveryPage(t *testing.T) {
	a := newActingSetup(t, "web-act-banner", []string{operatorEmail})

	if strings.Contains(a.getAs(t, "/", a.operator).Body.String(), "data-acting-banner") {
		t.Fatal("the acting banner is drawn before acting")
	}
	a.act(t)

	for _, path := range []string{
		"/", "/projects", "/deployments", "/settings", "/team", "/account",
		"/apps/" + a.custApp, "/admin/teams", "/admin/teams/" + a.custTeam,
		"/admin/capacity", "/cluster/nodes",
	} {
		rec := a.getAs(t, path, a.operator)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s while acting = %d, want 200", path, rec.Code)
			continue
		}
		body := rec.Body.String()
		if !strings.Contains(body, "data-acting-banner") || !strings.Contains(body, "You are acting as Customer Co") {
			t.Errorf("GET %s while acting has no acting banner", path)
		}
		if !strings.Contains(body, `action="/acting/stop"`) {
			t.Errorf("GET %s while acting has no way to stop", path)
		}
	}
	// The team page offers the operator no control over the customer's
	// people: they are its owner for its apps, not a member of it.
	if strings.Contains(a.getAs(t, "/team", a.operator).Body.String(), `action="/team/invite"`) {
		t.Error("acting as a team offers the operator its invitations")
	}
}

// A wrapping application that fills Banner with its own notice composes with
// the acting one; it cannot replace it.
func TestAWrappersBannerCannotHideActing(t *testing.T) {
	ctx := context.WithValue(context.Background(), surfacesKey{}, Surfaces{ActingAs: "Customer Co"})
	own := templ.Raw(`<p data-wrapper-banner>Your balance is low</p>`)

	var buf bytes.Buffer
	if err := Layout(Slots{Banner: own}, templ.Raw("page")).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, "data-wrapper-banner") {
		t.Error("the wrapper's own banner was dropped")
	}
	if !strings.Contains(body, "You are acting as Customer Co") {
		t.Error("a wrapper's banner hid the acting notice")
	}
	if strings.Index(body, "data-acting-banner") > strings.Index(body, "data-wrapper-banner") {
		t.Error("the acting notice is drawn below the wrapper's banner")
	}

	// And the engine's own banner is not drawn twice.
	buf.Reset()
	if err := Layout(Slots{Banner: ActingBanner("Customer Co")}, templ.Raw("page")).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if n := strings.Count(buf.String(), "data-acting-banner"); n != 1 {
		t.Errorf("the acting banner is drawn %d times, want once", n)
	}
}
