package account

import (
	"context"
	"errors"
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
