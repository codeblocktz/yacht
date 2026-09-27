package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Acting as a team: an operator of the install reaching a customer's team to
// help them.
//
// This package keeps the state and the record and decides nothing about who
// may do it. It does not know who runs the install — that is configuration the
// engine holds — so starting is gated by the caller, and the identity provider
// asks an ActingCheck on every request whether the person may still be acting.
// Asking every time, rather than once when it began, is what makes removing
// somebody from the install's operators take effect on their next request
// instead of whenever their session happens to expire.

// ErrNoSuchTeam is returned when acting is asked for a team that does not
// exist.
var ErrNoSuchTeam = errors.New("account: no such team")

// ImpersonationStart and ImpersonationStop are the two things the record says.
const (
	ImpersonationStart = "start"
	ImpersonationStop  = "stop"
)

// ImpersonationEvent is one line of the durable record: who began or stopped
// acting as a team, and when.
type ImpersonationEvent struct {
	ID     uuid.UUID
	TeamID string

	// UserID is uuid.Nil once the operator's account has been removed. The
	// address stays, which is why it is stored beside the id: a record that
	// stopped saying who it was about when they left would not be a record.
	UserID        uuid.UUID
	OperatorEmail string
	Action        string
	At            time.Time
}

// ActingCheck reports whether a person may act as a team other than their own.
//
// Given the person rather than the request, because it is a question about
// who they are and not about what they sent: a check that could read a header
// could be answered by whoever sets it.
type ActingCheck func(ctx context.Context, user User) bool

// StartActing layers a team on top of a session, so that every request it
// makes resolves to that team until acting stops.
//
// Whether the session's holder may do this is the caller's decision and is
// not re-made here; the identity provider re-makes it on every request after.
// Acting as another team while already acting as one moves straight across,
// and the record says so — one stop, one start — rather than showing an
// impersonation that never ended. Asking again for the team already being
// acted as changes nothing and records nothing.
//
// The state and the record are one transaction. A durable record that can be
// missing an impersonation that happened is not one anybody can rely on.
func (s *Service) StartActing(ctx context.Context, sessionID uuid.UUID, teamID string) (Team, error) {
	var team Team
	err := s.inTx(ctx, func(q *dbgen.Queries) error {
		row, err := q.GetTeam(ctx, teamID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNoSuchTeam
			}
			return fmt.Errorf("account: read team: %w", err)
		}
		team = toTeam(row)

		cur, err := q.LockSessionActing(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrSessionInvalid
			}
			return fmt.Errorf("account: read session: %w", err)
		}
		if deref(cur.ActingTeamID) == teamID {
			return nil
		}
		if _, err := endActing(ctx, q, cur); err != nil {
			return err
		}

		n, err := q.StartActing(ctx, dbgen.StartActingParams{TeamID: teamID, ID: sessionID})
		if err != nil {
			return fmt.Errorf("account: start acting: %w", err)
		}
		if n == 0 {
			// Locked a moment ago and expired since. Nothing was started, so
			// nothing is recorded.
			return ErrSessionInvalid
		}
		return record(ctx, q, teamID, cur.UserID, cur.Email, ImpersonationStart)
	})
	return team, err
}

// StopActing puts a session back in its own team, returning the team it had
// been acting as — empty if it was not acting, which is not an error: the
// person asked to be themselves and already is.
func (s *Service) StopActing(ctx context.Context, sessionID uuid.UUID) (string, error) {
	var stopped string
	err := s.inTx(ctx, func(q *dbgen.Queries) error {
		cur, err := q.LockSessionActing(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrSessionInvalid
			}
			return fmt.Errorf("account: read session: %w", err)
		}
		stopped, err = endActing(ctx, q, cur)
		return err
	})
	return stopped, err
}

