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

// ErrCredentialInvalid is returned for every failed password sign-in.
//
// One error for an unknown address, an address with no password, a wrong
// password, and a stored hash that will not parse. A caller that could tell them
// apart would be a user-list disclosure with extra steps, which is the same rule
// ErrTokenInvalid and ErrSessionInvalid already state.
var ErrCredentialInvalid = errors.New("account: those sign-in details are not valid")

// ErrNoPassword is returned when there was nothing to remove.
//
// Distinct from success so that the page can say "you do not have one" rather
// than report that a credential which never existed has been withdrawn — which
// is how somebody stops looking for the one that is still there.
var ErrNoPassword = errors.New("account: no password is set")

// ErrNeedsReauthentication is returned when a session has not proved who is
// holding it recently enough to change how it can be signed in to.
//
// Not a failure to report as an error: it is the signal that asks the person to
// confirm, so it has to be distinguishable, which is why it is not folded into
// ErrForbidden.
var ErrNeedsReauthentication = errors.New("account: this needs to be confirmed first")

// StepUpWindow is how long a session stays recently-authenticated.
//
// Long enough to change a password and then think better of it without proving
// twice; short enough that a laptop somebody walked away from is not a
// credential.
const StepUpWindow = 10 * time.Minute

// Method names a way in.
//
// It exists for the sign-in log line, where an operator reading an unexpected
// session needs to know which door it came through. It is deliberately not a
// field on Session and not an argument to anything that makes a decision:
// nothing downstream of a completed sign-in is allowed to behave differently by
// method, because that is how two ways in stop producing the same session.
type Method string

const (
	MethodMagicLink Method = "magic_link"
	MethodPassword  Method = "password"
)

// Proof is how a session re-proves who is holding it.
//
// Empty means "rely on a recent sign-in". Somebody who has no password has no
// other option, and that is exactly the case adding a first password has to
// support — which is why this is a struct with one optional field rather than a
// required argument.
type Proof struct {
	CurrentPassword string
}

// AuthenticatePassword verifies an address and a password and returns the person
// they prove.
//
// Returns a User and nothing else, so that it produces exactly what
// ConsumeMagicLink produces and the caller opens a session the same way for
// both. Rate limiting is the caller's, because the limiter that exists is
// in-process and documents itself as a brake rather than a boundary. The equal
// cost of a hit and a miss is this function's, and is not something a caller
// could add afterwards.
func (s *Service) AuthenticatePassword(ctx context.Context, email, plain string) (User, error) {
	email = strings.TrimSpace(email)

	row, err := s.q.GetPasswordByEmail(ctx, email)

	// The dummy hash is verified when there is no row, so that an address nobody
	// holds costs the same as one that is registered. found is read after the
	// verify and never instead of it: an early return here is the enumeration
	// oracle this whole arrangement exists to close.
	encoded, found := dummyHash(), false
	switch {
	case err == nil:
		encoded, found = row.Secret, true
	case errors.Is(err, pgx.ErrNoRows):
		// An unknown address, or a known one with no password. The query
		// inner-joins, so these are the same answer by construction and there is
		// no branch here that could tell them apart.
	default:
		return User{}, fmt.Errorf("account: authenticate password: %w", err)
	}

	ok, stale, verr := verifyPassword(encoded, plain)
	if verr != nil {
		// A row that will not parse is an operational fault, not something the
		// person signing in can act on or should learn about. It is logged and
		// then answered exactly like a wrong password, having already cost the
		// same time.
		s.log.Error("stored password hash is unreadable",
			slog.String("user", row.ID.String()), slog.String("error", verr.Error()))
	}
	if !ok || !found {
		return User{}, ErrCredentialInvalid
	}

	user := User{
		ID:          row.ID,
		Email:       row.Email,
		DisplayName: row.DisplayName,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}

	if stale {
		s.rehash(ctx, user.ID, plain, row.Secret)
	}
	return user, nil
}

