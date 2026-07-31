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

// ErrTokenInvalid is returned for every magic link and invitation that cannot
// be used.
//
// One error for unknown, expired, already-consumed and malformed alike. A
// caller that could tell them apart would hand that difference to whoever is
// holding the token, and "this link has already been used" tells an attacker
// their guess was a real link.
var ErrTokenInvalid = errors.New("account: token is not valid")

// IssueMagicLink mints a sign-in link for an address, if anyone holds it.
//
// existed reports whether the address is registered, and is the only thing
// that differs between the two cases. The caller mails a link when it is true
// and nothing when it is false, while answering the visitor identically either
// way: a sign-in form that responds differently to a registered address is a
// user-list disclosure.
//
// An unknown address is deliberately not registered here. Doing so would let
// anyone fill the user table from the sign-in form, and would make the first
// sign-in of a stranger indistinguishable from that of an invited person.
func (s *Service) IssueMagicLink(
	ctx context.Context, email string, ttl time.Duration,
) (string, User, bool, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", User{}, false, errors.New("account: email is required")
	}

	// Minted before the lookup so that the same work happens whether or not the
	// address is known. It closes only the cheap half of the timing gap — the
	// insert below has no counterpart — so the caller still owes this endpoint
	// the rate limiting that makes timing unusable.
	raw, hash, err := NewToken()
	if err != nil {
		return "", User{}, false, err
	}

	row, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", User{}, false, nil
		}
		return "", User{}, false, fmt.Errorf("account: find user: %w", err)
	}
	user := toUser(row)

	if _, err := s.q.CreateMagicLink(ctx, dbgen.CreateMagicLinkParams{
		UserID:    user.ID,
		TokenHash: hash,
		ExpiresAt: time.Now().Add(ttl),
	}); err != nil {
		return "", User{}, false, fmt.Errorf("account: issue magic link: %w", err)
	}
	return raw, user, true, nil
}

// BootstrapOwner gives someone with no team one to act as, handing the first of
// them the team the install was already running as.
//
// This is the spec's third lockout guard. An install that switches accounts on
// has apps already deployed under YACHT_OWNER_ID; without this the first person
// to sign in lands in a fresh empty team and those apps belong to an owner
// nobody can authenticate as — visible only as a dashboard that has lost
// everything.
//
// The configured team is claimed only when it has no owner at all, and that is
// read and acted on inside one transaction holding the team row. Two people
// completing sign-in at the same moment therefore produce exactly one owner:
// the loser takes the second branch and gets a team of their own. The check is
// "has no owner" rather than "has no members" so that an install whose team was
// left with members but no owner is still recoverable.
//
// Idempotent: someone who already holds a role in the configured team is left
// exactly as they are, so a retried sign-in cannot mint a second team.
func (s *Service) BootstrapOwner(
	ctx context.Context, teamID, teamName string, user User,
) error {
	if user.ID == uuid.Nil {
		return errors.New("account: bootstrapping an owner needs a user")
	}
	teamID = strings.TrimSpace(teamID)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)

	claimed := teamID != ""
	if claimed {
		// FOR UPDATE is what serialises the count below against a second
		// sign-in. On a first install the row does not exist yet and there is
		// nothing to lock — the insert's own unique-index wait takes that job,
		// which is why the team is created before the count rather than after.
		if _, err := q.LockTeam(ctx, teamID); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("account: lock team: %w", err)
			}
			if _, err := q.CreateTeam(ctx, dbgen.CreateTeamParams{
				ID: teamID, DisplayName: teamName,
			}); err != nil {
				return fmt.Errorf("account: create team: %w", err)
			}
		}

		switch _, err := roleIn(ctx, q, user.ID, teamID); {
		case err == nil:
			// Already a member: there is nothing to hand out, and handing them a
			// second team would be the retry doing damage.
			return tx.Commit(ctx)
		case !errors.Is(err, ErrNotAMember):
			return err
		}

		owners, err := q.CountOwnersOfTeam(ctx, teamID)
		if err != nil {
			return fmt.Errorf("account: count owners: %w", err)
		}
		claimed = owners == 0
	}

	if !claimed {
		// Somebody else got there first, or there was no configured team to get
		// to. They still need somewhere to act, or the session resolves to no
		// owner and the dashboard is a wall.
		teamID, teamName = personalTeam(user)
		if _, err := q.CreateTeam(ctx, dbgen.CreateTeamParams{
			ID: teamID, DisplayName: teamName,
		}); err != nil {
			return fmt.Errorf("account: create team: %w", err)
		}
	}

	if _, err := q.UpsertMembership(ctx, dbgen.UpsertMembershipParams{
		UserID: user.ID, OwnerID: teamID, Role: string(RoleOwner),
	}); err != nil {
		return fmt.Errorf("account: create owner membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("account: commit: %w", err)
	}

	s.log.Info("owner bootstrapped",
		slog.String("team", teamID),
		slog.String("user", user.ID.String()),
		slog.Bool("inherited", claimed))
	return nil
}

// personalTeam names the team someone gets when the configured one is taken.
//
// The id is derived from the user id rather than from their address: two people
// at different domains share a local part, and a team id collision would hand
// one of them the other's apps.
func personalTeam(user User) (id, name string) {
	name = strings.TrimSpace(user.DisplayName)
	if name == "" {
		name = user.Email
	}
	return "user-" + user.ID.String(), name
}

// ConsumeMagicLink spends a sign-in link and returns the person it proves.
//
// Spending and reading are one statement, so a link cannot be followed twice —
// a link sits in a mailbox forever, and one that still worked the second time
// would be a permanent key to the account.
func (s *Service) ConsumeMagicLink(ctx context.Context, raw string) (User, error) {
	if raw == "" {
		return User{}, ErrTokenInvalid
	}
	row, err := s.q.ConsumeMagicLink(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrTokenInvalid
		}
		return User{}, fmt.Errorf("account: consume magic link: %w", err)
	}
	return toUser(row), nil
}
