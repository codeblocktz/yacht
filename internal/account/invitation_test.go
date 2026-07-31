package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// pendingInvitation reads the id of the outstanding invitation for an address,
// which is what a management page would list and hand to RevokeInvitation.
func pendingInvitation(t *testing.T, s *Service, teamID, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `
		SELECT id FROM invitations
		WHERE owner_id = $1 AND lower(email) = lower($2) AND accepted_at IS NULL
	`, teamID, email).Scan(&id); err != nil {
		t.Fatalf("read pending invitation: %v", err)
	}
	return id
}

// Inviting is administration: it hands someone the team's apps.
func TestMemberCannotInvite(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "invowner@example.test", "O")
	member, _ := s.EnsureUser(ctx, "invmember@example.test", "M")
	if _, err := s.CreateTeam(ctx, "team-invite-member", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := s.SetRole(ctx, owner.ID, "team-invite-member", member.ID, RoleMember); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	if _, err := s.Invite(ctx, member.ID, "team-invite-member",
		"stranger@example.test", RoleMember, time.Hour); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	// Nor may someone outside the team invite into it.
	outsider, _ := s.EnsureUser(ctx, "invoutsider@example.test", "X")
	if _, err := s.Invite(ctx, outsider.ID, "team-invite-member",
		"stranger@example.test", RoleMember, time.Hour); err == nil {
		t.Fatal("a non-member issued an invitation")
	}
}

// An admin who could invite an owner could hand away every permission the team
// has, which is the one thing ownership is for.
func TestAdminCannotInviteAnOwner(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "admowner@example.test", "O")
	admin, _ := s.EnsureUser(ctx, "admadmin@example.test", "A")
	if _, err := s.CreateTeam(ctx, "team-invite-owner", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := s.SetRole(ctx, owner.ID, "team-invite-owner", admin.ID, RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	if _, err := s.Invite(ctx, admin.ID, "team-invite-owner",
		"newowner@example.test", RoleOwner, time.Hour); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an admin invited an owner: %v", err)
	}
	if _, err := s.Invite(ctx, admin.ID, "team-invite-owner",
		"newmember@example.test", RoleMember, time.Hour); err != nil {
		t.Fatalf("an admin must be able to invite a member: %v", err)
	}
}

