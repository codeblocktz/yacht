package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// IssueSignupLink mints a link that creates an account for an address when it
// is followed.
//
// It is the open-sign-up counterpart of IssueMagicLink, for an address nobody
// holds yet, and deliberately creates nobody: an account made here would let
// anyone fill the user table from the sign-in form with addresses they cannot
// read. The account is made by ConsumeSignupLink, which only the owner of the
// mailbox can reach.
func (s *Service) IssueSignupLink(ctx context.Context, email string, ttl time.Duration) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", errors.New("account: email is required")
	}
	raw, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	if _, err := s.q.CreateSignupLink(ctx, dbgen.CreateSignupLinkParams{
		Email:     email,
		TokenHash: hash,
		ExpiresAt: time.Now().Add(ttl),
	}); err != nil {
		return "", fmt.Errorf("account: issue sign-up link: %w", err)
	}
	return raw, nil
}

// ConsumeSignupLink spends a sign-up link and returns the person it creates.
//
// Spending it and creating the account are one transaction, so a link that
// fails half way can be followed again rather than leaving a spent link and no
// account. An address that has gained an account since the link was mailed —
// invited in the meantime, say — gets that account: it is the same mailbox, so
// the same person.
//
// Unknown, expired and spent links are all ErrTokenInvalid, as they are for
// sign-in links.
func (s *Service) ConsumeSignupLink(ctx context.Context, raw string) (User, error) {
	if raw == "" {
		return User{}, ErrTokenInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)
	email, err := q.ConsumeSignupLink(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrTokenInvalid
		}
		return User{}, fmt.Errorf("account: consume sign-up link: %w", err)
	}
	row, err := q.UpsertUser(ctx, dbgen.UpsertUserParams{Email: email})
	if err != nil {
		return User{}, fmt.Errorf("account: create user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("account: commit: %w", err)
	}
	user := toUser(row)
	s.log.Info("signed up", slog.String("user", user.ID.String()))
	return user, nil
}