// ImpersonationEvents is the record for one team, newest first.
func (s *Service) ImpersonationEvents(ctx context.Context, teamID string, limit int32) ([]ImpersonationEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.q.ListImpersonationEvents(ctx, dbgen.ListImpersonationEventsParams{
		OwnerID: teamID, MaxRows: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("account: list impersonation events: %w", err)
	}
	out := make([]ImpersonationEvent, 0, len(rows))
	for _, r := range rows {
		ev := ImpersonationEvent{
			ID: r.ID, TeamID: r.OwnerID, OperatorEmail: r.OperatorEmail,
			Action: r.Action, At: r.At,
		}
		if r.UserID.Valid {
			ev.UserID = r.UserID.Bytes
		}
		out = append(out, ev)
	}
	return out, nil
}

// endActing clears a locked session's acting and records the stop, returning
// the team it ended — empty, with nothing written, when there was none. The
// row must have been read with LockSessionActing in the same transaction, which
// is what makes two racing stops record one.
func endActing(ctx context.Context, q *dbgen.Queries, cur dbgen.LockSessionActingRow) (string, error) {
	team := deref(cur.ActingTeamID)
	if team == "" {
		return "", nil
	}
	if err := q.ClearSessionActing(ctx, cur.ID); err != nil {
		return "", fmt.Errorf("account: stop acting: %w", err)
	}
	if err := record(ctx, q, team, cur.UserID, cur.Email, ImpersonationStop); err != nil {
		return "", err
	}
	return team, nil
}

func record(ctx context.Context, q *dbgen.Queries, teamID string, userID uuid.UUID, email, action string) error {
	if err := q.InsertImpersonationEvent(ctx, dbgen.InsertImpersonationEventParams{
		OwnerID: teamID, UserID: userID, OperatorEmail: email, Action: action,
	}); err != nil {
		return fmt.Errorf("account: record impersonation %s: %w", action, err)
	}
	return nil
}

// inTx runs fn in a transaction.
func (s *Service) inTx(ctx context.Context, fn func(q *dbgen.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("account: commit: %w", err)
	}
	return nil
}

// WithActingCheck lets sessions act as a team other than their own, for
// whoever check approves.
//
// Without one, no session acts as anything: an acting team left on a session
// is ignored and cleared. That is the safe reading of a provider built without
// thought for impersonation — a wrapping application that builds on these
// sessions does not inherit a way into every team by forgetting to wire this.
//
// Returns the provider so it can be chained where it is built.
func (p *Sessions) WithActingCheck(check ActingCheck) *Sessions {
	p.acting = check
	return p
}

// actingOwner decides whether a session that says it is acting as a team is
// still allowed to, and ends the acting when it is not.
//
// Three answers. Allowed: resolve to the team being acted as. Refused: clear
// the acting, record the stop, and resolve to the person's own team — the next
// request finds nothing to refuse. Unknown, because the person could not be
// read: resolve to their own team for this request but leave the acting in
// place. Failing closed is the part that matters; ending somebody's support
// session because the database hiccupped is not.
func (p *Sessions) actingOwner(ctx context.Context, sess Session) bool {
	user, err := p.svc.User(ctx, sess.UserID)
	if err != nil {
		p.svc.log.Error("read the person acting as a team",
			slog.String("user", sess.UserID.String()), slog.String("error", err.Error()))
		return false
	}
	if p.acting == nil || !p.acting(ctx, user) {
		p.lapse(ctx, sess, user)
		return false
	}
	return true
}

// lapse ends an impersonation its holder is no longer entitled to. Logged at
// Info rather than Warn: an operator taken off the list is the system working
// as designed, not something going wrong — but it is a stop, and every stop is
// logged with who and which team, like the ones somebody asked for.
func (p *Sessions) lapse(ctx context.Context, sess Session, user User) {
	team, err := p.svc.StopActing(ctx, sess.ID)
	if err != nil {
		p.svc.log.Error("end an impersonation that is no longer permitted",
			slog.String("user", sess.UserID.String()),
			slog.String("team", sess.ActingTeamID), slog.String("error", err.Error()))
		return
	}
	if team != "" {
		p.svc.log.Info("impersonation stopped: no longer permitted to act as a team",
			slog.String("operator", user.Email),
			slog.String("user", user.ID.String()),
			slog.String("team", team))
	}
}