// Resending must not leave the first token live, or revoking the invitation
// someone can see does nothing to the one they cannot.
func TestReinvitingReplacesThePendingInvitation(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "reowner@example.test", "O")
	if _, err := s.CreateTeam(ctx, "team-reinvite", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	first, err := s.Invite(ctx, owner.ID, "team-reinvite",
		"joiner@example.test", RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	second, err := s.Invite(ctx, owner.ID, "team-reinvite",
		"joiner@example.test", RoleAdmin, time.Hour)
	if err != nil {
		t.Fatalf("Invite again: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM invitations WHERE owner_id = $1`, "team-reinvite").Scan(&n); err != nil {
		t.Fatalf("count invitations: %v", err)
	}
	if n != 1 {
		t.Fatalf("invitations = %d, want 1 outstanding per address per team", n)
	}

	joiner, _ := s.EnsureUser(ctx, "joiner@example.test", "J")
	if _, _, err := s.AcceptInvitation(ctx, first, joiner.ID); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("the replaced token still works: %v", err)
	}
	teamID, role, err := s.AcceptInvitation(ctx, second, joiner.ID)
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if teamID != "team-reinvite" || role != RoleAdmin {
		t.Fatalf("accepted %q as %q, want team-reinvite as admin", teamID, role)
	}
}

func TestAcceptedInvitationCreatesTheMembership(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "accowner@example.test", "O")
	if _, err := s.CreateTeam(ctx, "team-accept", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	raw, err := s.Invite(ctx, owner.ID, "team-accept", "accjoiner@example.test", RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	joiner, _ := s.EnsureUser(ctx, "accjoiner@example.test", "J")
	teamID, role, err := s.AcceptInvitation(ctx, raw, joiner.ID)
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if teamID != "team-accept" || role != RoleMember {
		t.Fatalf("accepted %q as %q", teamID, role)
	}

	got, err := s.RoleIn(ctx, joiner.ID, "team-accept")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if got != RoleMember {
		t.Fatalf("role = %q, want member", got)
	}
}

// The token is in a mailbox forever. Accepting twice must not be a way back in
// after being removed.
func TestAcceptedInvitationCannotBeReused(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "reuseowner@example.test", "O")
	if _, err := s.CreateTeam(ctx, "team-reuse", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	raw, err := s.Invite(ctx, owner.ID, "team-reuse", "reusejoiner@example.test", RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	joiner, _ := s.EnsureUser(ctx, "reusejoiner@example.test", "J")
	if _, _, err := s.AcceptInvitation(ctx, raw, joiner.ID); err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if err := s.RemoveMember(ctx, owner.ID, "team-reuse", joiner.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	if _, _, err := s.AcceptInvitation(ctx, raw, joiner.ID); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("second acceptance: want ErrTokenInvalid, got %v", err)
	}
	if _, err := s.RoleIn(ctx, joiner.ID, "team-reuse"); !errors.Is(err, ErrNotAMember) {
		t.Fatal("a reused invitation put a removed person back in the team")
	}
}

// Revoking is the only way to take back an invitation sent to the wrong
// address, and it has to reach the token rather than just the row on screen.
func TestRevokedInvitationCannotBeAccepted(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "revowner@example.test", "O")
	member, _ := s.EnsureUser(ctx, "revmember@example.test", "M")
	if _, err := s.CreateTeam(ctx, "team-revoke", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := s.SetRole(ctx, owner.ID, "team-revoke", member.ID, RoleMember); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	raw, err := s.Invite(ctx, owner.ID, "team-revoke", "revjoiner@example.test", RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	id := pendingInvitation(t, s, "team-revoke", "revjoiner@example.test")

	if err := s.RevokeInvitation(ctx, member.ID, "team-revoke", id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member revoked an invitation: %v", err)
	}
	if err := s.RevokeInvitation(ctx, owner.ID, "team-revoke", id); err != nil {
		t.Fatalf("RevokeInvitation: %v", err)
	}

	joiner, _ := s.EnsureUser(ctx, "revjoiner@example.test", "J")
	if _, _, err := s.AcceptInvitation(ctx, raw, joiner.ID); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a revoked invitation was accepted: %v", err)
	}
}

func TestExpiredInvitationIsRejected(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "expowner@example.test", "O")
	if _, err := s.CreateTeam(ctx, "team-invite-expired", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	raw, err := s.Invite(ctx, owner.ID, "team-invite-expired",
		"expjoiner@example.test", RoleMember, -time.Minute)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	joiner, _ := s.EnsureUser(ctx, "expjoiner@example.test", "J")
	if _, _, err := s.AcceptInvitation(ctx, raw, joiner.ID); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("want ErrTokenInvalid for an expired invitation, got %v", err)
	}
}

// The database must not hold anything that can be mailed to a person.
func TestInvitationTokenIsStoredAsAHash(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "hashowner@example.test", "O")
	if _, err := s.CreateTeam(ctx, "team-invite-hash", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	raw, err := s.Invite(ctx, owner.ID, "team-invite-hash",
		"hashjoiner@example.test", RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM invitations WHERE encode(token_hash, 'escape') = $1`,
		raw).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatal("the raw invitation token is present in the database")
	}
}

// Accepting must never take away access the person already has: a sole owner
// accepting a member invitation would otherwise leave the team ownerless.
func TestAcceptingCannotDemote(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "demoteowner@example.test", "O")
	if _, err := s.CreateTeam(ctx, "team-demote", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	raw, err := s.Invite(ctx, owner.ID, "team-demote",
		"demoteowner@example.test", RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}

	if _, _, err := s.AcceptInvitation(ctx, raw, owner.ID); err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	role, err := s.RoleIn(ctx, owner.ID, "team-demote")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("role = %q, want owner — accepting demoted the last owner", role)
	}
}
