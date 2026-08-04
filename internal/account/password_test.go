package account

import (
	"errors"
	"strings"
	"testing"
)

// None of these needs a database, which is deliberate. CONTRIBUTING warns that a
// skip looks like a pass, and the properties asserted here — that a wrong
// password is refused, that a malformed hash is never read as a match — are the
// ones it would be worst to have silently unchecked on a laptop without
// Postgres.

// A password that was stored must verify, and one that was not must not.
//
// The failure this prevents is the whole feature being decorative: an encode
// and a decode that disagree, or a comparison that answers true for everything.
func TestStoredPasswordVerifiesAndAWrongOneDoesNot(t *testing.T) {
	const plain = "correct horse battery staple"

	encoded, err := hashPassword(plain)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Errorf("stored form is not PHC-encoded argon2id: %q", encoded)
	}

	ok, stale, err := verifyPassword(encoded, plain)
	if err != nil || !ok {
		t.Errorf("the stored password did not verify: ok=%v err=%v", ok, err)
	}
	if stale {
		t.Error("a hash just written at the current parameters reports itself stale")
	}

	ok, _, err = verifyPassword(encoded, plain+" ")
	if err != nil {
		t.Fatalf("verify a wrong password: %v", err)
	}
	if ok {
		t.Error("a password that differs by one trailing space was accepted")
	}
}

// Two people who choose the same password must not share a stored hash.
//
// Without a per-row random salt, one cracked hash is every account that chose
// that password, and equal hashes are visible in a database dump without any
// cracking at all.
func TestTheSamePasswordStoresDifferentlyEveryTime(t *testing.T) {
	const plain = "the same password twice"

	first, err := hashPassword(plain)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	second, err := hashPassword(plain)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if first == second {
		t.Error("the same password produced the same stored hash, so the salt is not random")
	}
	for _, encoded := range []string{first, second} {
		if ok, _, err := verifyPassword(encoded, plain); err != nil || !ok {
			t.Errorf("a salted hash did not verify: ok=%v err=%v", ok, err)
		}
	}
}

// A hash written at an older cost keeps working, and says it is out of date.
//
// This is what makes raising the parameters a rehash on next login rather than a
// reset for everybody. If the old hash stopped verifying, every existing
// password would break the day the cost changed.
func TestAHashMadeAtAnOlderCostStillVerifiesAndReportsItselfStale(t *testing.T) {
	const plain = "a password from before the bump"

	old := passwordParams{memory: 32 * 1024, passes: 2, parallelism: 1, saltLen: 16, keyLen: 32}
	salt := []byte("0123456789abcdef")
	encoded := encodePassword(old, salt, derive(plain, salt, old))

	ok, stale, err := verifyPassword(encoded, plain)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Error("a password stored at the old cost stopped verifying, which would lock everybody out")
	}
	if !stale {
		t.Error("a hash at the old cost did not report itself stale, so it would never be rehashed")
	}
}

// A stored hash that cannot be read must never be read as a match.
//
// The one thing that must not happen is an error somewhere being treated as
// "skip the check". Every one of these has to answer false, and answering an
// error as well is not enough on its own.
func TestAMalformedHashIsNeverAMatch(t *testing.T) {
	good, err := hashPassword("a perfectly ordinary password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	for name, encoded := range map[string]string{
		"empty":            "",
		"not phc at all":   "hunter2",
		"too few fields":   "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA",
		"too many fields":  good + "$extra",
		"wrong algorithm":  strings.Replace(good, "argon2id", "argon2i", 1),
		"wrong version":    strings.Replace(good, "v=19", "v=16", 1),
		"unparseable cost": strings.Replace(good, "m=65536", "m=lots", 1),
		"zero memory":      strings.Replace(good, "m=65536", "m=0", 1),
		"bad base64 salt":  "$argon2id$v=19$m=65536,t=3,p=2$not!base64$" + strings.Split(good, "$")[5],
		"empty salt":       "$argon2id$v=19$m=65536,t=3,p=2$$" + strings.Split(good, "$")[5],
	} {
		ok, _, err := verifyPassword(encoded, "a perfectly ordinary password")
		if ok {
			t.Errorf("%s: a malformed stored hash was accepted as a match", name)
		}
		if !errors.Is(err, errHashMalformed) {
			t.Errorf("%s: want errHashMalformed, got %v", name, err)
		}
	}
}

// The dummy hash exists so that an address nobody holds costs the same to refuse
// as one that is registered.
//
// It must be readable — a malformed one would send the unknown-address path down
// a different, faster branch and reopen the timing gap it was written to close —
// and nothing must verify against it.
func TestTheDummyHashIsUsableAndMatchesNothing(t *testing.T) {
	encoded := dummyHash()

	if _, _, err := verifyPassword(encoded, "anything at all"); err != nil {
		t.Fatalf("the dummy hash does not parse, so the unknown-address path is cheaper: %v", err)
	}
	for _, guess := range []string{"", "password", "anything at all", encoded} {
		if ok, _, _ := verifyPassword(encoded, guess); ok {
			t.Errorf("something verified against the dummy hash: %q", guess)
		}
	}
	if dummyHash() != encoded {
		t.Error("the dummy hash is recomputed per call, which is work on every failed sign-in")
	}
}

// The policy is a length floor, a length ceiling and one special case. This
// pins all three, and pins the absence of everything else.
func TestPasswordPolicyRejectsWhatItSaysAndNothingMore(t *testing.T) {
	const email = "person@example.test"

	for name, tc := range map[string]struct {
		plain string
		want  error
	}{
		"eleven runes is short":     {strings.Repeat("a", 11), ErrPasswordTooShort},
		"twelve runes is enough":    {strings.Repeat("a", 12), nil},
		"empty":                     {"", ErrPasswordTooShort},
		"one over the byte ceiling": {strings.Repeat("a", maxPasswordBytes+1), ErrPasswordTooLong},
		"exactly the ceiling":       {strings.Repeat("a", maxPasswordBytes), nil},
		"their own address":         {email, ErrPasswordIsEmail},
		"their address in caps":     {strings.ToUpper(email), ErrPasswordIsEmail},

		// Counted in runes, so a passphrase in a non-Latin script is not held to
		// a shorter limit than the same number of ASCII characters would be.
		"twelve non-ascii runes": {"пароль-длинный", nil},

		// Nothing is trimmed and there are no composition rules. A password of
		// nothing but lowercase letters and spaces is a good password if it is
		// long, and a leading space is a character somebody meant to type.
		"spaces and lowercase only": {"a b c d e f g", nil},
		"leading space kept":        {" leading space is fine", nil},
		"no digit or symbol needed": {"alllowercaseletters", nil},
	} {
		if got := checkPasswordPolicy(tc.plain, email); !errors.Is(got, tc.want) {
			t.Errorf("%s: checkPasswordPolicy(%q) = %v, want %v", name, tc.plain, got, tc.want)
		}
	}
}

// A password at the byte ceiling must still be hashable.
//
// Argon2 does not truncate the way bcrypt does, so the ceiling is what bounds
// the work an attacker can ask for. It is worth proving the boundary itself
// round-trips rather than assuming it.
func TestAPasswordAtTheCeilingStillRoundTrips(t *testing.T) {
	plain := strings.Repeat("x", maxPasswordBytes)

	encoded, err := hashPassword(plain)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if ok, _, err := verifyPassword(encoded, plain); err != nil || !ok {
		t.Errorf("a password at the ceiling did not verify: ok=%v err=%v", ok, err)
	}
}
