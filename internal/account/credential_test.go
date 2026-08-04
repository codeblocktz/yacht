package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// signedIn returns a person, a team they own, and a live session on it.
//
// The session id is what every credential method takes, so the tests need it
// rather than the raw token — which is the point of the signature: whose
// password is being changed comes from the session row and never from the
// caller.
func signedIn(t *testing.T, s *Service, name string) (User, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	u, err := s.EnsureUser(ctx, name+"@example.test", name)
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if _, err := s.CreateTeam(ctx, "team-"+name, "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	raw, err := s.CreateSession(ctx, u.ID, "team-"+name, "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := s.ResolveSession(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	return u, sess.ID
}

// ageSession pushes a session's last authentication back out of the step-up
// window, which is the only way to test the stale-session branch without
// sleeping for ten minutes.
func ageSession(t *testing.T, s *Service, id uuid.UUID) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET authenticated_at = now() - interval '1 hour' WHERE id = $1`,
		id); err != nil {
		t.Fatalf("age session: %v", err)
	}
}

func storedSecret(t *testing.T, s *Service, userID uuid.UUID) string {
	t.Helper()
	var secret string
	err := s.pool.QueryRow(context.Background(),
		`SELECT secret FROM user_credentials WHERE user_id = $1 AND kind = 'password'`,
		userID).Scan(&secret)
	if err != nil {
		return ""
	}
	return secret
}

// A new session is authenticated now, so a person who has just followed a magic
// link can add a password without proving anything else.
//
// This is the whole recovery story. If a fresh session did not satisfy step-up,
// somebody who forgot their password would have no way back in but a second kind
// of mailed token, which is the table this design does not have.
func TestAFreshSessionCanAddAPassword(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "adds")

	if err := s.SetPassword(ctx, sess, "a first password", Proof{}); err != nil {
		t.Fatalf("SetPassword on a fresh session: %v", err)
	}

	got, err := s.AuthenticatePassword(ctx, u.Email, "a first password")
	if err != nil {
		t.Fatalf("the password that was just set does not authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("authenticated as %s, want %s", got.ID, u.ID)
	}
}

// An old session must not be able to add a password on its own.
//
// This is the left-open-laptop escalation: a cookie somebody walked away from
// turning itself into a permanent credential. The status alone is not enough to
// assert — a handler that wrote first and complained afterwards would pass that
// — so the database is checked too.
func TestAnOldSessionCannotAddAPassword(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "stale")
	ageSession(t, s, sess)

	err := s.SetPassword(ctx, sess, "a password from a stale session", Proof{})
	if !errors.Is(err, ErrNeedsReauthentication) {
		t.Fatalf("SetPassword = %v, want ErrNeedsReauthentication", err)
	}
	if secret := storedSecret(t, s, u.ID); secret != "" {
		t.Error("a password was written despite the step-up refusal")
	}
}

// An old session may still change a password by producing the current one.
func TestAnOldSessionCanChangeAPasswordWithTheCurrentOne(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "proves")
	if err := s.SetPassword(ctx, sess, "the first password", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	ageSession(t, s, sess)

	err := s.SetPassword(ctx, sess, "the second password", Proof{CurrentPassword: "wrong"})
	if !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("a wrong current password gave %v, want ErrCredentialInvalid", err)
	}

	if err := s.SetPassword(ctx, sess, "the second password",
		Proof{CurrentPassword: "the first password"}); err != nil {
		t.Fatalf("SetPassword with the current one: %v", err)
	}

	if _, err := s.AuthenticatePassword(ctx, u.Email, "the second password"); err != nil {
		t.Errorf("the new password does not authenticate: %v", err)
	}
	if _, err := s.AuthenticatePassword(ctx, u.Email, "the first password"); !errors.Is(
		err, ErrCredentialInvalid) {
		t.Error("the old password still authenticates after being replaced")
	}
}

// Proving the current password re-opens the step-up window.
//
// Without this, somebody who changes their password and then decides to remove
// it is asked for the old one a second time — and by then it is gone, so the
// only way through is to sign in again.
func TestProvingThePasswordReopensTheWindow(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	_, sess := signedIn(t, s, "reopens")
	if err := s.SetPassword(ctx, sess, "the first password", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	ageSession(t, s, sess)

	if err := s.SetPassword(ctx, sess, "the second password",
		Proof{CurrentPassword: "the first password"}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	// No proof offered this time. The change above is what stands in for it.
	if err := s.RemovePassword(ctx, sess, Proof{}); err != nil {
		t.Fatalf("RemovePassword straight after a proved change: %v", err)
	}
}

// Adding a first password must not sign the person out of anything.
//
// Gaining a way in is not a change of trust. Revoking here would read as the
// feature having broken the sessions somebody already had.
func TestAddingAFirstPasswordRevokesNothing(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "keeps")
	other, err := s.CreateSession(ctx, u.ID, "team-keeps", "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := s.SetPassword(ctx, sess, "a first password", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if _, err := s.ResolveSession(ctx, other); err != nil {
		t.Errorf("adding a first password ended another session: %v", err)
	}
}

// Replacing a password ends every other session and keeps this one.
//
// Both halves matter. Revoking nothing leaves a stolen session running after the
// person has done the one thing they know to do about it; revoking everything
// signs them out of the browser they are looking at, which reads as the change
// having failed.
func TestChangingAPasswordEndsOtherSessionsButNotThisOne(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "revokes")
	if err := s.SetPassword(ctx, sess, "the first password", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	other, err := s.CreateSession(ctx, u.ID, "team-revokes", "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := s.SetPassword(ctx, sess, "the second password", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if _, err := s.ResolveSession(ctx, other); !errors.Is(err, ErrSessionInvalid) {
		t.Error("another session survived a password change")
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE id = $1`, sess).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Error("the session that made the change was revoked along with the others")
	}
}

