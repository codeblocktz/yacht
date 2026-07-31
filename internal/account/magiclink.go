package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
