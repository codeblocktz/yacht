package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/identity"
)

func TestSessionRoundTrip(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "session@example.test", "S")
	if _, err := s.CreateTeam(ctx, "team-session", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	raw, err := s.CreateSession(ctx, u.ID, "team-session", "test-agent", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.ResolveSession(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if got.UserID != u.ID || got.ActiveTeamID != "team-session" {
		t.Fatalf("session = %+v, want user %s in team-session", got, u.ID)
	}
}

// The database must not hold anything that can be replayed.
func TestSessionTokenIsStoredAsAHash(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "hash@example.test", "H")
	_, _ = s.CreateTeam(ctx, "team-hash", "Team", u.ID)
	raw, err := s.CreateSession(ctx, u.ID, "team-hash", "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE encode(token_hash, 'escape') = $1`,
		raw).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatal("the raw session token is present in the database")
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "expired@example.test", "E")
	_, _ = s.CreateTeam(ctx, "team-expired", "Team", u.ID)
	raw, _ := s.CreateSession(ctx, u.ID, "team-expired", "", "", -time.Minute)

	if _, err := s.ResolveSession(ctx, raw); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("want ErrSessionInvalid for an expired session, got %v", err)
	}
}

func TestUnknownSessionIsRejected(t *testing.T) {
	s := testService(t)
	if _, err := s.ResolveSession(context.Background(), "not-a-real-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("want ErrSessionInvalid, got %v", err)
	}
}

// Without this a leaked session cannot be revoked.
func TestRevokeAllSessionsSignsOutEverywhere(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "everywhere@example.test", "E")
	_, _ = s.CreateTeam(ctx, "team-everywhere", "Team", u.ID)

	first, _ := s.CreateSession(ctx, u.ID, "team-everywhere", "", "", time.Hour)
	second, _ := s.CreateSession(ctx, u.ID, "team-everywhere", "", "", time.Hour)

	if err := s.RevokeAllSessions(ctx, u.ID); err != nil {
		t.Fatalf("RevokeAllSessions: %v", err)
	}
	for name, raw := range map[string]string{"first": first, "second": second} {
		if _, err := s.ResolveSession(ctx, raw); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("%s session still resolves after sign-out everywhere", name)
		}
	}
}

// Signing out of one browser must not sign the person out of the others.
func TestRevokeSessionEndsOnlyThatSession(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "one@example.test", "O")
	_, _ = s.CreateTeam(ctx, "team-one", "Team", u.ID)

	here, _ := s.CreateSession(ctx, u.ID, "team-one", "", "", time.Hour)
	elsewhere, _ := s.CreateSession(ctx, u.ID, "team-one", "", "", time.Hour)

	if err := s.RevokeSession(ctx, here); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := s.ResolveSession(ctx, here); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("the revoked session still resolves")
	}
	if _, err := s.ResolveSession(ctx, elsewhere); err != nil {
		t.Errorf("the other session was signed out too: %v", err)
	}
}

// A crafted switch would otherwise put a stranger's team on a session, and
// every query downstream is scoped by exactly that value.
func TestSwitchTeamRefusesATeamYouAreNotIn(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	mine, _ := s.EnsureUser(ctx, "switcher@example.test", "S")
	theirs, _ := s.EnsureUser(ctx, "bystander@example.test", "B")
	_, _ = s.CreateTeam(ctx, "team-mine", "Mine", mine.ID)
	_, _ = s.CreateTeam(ctx, "team-theirs", "Theirs", theirs.ID)

	raw, _ := s.CreateSession(ctx, mine.ID, "team-mine", "", "", time.Hour)
	sess, err := s.ResolveSession(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}

	if err := s.SwitchTeam(ctx, sess.ID, "team-theirs"); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("want ErrNotAMember, got %v", err)
	}
	after, err := s.ResolveSession(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveSession after the refusal: %v", err)
	}
	if after.ActiveTeamID != "team-mine" {
		t.Fatalf("active team = %q, the refused switch took effect", after.ActiveTeamID)
	}

	if _, err := s.CreateTeam(ctx, "team-second", "Second", mine.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := s.SwitchTeam(ctx, sess.ID, "team-second"); err != nil {
		t.Fatalf("switching to a team you own: %v", err)
	}
	switched, err := s.ResolveSession(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveSession after the switch: %v", err)
	}
	if switched.ActiveTeamID != "team-second" {
		t.Fatalf("active team = %q, want team-second", switched.ActiveTeamID)
	}
}

// The provider is what lets every existing handler stay untouched.
func TestSessionsImplementsIdentityProvider(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "provider@example.test", "P")
	_, _ = s.CreateTeam(ctx, "team-provider", "Team", u.ID)
	raw, _ := s.CreateSession(ctx, u.ID, "team-provider", "", "", time.Hour)

	var p identity.Provider = s.Provider("yacht_session")

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "yacht_session", Value: raw})

	owner, err := p.Resolve(ctx, r)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The owner is the active TEAM, not the user. Everything downstream is
	// scoped by owner_id, and owner_id means team.
	if owner.ID != "team-provider" {
		t.Fatalf("owner.ID = %q, want the active team", owner.ID)
	}

	bare := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := p.Resolve(ctx, bare); err == nil {
		t.Fatal("Resolve accepted a request with no cookie")
	}
}