// Removing a password ends other sessions too, for the same reason.
func TestRemovingAPasswordEndsOtherSessions(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "removes")
	if err := s.SetPassword(ctx, sess, "a password to remove", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	other, err := s.CreateSession(ctx, u.ID, "team-removes", "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := s.RemovePassword(ctx, sess, Proof{}); err != nil {
		t.Fatalf("RemovePassword: %v", err)
	}
	if _, err := s.ResolveSession(ctx, other); !errors.Is(err, ErrSessionInvalid) {
		t.Error("another session survived a password removal")
	}
}

// Removing a password that is not there is distinguishable from success.
//
// Reporting a credential withdrawn when none existed is how somebody stops
// looking for the one that is still there.
func TestRemovingAPasswordNobodySetSaysSo(t *testing.T) {
	s := testService(t)

	_, sess := signedIn(t, s, "nothing")

	if err := s.RemovePassword(context.Background(), sess, Proof{}); !errors.Is(
		err, ErrNoPassword) {
		t.Fatalf("RemovePassword = %v, want ErrNoPassword", err)
	}
}

// After a removal the account has no password at all, and the magic link is
// still the way in.
//
// The lock-out this prevents is the reason removal is allowed in the first
// place: it is only safe because the other method never stopped working.
func TestRemovingAPasswordLeavesTheAccountReachable(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "reachable")
	if err := s.SetPassword(ctx, sess, "a password to remove", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := s.RemovePassword(ctx, sess, Proof{}); err != nil {
		t.Fatalf("RemovePassword: %v", err)
	}

	if _, err := s.AuthenticatePassword(ctx, u.Email, "a password to remove"); !errors.Is(
		err, ErrCredentialInvalid) {
		t.Error("the removed password still authenticates")
	}
	has, err := s.HasPassword(ctx, u.ID)
	if err != nil {
		t.Fatalf("HasPassword: %v", err)
	}
	if has {
		t.Error("HasPassword still reports one after removal")
	}

	raw, _, existed, err := s.IssueMagicLink(ctx, u.Email, time.Hour)
	if err != nil || !existed {
		t.Fatalf("IssueMagicLink after removal: existed=%v err=%v", existed, err)
	}
	if _, err := s.ConsumeMagicLink(ctx, raw); err != nil {
		t.Errorf("the magic link stopped working after a password was removed: %v", err)
	}
}

// An unknown address, a known address with no password, and a wrong password are
// one error.
//
// A caller able to tell them apart is a user-list disclosure, and the handler
// above this cannot invent a distinction the service does not hand it.
func TestEveryFailedPasswordSignInIsTheSameError(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	withPassword, sess := signedIn(t, s, "known")
	if err := s.SetPassword(ctx, sess, "the real password", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	noPassword, _ := signedIn(t, s, "passwordless")

	for name, addr := range map[string]string{
		"an address nobody holds":     "nobody@example.test",
		"an address with no password": noPassword.Email,
		"the wrong password":          withPassword.Email,
	} {
		_, err := s.AuthenticatePassword(ctx, addr, "not the real password")
		if !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("%s: got %v, want ErrCredentialInvalid", name, err)
		}
	}
}

