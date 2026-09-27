package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// actingFixture is an operator signed in to their own team, and a customer
// team they are not a member of.
type actingFixture struct {
	s        *Service
	operator User
	raw      string
	session  Session
}

func newActingFixture(t *testing.T, prefix string) actingFixture {
	t.Helper()
	s := testService(t)
	ctx := context.Background()

	op, _ := s.EnsureUser(ctx, prefix+"-op@example.test", "Op")
	if _, err := s.CreateTeam(ctx, "team-"+prefix+"-home", "Home", op.ID); err != nil {
		t.Fatalf("CreateTeam home: %v", err)
	}
	cust, _ := s.EnsureUser(ctx, prefix+"-cust@example.test", "Cust")
	if _, err := s.CreateTeam(ctx, "team-"+prefix+"-cust", "Customer Co", cust.ID); err != nil {
		t.Fatalf("CreateTeam customer: %v", err)
	}
	raw, err := s.CreateSession(ctx, op.ID, "team-"+prefix+"-home", "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := s.ResolveSession(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	return actingFixture{s: s, operator: op, raw: raw, session: sess}
}

func (f actingFixture) resolve(t *testing.T, p *Sessions) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: f.raw})
	owner, err := p.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return owner.ID
}

func (f actingFixture) actions(t *testing.T, team string) []string {
	t.Helper()
	evs, err := f.s.ImpersonationEvents(context.Background(), team, 10)
	if err != nil {
		t.Fatalf("ImpersonationEvents: %v", err)
	}
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].OperatorEmail != f.operator.Email || evs[i].UserID != f.operator.ID {
			t.Errorf("event names %s (%s), want the operator", evs[i].OperatorEmail, evs[i].UserID)
		}
		out = append(out, evs[i].Action)
	}
	return out
}

// The check is asked on every request, not once when acting starts: the
// request after it says no is back in the operator's own team, and the acting
// is gone rather than hidden — saying yes again does not restore it.
func TestActingIsReCheckedOnEveryRequest(t *testing.T) {
	f := newActingFixture(t, "recheck")
	ctx := context.Background()

	var allowed atomic.Bool
	allowed.Store(true)
	p := f.s.Provider("").WithActingCheck(func(_ context.Context, u User) bool {
		return u.ID == f.operator.ID && allowed.Load()
	})

	team, err := f.s.StartActing(ctx, f.session.ID, "team-recheck-cust")
	if err != nil {
		t.Fatalf("StartActing: %v", err)
	}
	if team.DisplayName != "Customer Co" {
		t.Errorf("StartActing returned team %+v", team)
	}
	if got := f.resolve(t, p); got != "team-recheck-cust" {
		t.Fatalf("acting resolves to %q, want the customer's team", got)
	}
	sess, _ := f.s.ResolveSession(ctx, f.raw)
	if sess.ActiveTeamID != "team-recheck-home" || sess.Role != RoleOwner {
		t.Errorf("acting changed the operator's own team or role: %+v", sess)
	}

	allowed.Store(false)
	if got := f.resolve(t, p); got != "team-recheck-home" {
		t.Fatalf("with the check refusing, resolved to %q, want the operator's own team", got)
	}
	allowed.Store(true)
	if got := f.resolve(t, p); got != "team-recheck-home" {
		t.Errorf("the check approving again put them back in %q — the acting was hidden, not ended", got)
	}
	if got := f.actions(t, "team-recheck-cust"); len(got) != 2 || got[0] != "start" || got[1] != "stop" {
		t.Errorf("recorded %v, want [start stop]", got)
	}
}