// rehash re-stores a password at the current cost after a sign-in that already
// succeeded.
//
// Best effort by design: this runs after the person is through, so a failure
// here must never turn a good password into a refused one. The write is
// conditional on the old value, which makes it a compare-and-swap — between the
// verify above and this update the person may have changed their password in
// another tab, and overwriting that with a rehash of the old one would lock them
// out of an account they had just secured.
func (s *Service) rehash(ctx context.Context, userID uuid.UUID, plain, old string) {
	next, err := hashPassword(plain)
	if err != nil {
		s.log.Error("rehash password", slog.String("error", err.Error()))
		return
	}
	if _, err := s.q.UpdatePasswordSecret(ctx, dbgen.UpdatePasswordSecretParams{
		UserID: userID, Secret: next, OldSecret: old,
	}); err != nil {
		s.log.Error("store rehashed password", slog.String("error", err.Error()))
	}
}

// HasPassword reports whether an account can be signed in to with one.
//
// Answers the account page, which already knows who it is rendering for, so it
// takes a user id rather than a session. It is never called from a signed-out
// path: "does this address have a password" is precisely the disclosure the
// sign-in design exists to withhold.
func (s *Service) HasPassword(ctx context.Context, userID uuid.UUID) (bool, error) {
	has, err := s.q.HasPassword(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("account: has password: %w", err)
	}
	return has, nil
}

// SetPassword adds or replaces the password on the account holding a session.
//
// It takes a session id and not a user id, for the reason SwitchTeam takes one:
// whose password this is comes from the session row, never from anything the
// request supplied. A user id in this signature would be a way to set anybody's
// password by knowing their id, and the only thing standing in the way would be
// every caller remembering to check.
//
// Recent authentication is verified here rather than trusted from the caller,
// the same way CreateSession verifies membership itself. Adding a password is
// how a stolen cookie becomes a permanent credential, so the check that stops
// that must not be skippable by a handler that forgot to make it.
//
// No old password is required as such, and that is not a relaxation. Somebody
// adding their first password has no old one, so demanding it for a change but
// not for an add would be two flows, two forms and two chances to get the
// recency check wrong. One Proof covers both, and for a person who does have a
// password, typing it is what satisfies it.
//
// Replacing a password ends every other session; adding a first one does not.
// Gaining a way in is not a change of trust, and signing somebody out of their
// other devices for it reads as the feature having broken something. The
// difference is read inside the transaction, so it cannot race.
func (s *Service) SetPassword(
	ctx context.Context, sessionID uuid.UUID, plain string, proof Proof,
) error {
	// Hashed before the transaction opens. This is 60-90ms of CPU plus a wait on
	// a bounded slot, and doing it inside would hold a pooled connection and a
	// row lock for work that touches no row. The natural way to write this is
	// the wrong way round.
	encoded, err := s.preparePassword(ctx, sessionID, plain)
	if err != nil {
		return err
	}

	return s.inSession(ctx, sessionID, func(q *dbgen.Queries, sess dbgen.GetLiveSessionRow) error {
		existing, err := stepUp(ctx, q, sess, proof)
		if err != nil {
			return err
		}

		if _, err := q.UpsertPassword(ctx, dbgen.UpsertPasswordParams{
			UserID: sess.UserID, Secret: encoded,
		}); err != nil {
			return fmt.Errorf("account: set password: %w", err)
		}

		// Only a replace. See the doc comment.
		if existing {
			return revokeOthers(ctx, q, sess.UserID, sess.ID)
		}
		return nil
	})
}

// preparePassword validates a chosen password and returns its stored form.
//
// The address is read from the session's own user so that the "not your email"
// rule cannot be dodged by a request that supplies a different one.
func (s *Service) preparePassword(
	ctx context.Context, sessionID uuid.UUID, plain string,
) (string, error) {
	sess, err := s.q.GetLiveSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrSessionInvalid
		}
		return "", fmt.Errorf("account: read session: %w", err)
	}
	user, err := s.q.GetUserByID(ctx, sess.UserID)
	if err != nil {
		return "", fmt.Errorf("account: read user: %w", err)
	}
	if err := checkPasswordPolicy(plain, user.Email); err != nil {
		return "", err
	}
	return hashPassword(plain)
}

