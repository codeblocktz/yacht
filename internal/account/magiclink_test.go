package account

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// A sign-in form that answers differently for a registered address is a
// user-list disclosure. Nothing observable may differ but the flag the caller
// uses to decide whether to send mail.
func TestIssueMagicLinkDoesNotRevealExistence(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	if _, err := s.EnsureUser(ctx, "known@example.test", "Known"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	raw, user, existed, err := s.IssueMagicLink(ctx, "known@example.test", time.Minute)
	if err != nil {
		t.Fatalf("IssueMagicLink for a known address: %v", err)
	}
	if !existed || raw == "" || user.Email != "known@example.test" {
		t.Fatalf("known address: existed=%v raw=%q user=%+v", existed, raw, user)
	}

	// The same call for an address nobody has must succeed just as quietly.
	_, _, existed, err = s.IssueMagicLink(ctx, "nobody@example.test", time.Minute)
	if err != nil {
		t.Fatalf("IssueMagicLink for an unknown address: %v", err)
	}
	if existed {
		t.Fatal("an unknown address reported as existing")
	}

	// And it must not have created the account it was asked about: a stranger
	// filling in the sign-in form would otherwise populate the user table.
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE lower(email) = 'nobody@example.test'`).Scan(&n); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if n != 0 {
		t.Fatal("issuing a link for an unknown address created the user")
	}
}

// A link in a mail archive is a permanent key otherwise.
func TestMagicLinkIsSingleUse(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "single@example.test", "Single")
	raw, _, _, err := s.IssueMagicLink(ctx, "single@example.test", time.Minute)
	if err != nil {
		t.Fatalf("IssueMagicLink: %v", err)
	}

	got, err := s.ConsumeMagicLink(ctx, raw)
	if err != nil {
		t.Fatalf("ConsumeMagicLink: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("consumed link resolved to %s, want %s", got.ID, u.ID)
	}

	if _, err := s.ConsumeMagicLink(ctx, raw); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("second use: want ErrTokenInvalid, got %v", err)
	}
}

func TestExpiredMagicLinkIsRejected(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	if _, err := s.EnsureUser(ctx, "stale@example.test", "Stale"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	raw, _, _, err := s.IssueMagicLink(ctx, "stale@example.test", -time.Minute)
	if err != nil {
		t.Fatalf("IssueMagicLink: %v", err)
	}

	if _, err := s.ConsumeMagicLink(ctx, raw); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("want ErrTokenInvalid for an expired link, got %v", err)
	}
}

// An unknown token must fail the same way an expired one does.
func TestUnknownMagicLinkIsRejected(t *testing.T) {
	s := testService(t)
	if _, err := s.ConsumeMagicLink(context.Background(), "not-a-real-token"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("want ErrTokenInvalid, got %v", err)
	}
}

// ------------------------------------------------------- first-user bootstrap

// The spec's third lockout guard. An install that switches accounts on has apps
// already deployed under YACHT_OWNER_ID; without this they belong to a team
// nobody can sign in to.
func TestBootstrapOwnerClaimsTheConfiguredTeam(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	// The team the install has been running as: the row exists, and no person
	// holds any role in it.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO teams (id, display_name) VALUES ('team-bootstrap', 'Local')`); err != nil {
		t.Fatalf("seed the configured team: %v", err)
	}

	first, err := s.EnsureUser(ctx, "first@example.test", "First")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if err := s.BootstrapOwner(ctx, "team-bootstrap", "Local", first); err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}

	role, err := s.RoleIn(ctx, first.ID, "team-bootstrap")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("role = %q, want owner", role)
	}

	teams, err := s.TeamsFor(ctx, first.ID)
	if err != nil {
		t.Fatalf("TeamsFor: %v", err)
	}
	if len(teams) != 1 || teams[0].TeamID != "team-bootstrap" {
		t.Fatalf("teams = %+v, want only the configured one — a fresh team leaves "+
			"the apps already deployed under an owner nobody can sign in as", teams)
	}
}

// A first install has no team row at all yet, and the first person to sign in
// still has to land somewhere.
func TestBootstrapOwnerCreatesTheConfiguredTeamWhenItIsMissing(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, err := s.EnsureUser(ctx, "fresh@example.test", "Fresh")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if err := s.BootstrapOwner(ctx, "team-fresh", "Local", u); err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}

	role, err := s.RoleIn(ctx, u.ID, "team-fresh")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("role = %q, want owner", role)
	}
}

// ...and only the first person. The second must not be handed ownership of the
// existing team just for signing in.
func TestBootstrapOwnerLeavesAnOwnedTeamAlone(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	held, _ := s.EnsureUser(ctx, "held@example.test", "Held")
	if _, err := s.CreateTeam(ctx, "team-held", "Held", held.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	second, _ := s.EnsureUser(ctx, "second@example.test", "Second")
	if err := s.BootstrapOwner(ctx, "team-held", "Held", second); err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}

	if _, err := s.RoleIn(ctx, second.ID, "team-held"); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("the second person to sign in joined a team they were never invited to: %v", err)
	}
	if n := ownersOf(t, s, "team-held"); n != 1 {
		t.Fatalf("owners of the configured team = %d, want 1", n)
	}

	// They are not left with nowhere to act, though: they own a team of their
	// own, and it is not the one that was taken.
	teams, err := s.TeamsFor(ctx, second.ID)
	if err != nil {
		t.Fatalf("TeamsFor: %v", err)
	}
	if len(teams) != 1 {
		t.Fatalf("teams = %+v, want exactly one", teams)
	}
	if teams[0].TeamID == "team-held" || teams[0].Role != RoleOwner {
		t.Fatalf("team = %+v, want ownership of a team of their own", teams[0])
	}
}

