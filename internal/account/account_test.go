package account

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/codeblocktz/yacht/internal/store"
)

// testService migrates, connects and returns a Service against the test
// database, skipping when none is configured so `go test ./...` stays useful
// on a laptop without Postgres.
//
// Leftovers are purged before the test as well as after it, so a run that
// crashed halfway does not fail the next one on a unique constraint. The pool's
// Cleanup is registered first because cleanups run last-registered-first and
// the purge has to happen while the pool is still open.
func testService(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run account tests")
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

	// Memberships, sessions and invitations follow by cascade. The prefixes
	// keep this clear of the owner ids the store, app and domain packages use,
	// because `go test ./...` runs those packages against the same database at
	// the same time.
	purge := func() {
		// The team the bootstrap gives someone who could not have the configured
		// one is named after them, so it is found through the same address filter
		// — and has to go before the user it is derived from.
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id IN (
			SELECT 'user-' || u.id::text FROM users u
			WHERE lower(u.email) LIKE '%@example.test')`); err != nil {
			t.Errorf("purge personal teams: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id LIKE 'team-%'`); err != nil {
			t.Errorf("purge teams: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM users WHERE lower(email) LIKE '%@example.test'`); err != nil {
			t.Errorf("purge users: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	return NewService(pool, log)
}

func TestRoleOrdering(t *testing.T) {
	if !RoleOwner.AtLeast(RoleAdmin) || !RoleOwner.AtLeast(RoleMember) {
		t.Error("owner must outrank admin and member")
	}
	if !RoleAdmin.AtLeast(RoleMember) {
		t.Error("admin must outrank member")
	}
	if RoleMember.AtLeast(RoleAdmin) {
		t.Error("member must not outrank admin")
	}
	if !RoleAdmin.CanAdminister() || RoleMember.CanAdminister() {
		t.Error("CanAdminister: admin yes, member no")
	}
	if !RoleOwner.CanOwn() || RoleAdmin.CanOwn() {
		t.Error("CanOwn: owner only")
	}
}

// Addresses are one identity regardless of case. Treating Alice@ and alice@ as
// two people sends an invitation to an account nobody can sign in to.
func TestEnsureUserIsCaseInsensitive(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	a, err := s.EnsureUser(ctx, "Alice@Example.Test", "Alice")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	b, err := s.EnsureUser(ctx, "alice@example.test", "")
	if err != nil {
		t.Fatalf("EnsureUser again: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("two users created for one address: %s and %s", a.ID, b.ID)
	}
	// An empty name on a later sign-in must not wipe the one already there.
	if b.DisplayName != "Alice" {
		t.Fatalf("display name = %q, want Alice", b.DisplayName)
	}
}

func TestCreateTeamMakesTheCreatorAnOwner(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "owner@example.test", "Owner")
	if _, err := s.CreateTeam(ctx, "team-create", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	role, err := s.RoleIn(ctx, u.ID, "team-create")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("role = %q, want owner", role)
	}

	teams, err := s.TeamsFor(ctx, u.ID)
	if err != nil {
		t.Fatalf("TeamsFor: %v", err)
	}
	if len(teams) != 1 || teams[0].TeamID != "team-create" || teams[0].Role != RoleOwner {
		t.Fatalf("memberships = %+v, want one owner membership of team-create", teams)
	}
}

// The id is a slug someone chooses. Creating over a team that already has an
// owner would hand the caller that team's apps.
func TestCreateTeamRefusesAnIdSomeoneElseHolds(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	held, _ := s.EnsureUser(ctx, "holder@example.test", "Holder")
	if _, err := s.CreateTeam(ctx, "team-taken", "Team", held.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	other, _ := s.EnsureUser(ctx, "other@example.test", "Other")
	if _, err := s.CreateTeam(ctx, "team-taken", "Mine Now", other.ID); !errors.Is(err, ErrTeamTaken) {
		t.Fatalf("want ErrTeamTaken, got %v", err)
	}
	if _, err := s.RoleIn(ctx, other.ID, "team-taken"); !errors.Is(err, ErrNotAMember) {
		t.Fatal("the refused creation still made them a member")
	}

	// Re-creating a team you already own stays idempotent, so an operator can
	// re-run the bootstrap that made it.
	if _, err := s.CreateTeam(ctx, "team-taken", "Renamed", held.ID); err != nil {
		t.Fatalf("the owner re-creating their own team: %v", err)
	}
}

// A team with no owner cannot be administered by anyone ever again.
func TestLastOwnerCannotBeDemotedOrRemoved(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "solo@example.test", "Solo")
	if _, err := s.CreateTeam(ctx, "team-last-owner", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	if err := s.SetRole(ctx, u.ID, "team-last-owner", u.ID, RoleMember); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the last owner: want ErrLastOwner, got %v", err)
	}
	if err := s.RemoveMember(ctx, u.ID, "team-last-owner", u.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the last owner: want ErrLastOwner, got %v", err)
	}

	// The refusals must have changed nothing.
	role, err := s.RoleIn(ctx, u.ID, "team-last-owner")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("role after two refused changes = %q, want owner", role)
	}
}

func TestOnlyAnOwnerCanManageAdmins(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "o@example.test", "O")
	admin, _ := s.EnsureUser(ctx, "a@example.test", "A")
	if _, err := s.CreateTeam(ctx, "team-roles", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := s.SetRole(ctx, owner.ID, "team-roles", admin.ID, RoleAdmin); err != nil {
		t.Fatalf("owner promoting to admin: %v", err)
	}

	// An admin must not be able to make another admin — that is ownership.
	member, _ := s.EnsureUser(ctx, "m@example.test", "M")
	if err := s.SetRole(ctx, admin.ID, "team-roles", member.ID, RoleAdmin); err == nil {
		t.Fatal("an admin promoted someone to admin")
	}
	if err := s.SetRole(ctx, admin.ID, "team-roles", member.ID, RoleMember); err != nil {
		t.Fatalf("an admin must be able to add a member: %v", err)
	}

	// Nor may an admin unseat the person who appointed them.
	if err := s.RemoveMember(ctx, admin.ID, "team-roles", owner.ID); err == nil {
		t.Fatal("an admin removed an owner")
	}
	if err := s.RemoveMember(ctx, member.ID, "team-roles", admin.ID); err == nil {
		t.Fatal("a member removed an admin")
	}
}

func TestRoleInReportsNonMembers(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "stranger@example.test", "S")
	if _, err := s.RoleIn(ctx, u.ID, "team-does-not-exist"); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("want ErrNotAMember, got %v", err)
	}
}