// A provider built without a check lets nobody act. A wrapping application
// building on these sessions must not inherit a way into every team by
// forgetting to wire one.
func TestAProviderWithoutACheckLetsNobodyAct(t *testing.T) {
	f := newActingFixture(t, "nocheck")
	if _, err := f.s.StartActing(context.Background(), f.session.ID, "team-nocheck-cust"); err != nil {
		t.Fatalf("StartActing: %v", err)
	}
	if got := f.resolve(t, f.s.Provider("")); got != "team-nocheck-home" {
		t.Errorf("a provider with no acting check resolved to %q", got)
	}
	sess, _ := f.s.ResolveSession(context.Background(), f.raw)
	if sess.ActingTeamID != "" {
		t.Errorf("the refused acting was left on the session: %q", sess.ActingTeamID)
	}
}

func TestSwitchingTeamStopsActing(t *testing.T) {
	f := newActingFixture(t, "switch")
	ctx := context.Background()
	if _, err := f.s.StartActing(ctx, f.session.ID, "team-switch-cust"); err != nil {
		t.Fatalf("StartActing: %v", err)
	}
	if err := f.s.SwitchTeam(ctx, f.session.ID, "team-switch-home"); err != nil {
		t.Fatalf("SwitchTeam: %v", err)
	}
	sess, _ := f.s.ResolveSession(ctx, f.raw)
	if sess.ActingTeamID != "" {
		t.Errorf("switching team left the session acting as %q", sess.ActingTeamID)
	}
	if got := f.actions(t, "team-switch-cust"); len(got) != 2 || got[1] != "stop" {
		t.Errorf("recorded %v, want [start stop]", got)
	}

	// A switch the membership check refuses changes nothing — including not
	// ending acting on the say-so of a request that was turned away.
	if _, err := f.s.StartActing(ctx, f.session.ID, "team-switch-cust"); err != nil {
		t.Fatalf("StartActing again: %v", err)
	}
	if err := f.s.SwitchTeam(ctx, f.session.ID, "team-switch-cust"); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("switching into a team the operator is not in = %v, want ErrNotAMember", err)
	}
	sess, _ = f.s.ResolveSession(ctx, f.raw)
	if sess.ActingTeamID != "team-switch-cust" {
		t.Errorf("a refused switch ended the acting: %q", sess.ActingTeamID)
	}
}

func TestSigningOutRecordsTheStop(t *testing.T) {
	f := newActingFixture(t, "signout")
	ctx := context.Background()
	if _, err := f.s.StartActing(ctx, f.session.ID, "team-signout-cust"); err != nil {
		t.Fatalf("StartActing: %v", err)
	}
	if err := f.s.RevokeSession(ctx, f.raw); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if got := f.actions(t, "team-signout-cust"); len(got) != 2 || got[1] != "stop" {
		t.Errorf("recorded %v, want [start stop]", got)
	}
}

// Moving straight from one team to another records the first one's end,
// rather than leaving an impersonation in the record that never stopped.
func TestActingAsAnotherTeamStopsTheFirst(t *testing.T) {
	f := newActingFixture(t, "move")
	ctx := context.Background()
	second, _ := f.s.EnsureUser(ctx, "move-second@example.test", "")
	if _, err := f.s.CreateTeam(ctx, "team-move-second", "Second", second.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	for _, team := range []string{"team-move-cust", "team-move-cust", "team-move-second"} {
		if _, err := f.s.StartActing(ctx, f.session.ID, team); err != nil {
			t.Fatalf("StartActing %s: %v", team, err)
		}
	}
	if got := f.actions(t, "team-move-cust"); len(got) != 2 || got[0] != "start" || got[1] != "stop" {
		t.Errorf("first team recorded %v, want [start stop] — asking twice for the same team is one start", got)
	}
	if got := f.actions(t, "team-move-second"); len(got) != 1 || got[0] != "start" {
		t.Errorf("second team recorded %v, want [start]", got)
	}
}

func TestActingAsATeamThatDoesNotExist(t *testing.T) {
	f := newActingFixture(t, "missing")
	if _, err := f.s.StartActing(context.Background(), f.session.ID, "team-missing-nobody"); !errors.Is(err, ErrNoSuchTeam) {
		t.Fatalf("StartActing an unknown team = %v, want ErrNoSuchTeam", err)
	}
}