// Two people completing sign-in at the same moment must produce exactly one
// owner. Counting the owners and then writing one, with nothing holding the
// team row in between, produces two — and an unexpected second owner of the
// install's own team is a silent grant of everything it holds.
//
// Both states an install can be in are raced, because they are protected by
// different things: an existing team row is held by the FOR UPDATE, and a team
// that does not exist yet is held by the unique index the creators queue on.
func TestBootstrapOwnerIsRaceSafe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		teamID string
		seed   bool
	}{
		{"the install already had a team", "team-race-existing", true},
		{"a first install, with no team row yet", "team-race-fresh", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(t)
			ctx := context.Background()

			if tc.seed {
				if _, err := s.pool.Exec(ctx,
					`INSERT INTO teams (id, display_name) VALUES ($1, 'Local')`,
					tc.teamID); err != nil {
					t.Fatalf("seed the configured team: %v", err)
				}
			}

			const racers = 32
			users := make([]User, racers)
			for i := range users {
				u, err := s.EnsureUser(ctx, fmt.Sprintf("race-%d@example.test", i), "Racer")
				if err != nil {
					t.Fatalf("EnsureUser: %v", err)
				}
				users[i] = u
			}
			warmPool(t, s)

			errs := make([]error, racers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range users {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					errs[i] = s.BootstrapOwner(ctx, tc.teamID, "Race", users[i])
				}()
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("BootstrapOwner for racer %d: %v", i, err)
				}
			}
			if n := ownersOf(t, s, tc.teamID); n != 1 {
				t.Fatalf("owners of the configured team = %d, want exactly 1", n)
			}

			// Everyone else got a team of their own, and nobody got two.
			claimants := 0
			for i, u := range users {
				teams, err := s.TeamsFor(ctx, u.ID)
				if err != nil {
					t.Fatalf("TeamsFor: %v", err)
				}
				if len(teams) != 1 {
					t.Fatalf("racer %d holds %d teams, want 1: %+v", i, len(teams), teams)
				}
				if teams[0].Role != RoleOwner {
					t.Fatalf("racer %d = %q of %q, want owner of something",
						i, teams[0].Role, teams[0].TeamID)
				}
				if teams[0].TeamID == tc.teamID {
					claimants++
				}
			}
			if claimants != 1 {
				t.Fatalf("%d racers ended up in the configured team, want 1", claimants)
			}
		})
	}
}

// A retried sign-in must not hand the same person a second team.
func TestBootstrapOwnerIsIdempotent(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "again@example.test", "Again")
	for i := range 3 {
		if err := s.BootstrapOwner(ctx, "team-again", "Local", u); err != nil {
			t.Fatalf("BootstrapOwner call %d: %v", i+1, err)
		}
	}

	teams, err := s.TeamsFor(ctx, u.ID)
	if err != nil {
		t.Fatalf("TeamsFor: %v", err)
	}
	if len(teams) != 1 || teams[0].TeamID != "team-again" {
		t.Fatalf("teams = %+v, want the configured one and nothing else", teams)
	}
	if n := ownersOf(t, s, "team-again"); n != 1 {
		t.Fatalf("owners = %d, want 1", n)
	}
}

// warmPool opens every connection the pool will lend out before the race runs.
//
// Without it the first racer wins by default: the pool starts with a single
// connection and builds the rest one at a time, which takes longer than a whole
// transaction, so the others queue and read a table that is already settled.
// Measured with the lock removed, a cold pool produced one owner — the right
// answer for the wrong reason — and a warm one produced ten.
func warmPool(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()

	var wg sync.WaitGroup
	for range s.pool.Config().MaxConns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Held long enough that the pool cannot satisfy the next caller by
			// handing back the same connection.
			if _, err := s.pool.Exec(ctx, `SELECT pg_sleep(0.05)`); err != nil {
				t.Errorf("warm pool: %v", err)
			}
		}()
	}
	wg.Wait()
}

func ownersOf(t *testing.T, s *Service, teamID string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM memberships WHERE owner_id = $1 AND role = 'owner'`,
		teamID).Scan(&n); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	return n
}

// The database must not hold anything that can be mailed to a person.
func TestMagicLinkTokenIsStoredAsAHash(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	if _, err := s.EnsureUser(ctx, "linkhash@example.test", "H"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	raw, _, _, err := s.IssueMagicLink(ctx, "linkhash@example.test", time.Minute)
	if err != nil {
		t.Fatalf("IssueMagicLink: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM magic_links WHERE encode(token_hash, 'escape') = $1`,
		raw).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatal("the raw magic-link token is present in the database")
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM magic_links WHERE token_hash = $1`,
		HashToken(raw)).Scan(&n); err != nil {
		t.Fatalf("query by hash: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows stored under the hash = %d, want 1", n)
	}
}