// RemovePassword takes the password off an account, leaving the magic link.
//
// Allowed deliberately: the person can still get in through their mailbox, so
// this cannot lock anybody out, and refusing it would mean a password added once
// can never be taken back. Every other session goes with it — removing a
// password is what somebody does when they believe it is known.
func (s *Service) RemovePassword(ctx context.Context, sessionID uuid.UUID, proof Proof) error {
	return s.inSession(ctx, sessionID, func(q *dbgen.Queries, sess dbgen.GetLiveSessionRow) error {
		if _, err := stepUp(ctx, q, sess, proof); err != nil {
			return err
		}

		n, err := q.DeletePassword(ctx, sess.UserID)
		if err != nil {
			return fmt.Errorf("account: remove password: %w", err)
		}
		if n == 0 {
			return ErrNoPassword
		}
		return revokeOthers(ctx, q, sess.UserID, sess.ID)
	})
}

// RevokeOtherSessions ends every session a person holds except one.
//
// RevokeAllSessions cannot serve here: it takes out the session doing the
// asking, so a change made from the account page would sign the person out of
// the browser they are looking at, and they would read that as the change having
// failed rather than having worked.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keep uuid.UUID) error {
	return revokeOthers(ctx, s.q, userID, keep)
}

func revokeOthers(ctx context.Context, q *dbgen.Queries, userID, keep uuid.UUID) error {
	if err := q.DeleteOtherSessionsForUser(ctx, dbgen.DeleteOtherSessionsForUserParams{
		UserID: userID, Keep: keep,
	}); err != nil {
		return fmt.Errorf("account: revoke other sessions: %w", err)
	}
	return nil
}

// stepUp refuses unless the session has proved who is holding it recently
// enough, and reports whether the person already has a password.
//
// Two ways to satisfy it. A session authenticated inside the window is already
// proved — that is what makes a magic link a complete answer for somebody who
// has no password to type, and therefore what makes a separate recovery token
// unnecessary. Otherwise the current password stands in for it, and proving it
// re-opens the window in the same transaction, so a person changing two things
// in a row is not asked twice.
//
// The stored hash is read from the locked row rather than fetched separately, so
// the credential this checks against is the same one the caller is about to
// overwrite.
func stepUp(
	ctx context.Context, q *dbgen.Queries, sess dbgen.GetLiveSessionRow, proof Proof,
) (exists bool, err error) {
	row, err := q.LockPassword(ctx, sess.UserID)
	switch {
	case err == nil:
		exists = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return false, fmt.Errorf("account: read password: %w", err)
	}

	if time.Since(sess.AuthenticatedAt) < StepUpWindow {
		return exists, nil
	}

	// Nothing offered and nothing recent. The caller sends the person to prove
	// who they are, by whichever means their account actually has.
	if proof.CurrentPassword == "" || !exists {
		return exists, ErrNeedsReauthentication
	}

	ok, _, verr := verifyPassword(row.Secret, proof.CurrentPassword)
	if verr != nil {
		return exists, ErrCredentialInvalid
	}
	if !ok {
		return exists, ErrCredentialInvalid
	}

	// Proving the password is an authentication, so the window opens. Without
	// this, somebody changing a password and then removing it would be asked
	// for the old one twice — and the second time it would already be gone.
	if _, err := q.TouchSessionAuthentication(ctx, sess.ID); err != nil {
		return exists, fmt.Errorf("account: record authentication: %w", err)
	}
	return exists, nil
}

// inSession runs fn in a transaction holding a live session row.
//
// The session is read with GetLiveSession rather than GetSession because that
// one filters expiry in SQL. A session that ended an hour ago must not be able
// to set a password, and a caller that has to remember to check is a caller that
// will eventually not.
func (s *Service) inSession(
	ctx context.Context, sessionID uuid.UUID,
	fn func(q *dbgen.Queries, sess dbgen.GetLiveSessionRow) error,
) error {
	if sessionID == uuid.Nil {
		return ErrSessionInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)

	sess, err := q.GetLiveSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSessionInvalid
		}
		return fmt.Errorf("account: read session: %w", err)
	}

	if err := fn(q, sess); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("account: commit: %w", err)
	}
	return nil
}