// Refusing an address nobody holds must cost what refusing a real one costs.
//
// The dummy hash is the only thing making that true, and it is exactly the kind
// of code somebody deletes later as obviously redundant. Medians over a handful
// of samples with a wide tolerance: the point is to catch an early return, not
// to measure nanoseconds.
func TestRefusingAnUnknownAddressCostsTheSameAsARealOne(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	known, sess := signedIn(t, s, "timed")
	if err := s.SetPassword(ctx, sess, "the real password", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// Warm the dummy hash, which is computed once and would otherwise land
	// entirely in whichever sample happened to come first.
	_, _ = s.AuthenticatePassword(ctx, "warm@example.test", "x")

	measure := func(addr string) time.Duration {
		var samples []time.Duration
		for range 5 {
			start := time.Now()
			if _, err := s.AuthenticatePassword(ctx, addr, "not the real password"); err == nil {
				t.Fatal("a wrong password authenticated")
			}
			samples = append(samples, time.Since(start))
		}
		slices := samples
		for i := range slices {
			for j := i + 1; j < len(slices); j++ {
				if slices[j] < slices[i] {
					slices[i], slices[j] = slices[j], slices[i]
				}
			}
		}
		return slices[len(slices)/2]
	}

	unknown := measure("nobody-timed@example.test")
	real := measure(known.Email)

	ratio := float64(unknown) / float64(real)
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("refusing an unknown address took %v against %v for a known one "+
			"(ratio %.2f) — the two paths are no longer doing the same work",
			unknown, real, ratio)
	}
}

// A password stored at an older cost is rewritten after a successful sign-in.
//
// Without this, raising the parameters only ever applies to passwords set after
// the change, and everybody who already had one keeps the old cost forever.
func TestSigningInRehashesAPasswordStoredAtAnOlderCost(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "rehash")
	if err := s.SetPassword(ctx, sess, "a password to rehash", Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// Rewrite the stored hash at a cheaper cost, standing in for a row written
	// before the parameters were raised.
	old := passwordParams{memory: 32 * 1024, passes: 2, parallelism: 1, saltLen: 16, keyLen: 32}
	salt := []byte("fedcba9876543210")
	stale := encodePassword(old, salt, derive("a password to rehash", salt, old))
	if _, err := s.pool.Exec(ctx,
		`UPDATE user_credentials SET secret = $1 WHERE user_id = $2`, stale, u.ID); err != nil {
		t.Fatalf("plant an old hash: %v", err)
	}

	if _, err := s.AuthenticatePassword(ctx, u.Email, "a password to rehash"); err != nil {
		t.Fatalf("a password at the old cost did not authenticate: %v", err)
	}

	got := storedSecret(t, s, u.ID)
	if got == stale {
		t.Error("the stored hash was not rewritten at the current cost")
	}
	if _, err := s.AuthenticatePassword(ctx, u.Email, "a password to rehash"); err != nil {
		t.Errorf("the rehashed password stopped authenticating: %v", err)
	}
}

// A session that has expired cannot change how the account is signed in to.
//
// GetSession has no expiry filter, so this is the property that would quietly
// disappear if the credential path ever reached for that query instead.
func TestAnExpiredSessionCannotSetAPassword(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "expired")
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		sess); err != nil {
		t.Fatalf("expire session: %v", err)
	}

	if err := s.SetPassword(ctx, sess, "a password after expiry", Proof{}); !errors.Is(
		err, ErrSessionInvalid) {
		t.Fatalf("SetPassword = %v, want ErrSessionInvalid", err)
	}
	if secret := storedSecret(t, s, u.ID); secret != "" {
		t.Error("an expired session wrote a password")
	}
}

// The policy is enforced by the service, so no caller can route around it.
func TestSetPasswordAppliesThePolicy(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "policy")

	if err := s.SetPassword(ctx, sess, "short", Proof{}); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("SetPassword with a short one = %v, want ErrPasswordTooShort", err)
	}
	// The address is read from the session's own user, so supplying a different
	// one in the request cannot dodge this.
	if err := s.SetPassword(ctx, sess, u.Email, Proof{}); !errors.Is(err, ErrPasswordIsEmail) {
		t.Errorf("SetPassword with their address = %v, want ErrPasswordIsEmail", err)
	}
	if secret := storedSecret(t, s, u.ID); secret != "" {
		t.Error("a password refused by the policy was written anyway")
	}
}

// The stored form is a hash, and the password itself is nowhere in the database.
func TestThePasswordIsNeverStored(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, sess := signedIn(t, s, "storage")
	const plain = "a password worth not storing"
	if err := s.SetPassword(ctx, sess, plain, Proof{}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM user_credentials WHERE user_id = $1 AND secret = $2`,
		u.ID, plain).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatal("the password is stored in plain text")
	}
	if got := storedSecret(t, s, u.ID); !hasArgonPrefix(got) {
		t.Errorf("the stored secret is not an argon2id hash: %q", got)
	}
}

func hasArgonPrefix(s string) bool {
	const want = "$argon2id$"
	return len(s) >= len(want) && s[:len(want)] == want
}
