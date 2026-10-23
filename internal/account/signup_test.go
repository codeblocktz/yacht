package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Issuing is not signing up. A stranger typing addresses into the form must not
// fill the user table with mailboxes they cannot read.
func TestIssueSignupLinkCreatesNobody(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	raw, err := s.IssueSignupLink(ctx, "newcomer@example.test", time.Minute)
	if err != nil || raw == "" {
		t.Fatalf("IssueSignupLink = %q, %v", raw, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE lower(email) = 'newcomer@example.test'`).Scan(&n); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if n != 0 {
		t.Fatal("issuing a sign-up link created the user before anybody followed it")
	}
}

// Following it is: the account exists, once, and the link is spent.
func TestSignupLinkCreatesTheAccountOnce(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	raw, err := s.IssueSignupLink(ctx, "Newcomer@Example.test", time.Minute)
	if err != nil {
		t.Fatalf("IssueSignupLink: %v", err)
	}
	u, err := s.ConsumeSignupLink(ctx, raw)
	if err != nil {
		t.Fatalf("ConsumeSignupLink: %v", err)
	}
	if u.Email != "newcomer@example.test" {
		t.Fatalf("created %q, want the address lowercased", u.Email)
	}
	if _, err := s.ConsumeSignupLink(ctx, raw); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("second use: want ErrTokenInvalid, got %v", err)
	}
}

// Somebody invited between the mail and the click is the same mailbox, and so
// the same person: the link signs them in rather than failing or duplicating.
func TestSignupLinkForAnAddressThatGainedAnAccount(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	raw, err := s.IssueSignupLink(ctx, "both@example.test", time.Minute)
	if err != nil {
		t.Fatalf("IssueSignupLink: %v", err)
	}
	existing, err := s.EnsureUser(ctx, "both@example.test", "Both")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	u, err := s.ConsumeSignupLink(ctx, raw)
	if err != nil {
		t.Fatalf("ConsumeSignupLink: %v", err)
	}
	if u.ID != existing.ID || u.DisplayName != "Both" {
		t.Fatalf("got %+v, want the existing account %+v unchanged", u, existing)
	}
}

func TestExpiredSignupLinkIsRejected(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	raw, err := s.IssueSignupLink(ctx, "late@example.test", -time.Minute)
	if err != nil {
		t.Fatalf("IssueSignupLink: %v", err)
	}
	if _, err := s.ConsumeSignupLink(ctx, raw); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expired link: want ErrTokenInvalid, got %v", err)
	}
	if _, err := s.ConsumeSignupLink(ctx, "not-a-token"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("unknown link: want ErrTokenInvalid, got %v", err)
	}
}
