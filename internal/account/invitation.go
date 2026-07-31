package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrInvitationNotFound is returned when there is no such invitation to revoke
// in this team.
//
// Distinct from success so a caller can say "there was nothing to revoke"
// rather than reporting that a still-live token has been withdrawn.
var ErrInvitationNotFound = errors.New("account: no such invitation")

// Invite issues an invitation to join a team, and returns the token to mail.
//
// The actor's authority is checked with the same rule that governs role
// changes: inviting is administration, and an admin cannot invite anyone who
// could administer, because appointing an admin is a step away from appointing
// an owner.
//
// Inviting an address that already has an outstanding invitation replaces it,
// so the older token stops working. Two live tokens for one address would mean
// revoking the invitation on screen leaves the other one usable.
func (s *Service) Invite(
	ctx context.Context, actor uuid.UUID, teamID, email string, role Role, ttl time.Duration,
) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", errors.New("account: an invitation needs an address")
	}
	if !role.Valid() {
		return "", fmt.Errorf("account: unknown role %q", role)
	}

	raw, hash, err := NewToken()
	if err != nil {
		return "", err
	}

	err = s.inTeam(ctx, teamID, func(q *dbgen.Queries) error {
		// The target is nobody yet — an invitation names an address, not a
		// user — so authorise is asked only whether the actor may hand out
		// this role at all.
		if _, err := s.authorise(ctx, q, actor, teamID, uuid.Nil, role); err != nil {
			return err
		}
		if _, err := q.UpsertInvitation(ctx, dbgen.UpsertInvitationParams{
			OwnerID:   teamID,
			Email:     email,
			Role:      string(role),
			TokenHash: hash,
			InvitedBy: actor,
			ExpiresAt: time.Now().Add(ttl),
		}); err != nil {
			return fmt.Errorf("account: invite: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	s.log.Info("invitation issued",
		slog.String("team", teamID),
		slog.String("email", email),
		slog.String("role", string(role)))
	return raw, nil
}

// AcceptInvitation spends an invitation and returns the team and role it grants.
//
// The invitation is spent and the membership written in one transaction, so an
// invitation can never be marked accepted by someone who did not end up in the
// team, and the token can never be spent twice.
func (s *Service) AcceptInvitation(
	ctx context.Context, raw string, userID uuid.UUID,
) (string, Role, error) {
	if raw == "" {
		return "", "", ErrTokenInvalid
	}
	if userID == uuid.Nil {
		return "", "", errors.New("account: accepting an invitation needs a user")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)

	// Read the accepter first. The invitation names an address, and the token
	// alone must not confer the role — otherwise a forwarded or intercepted
	// mail hands membership to whoever opened it.
	accepter, err := q.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrTokenInvalid
		}
		return "", "", fmt.Errorf("account: accept invitation: %w", err)
	}

	row, err := q.AcceptInvitation(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrTokenInvalid
		}
		return "", "", fmt.Errorf("account: accept invitation: %w", err)
	}

	// Compared case-insensitively, because that is how the address was stored
	// and how the person will type it. Rolling the transaction back leaves the
	// invitation unconsumed, so the intended recipient can still use it.
	if !strings.EqualFold(strings.TrimSpace(row.Email), strings.TrimSpace(accepter.Email)) {
		return "", "", ErrTokenInvalid
	}

	granted := Role(row.Role)

	current, err := roleIn(ctx, q, userID, row.OwnerID)
	if err != nil && !errors.Is(err, ErrNotAMember) {
		return "", "", err
	}
	// An invitation grants access; it must never take away access already
	// held. Without this, a team's sole owner accepting a member invitation
	// would demote themselves and leave the team with no owner at all.
	if current.AtLeast(granted) {
		granted = current
	} else if _, err := q.UpsertMembership(ctx, dbgen.UpsertMembershipParams{
		UserID: userID, OwnerID: row.OwnerID, Role: string(granted),
	}); err != nil {
		return "", "", fmt.Errorf("account: accept invitation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("account: commit: %w", err)
	}

	s.log.Info("invitation accepted",
		slog.String("team", row.OwnerID),
		slog.String("user", userID.String()),
		slog.String("role", string(granted)))
	return row.OwnerID, granted, nil
}

// RevokeInvitation withdraws an invitation that has not been accepted.
//
// Deleting the row is what takes the token out of circulation, which is the
// only way to take back an invitation sent to the wrong address — the mail
// itself cannot be recalled.
func (s *Service) RevokeInvitation(
	ctx context.Context, actor uuid.UUID, teamID string, id uuid.UUID,
) error {
	return s.inTeam(ctx, teamID, func(q *dbgen.Queries) error {
		actorRole, err := roleIn(ctx, q, actor, teamID)
		if err != nil {
			return err
		}
		if !actorRole.CanAdminister() {
			return ErrForbidden
		}
		// The delete is scoped by team as well as by id, so an id belonging to
		// another team is not reachable by someone who administers this one.
		n, err := q.DeleteInvitation(ctx, dbgen.DeleteInvitationParams{
			ID: id, OwnerID: teamID,
		})
		if err != nil {
			return fmt.Errorf("account: revoke invitation: %w", err)
		}
		if n == 0 {
			return ErrInvitationNotFound
		}
		return nil
	})
}
