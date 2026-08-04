package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrSessionInvalid is returned for every session that cannot be used.
//
// One error for unknown, expired and malformed alike: a caller that could tell
// them apart could tell a real token from a guess, and so could anyone probing
// the endpoint that surfaces the difference.
var ErrSessionInvalid = errors.New("account: session is not valid")

// DefaultCookieName is the cookie the session provider reads.
const DefaultCookieName = "yacht_session"

// Session is a signed-in browser.
//
// It carries the active team's name and address so the request that presented
// the cookie can be resolved to an owner in one round trip, the same reason
// Membership carries the team name.
type Session struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	ActiveTeamID string
	TeamName     string
	TeamEmail    string

	// Role the person holds in the active team. Carried on the session because
	// resolving one already proves the membership exists — sub-project C's
	// route gates would otherwise re-query it on every request.
	Role Role

	UserAgent string
	IP        string
	ExpiresAt time.Time
	CreatedAt time.Time

	// AuthenticatedAt is when whoever holds this session last proved they are
	// its owner, as opposed to when the session began. Set at creation and
	// refreshed when a password is re-entered to confirm a change.
	//
	// Separate from CreatedAt because a session that re-proves itself is still
	// the same session. Folding the two together would mean the only way to
	// refresh the window is to mint a new one, which is a sign-in rather than a
	// confirmation.
	AuthenticatedAt time.Time
}

// RecentlyAuthenticated reports whether this session proved who holds it within
// d.
//
// Read at the moment of a write and never at page render: a form that was
// rendered inside the window and submitted outside it must not be honoured, and
// the service checks this itself for exactly that reason.
func (s Session) RecentlyAuthenticated(d time.Duration) bool {
	return time.Since(s.AuthenticatedAt) < d
}

// CreateSession issues a session for a person acting in a team, and returns the
// token to put in their cookie.
//
// The token is returned once and never again: only its hash is stored, so a
// database dump is not a set of live sessions.
//
// Membership of teamID is checked here rather than trusted from the caller.
// The active team is what every downstream query is scoped by, so a session
// opened onto a team the person does not belong to is a silent grant of that
// team's apps.
func (s *Service) CreateSession(
	ctx context.Context, userID uuid.UUID, teamID, userAgent, ip string, ttl time.Duration,
) (string, error) {
	if userID == uuid.Nil {
		return "", errors.New("account: a session needs a user")
	}

	var active *string
	if teamID != "" {
		if _, err := s.RoleIn(ctx, userID, teamID); err != nil {
			return "", err
		}
		active = &teamID
	}

	raw, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	if _, err := s.q.CreateSession(ctx, dbgen.CreateSessionParams{
		UserID:       userID,
		TokenHash:    hash,
		ActiveTeamID: active,
		UserAgent:    userAgent,
		Ip:           ip,
		ExpiresAt:    time.Now().Add(ttl),
	}); err != nil {
		return "", fmt.Errorf("account: create session: %w", err)
	}
	return raw, nil
}

// ResolveSession returns the session a token stands for.
//
// The presented token is hashed and looked up by hash; raw values are never
// compared, because there is no raw value stored to compare against.
func (s *Service) ResolveSession(ctx context.Context, raw string) (Session, error) {
	if raw == "" {
		return Session{}, ErrSessionInvalid
	}
	row, err := s.q.GetSessionByHash(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrSessionInvalid
		}
		return Session{}, fmt.Errorf("account: resolve session: %w", err)
	}
	return Session{
		ID:           row.ID,
		UserID:       row.UserID,
		ActiveTeamID: deref(row.ActiveTeamID),
		// Not nullable any more: the query inner-joins teams and memberships,
		// so a row only comes back when both still exist.
		TeamName:        row.TeamName,
		TeamEmail:       row.TeamEmail,
		Role:            Role(row.MemberRole),
		UserAgent:       row.UserAgent,
		IP:              row.Ip,
		ExpiresAt:       row.ExpiresAt,
		CreatedAt:       row.CreatedAt,
		AuthenticatedAt: row.AuthenticatedAt,
	}, nil
}

// SwitchTeam points a session at another of the person's teams.
//
// Membership is checked against the session's own user, not against anything
// the request supplied, so a crafted team id switches into nothing.
func (s *Service) SwitchTeam(ctx context.Context, sessionID uuid.UUID, teamID string) error {
	row, err := s.q.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSessionInvalid
		}
		return fmt.Errorf("account: get session: %w", err)
	}
	if _, err := s.RoleIn(ctx, row.UserID, teamID); err != nil {
		return err
	}
	if err := s.q.SetSessionTeam(ctx, dbgen.SetSessionTeamParams{
		ActiveTeamID: &teamID, ID: sessionID,
	}); err != nil {
		return fmt.Errorf("account: switch team: %w", err)
	}
	return nil
}

// RevokeSession ends one session — the sign-out on this browser.
func (s *Service) RevokeSession(ctx context.Context, raw string) error {
	if raw == "" {
		return ErrSessionInvalid
	}
	if err := s.q.DeleteSessionByHash(ctx, HashToken(raw)); err != nil {
		return fmt.Errorf("account: revoke session: %w", err)
	}
	return nil
}

// RevokeAllSessions ends every session a person holds.
//
// This is the answer to a stolen cookie. Without it a leaked session runs until
// it expires and there is nothing the person can do about it.
func (s *Service) RevokeAllSessions(ctx context.Context, userID uuid.UUID) error {
	if err := s.q.DeleteSessionsForUser(ctx, userID); err != nil {
		return fmt.Errorf("account: revoke sessions: %w", err)
	}
	return nil
}

// Provider returns the identity.Provider backed by these sessions.
func (s *Service) Provider(cookieName string) *Sessions {
	if cookieName == "" {
		cookieName = DefaultCookieName
	}
	return &Sessions{svc: s, cookie: cookieName}
}

// Sessions resolves the owner of a request from a session cookie.
//
// The third identity.Provider the engine ships, alongside SingleOwner and
// StaticToken. It exists as a Provider rather than as a replacement for the
// seam so that every handler, the app service, the store and the orchestrator
// stay untouched: they already take an OwnerID and never ask where it came
// from, and a wrapping application still swaps the whole provider out.
type Sessions struct {
	svc    *Service
	cookie string
}

var _ identity.Provider = (*Sessions)(nil)

// Resolve returns the ACTIVE TEAM as the owner, not the user.
//
// Everything downstream is scoped by owner_id, and owner_id means team. A
// session that resolved to the person would silently widen every query to
// every team they belong to.
func (p *Sessions) Resolve(ctx context.Context, r *http.Request) (identity.Owner, error) {
	c, err := r.Cookie(p.cookie)
	if err != nil || c.Value == "" {
		return identity.Owner{}, identity.ErrUnauthenticated
	}

	sess, err := p.svc.ResolveSession(ctx, c.Value)
	if err != nil {
		return identity.Owner{}, identity.ErrUnauthenticated
	}
	// A session with no active team is signed in but has nothing to act as —
	// the person belongs to no team, or the one they were in was deleted. The
	// surface that offers them a team to join is sub-project C's; here it is
	// simply not an owner.
	if sess.ActiveTeamID == "" {
		return identity.Owner{}, identity.ErrUnauthenticated
	}

	return identity.Owner{
		ID:          sess.ActiveTeamID,
		DisplayName: sess.TeamName,
		Email:       sess.TeamEmail,
	}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
