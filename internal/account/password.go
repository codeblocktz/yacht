package account

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// ErrPasswordTooShort and ErrPasswordTooLong are two of the three things this
// package will refuse a password for.
//
// There are no composition rules, deliberately. Every rule is a constraint an
// attacker subtracts from the search space, and in practice they push people
// towards a shorter password with a digit on the end. Length is the only lever
// that is monotonically good, which is why it is the only one pulled.
var (
	ErrPasswordTooShort = errors.New("account: a password must be at least 12 characters")
	ErrPasswordTooLong  = errors.New("account: that password is too long")
)

// ErrPasswordIsEmail is the third. An address is the one string an attacker
// guessing this account already has in front of them.
var ErrPasswordIsEmail = errors.New("account: a password cannot be your email address")

const (
	// MinPasswordLength is counted in runes rather than bytes so that a
	// passphrase in a non-Latin script is not held to a shorter limit than the
	// same number of characters in ASCII.
	//
	// Exported so that the form can say the number and the browser can enforce
	// it before a round trip does. The rule itself stays here — a minlength
	// attribute is a courtesy, not a check.
	MinPasswordLength = 12

	// maxPasswordBytes bounds the input to the hash. Argon2 has no bcrypt-style
	// truncation, so without a ceiling the work done per attempt is a number the
	// submitter chooses.
	maxPasswordBytes = 128
)

// passwordParams is one Argon2id cost setting.
//
// Carried as a value rather than as loose constants so that a stored hash can be
// compared against the current cost without either of them being spelled twice.
type passwordParams struct {
	memory      uint32 // KiB
	passes      uint32 // the t parameter
	parallelism uint8  // lanes
	saltLen     uint32
	keyLen      uint32
}

// currentParams is what new passwords are hashed at today.
//
// RFC 9106's second recommended profile is m=64MiB, t=3, p=4. Yacht is a
// self-hosted box that is usually also running Postgres and a k3s control plane,
// so the profile is met where it matters and trimmed where it does not:
//
//   - m=64MiB is the largest value still polite on a 2GB box. Memory is the
//     parameter that actually buys resistance to a GPU, so it is the last thing
//     to cut.
//   - t=3 is RFC 9106's pairing with that memory, and lands around 60-90ms.
//   - p=2 rather than 4 because argon2 runs lanes as goroutines and the common
//     install has two vCPUs. Four lanes there costs the same memory bandwidth
//     for no reduction in wall clock, and p is not a security parameter at
//     fixed m and t.
//   - saltLen=16 is 128 bits, RFC 9106's recommendation; more buys nothing
//     against a per-row random salt.
//   - keyLen=32 is 256 bits, matching HashToken's output width so that the
//     storage rule reads the same either side of this file.
//
// Raising any of these is a rehash on next login rather than a reset for
// everybody. See verifyPassword's stale return.
var currentParams = passwordParams{
	memory:      64 * 1024,
	passes:      3,
	parallelism: 2,
	saltLen:     16,
	keyLen:      32,
}

// maxConcurrentHashes bounds how much memory password hashing may hold at once.
//
// 64MiB per hash multiplied by the twenty concurrent attempts the per-IP rate
// limit permits is 1.28GiB, which on the hardware this runs on arrives as
// "Yacht died" rather than as "signing in is slow". Requests past this wait
// instead of allocating.
//
// The wait is a timing signal about how busy the server is, and not about any
// account, so it does not weaken the equal-cost verify in AuthenticatePassword.
const maxConcurrentHashes = 4

var hashSlots = make(chan struct{}, maxConcurrentHashes)

// errHashMalformed is returned for a stored hash that cannot be read.
//
// Never surfaced. The service logs it and answers ErrCredentialInvalid, because
// a corrupted row must not be distinguishable from a wrong password by anybody
// standing outside.
var errHashMalformed = errors.New("account: stored password hash is malformed")

// hashPassword returns the stored form of a password a person chose.
//
// Argon2id, which is the exact inverse of the rule stated on HashToken in
// token.go — deliberately, because the two inputs are opposites. A session
// token, a magic link and an invitation are 32 bytes this process drew from
// crypto/rand: there is nothing to guess, so a slow KDF would add latency to
// every request carrying a cookie and buy nothing at all. A password is a short
// string a human chose and probably reused somewhere, so guessing it is the
// entire attack, and making each guess expensive is the only defence there is.
// Cheap hashing is correct for one and negligent for the other, and what tells
// them apart is not how sensitive the value is but where its entropy came from.
//
// The parameters and a fresh random salt are encoded into the returned string,
// so raising the cost later is a rehash on next login rather than a forced reset
// for everybody.
func hashPassword(plain string) (string, error) {
	salt := make([]byte, currentParams.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("account: generate salt: %w", err)
	}
	return encodePassword(currentParams, salt, derive(plain, salt, currentParams)), nil
}

// verifyPassword reports whether plain produced encoded, and whether the stored
// hash was made at a cost this build has since moved on from.
//
// The comparison is recomputed with the parameters found in the stored string
// rather than with the current ones, which is what lets an old hash keep working
// after the cost is raised.
func verifyPassword(encoded, plain string) (ok, stale bool, err error) {
	p, salt, want, err := decodePassword(encoded)
	if err != nil {
		return false, false, err
	}
	got := derive(plain, salt, p)
	// Constant time, although the realistic risk is low: an attacker able to
	// time this would need a candidate hash rather than a candidate password.
	// It costs nothing and it is the shape a reviewer expects to find.
	return subtle.ConstantTimeCompare(want, got) == 1, p != currentParams, nil
}

// derive runs the KDF, holding one of the bounded hashing slots for the
// duration.
func derive(plain string, salt []byte, p passwordParams) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey([]byte(plain), salt, p.passes, p.memory, p.parallelism, p.keyLen)
}

// encodePassword writes the PHC string that goes in the database:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
func encodePassword(p passwordParams, salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.passes, p.parallelism,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decodePassword reads a PHC string back.
//
// Every failure is errHashMalformed rather than a description of what was wrong
// with it. The caller cannot act on the difference, and the one thing that must
// never happen here is an error a branch somewhere reads as "skip the check".
func decodePassword(encoded string) (p passwordParams, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errHashMalformed
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errHashMalformed
	}
	if _, err := fmt.Sscanf(
		parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.passes, &p.parallelism,
	); err != nil {
		return p, nil, nil, errHashMalformed
	}

	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil {
		return p, nil, nil, errHashMalformed
	}
	if key, err = b64.DecodeString(parts[5]); err != nil {
		return p, nil, nil, errHashMalformed
	}
	// Taken from what is actually stored rather than from the header, so that a
	// hash written at a different width still verifies and still reports itself
	// stale against the current setting.
	p.saltLen, p.keyLen = uint32(len(salt)), uint32(len(key))

	if p.memory == 0 || p.passes == 0 || p.parallelism == 0 || len(salt) == 0 || len(key) == 0 {
		return p, nil, nil, errHashMalformed
	}
	return p, salt, key, nil
}

// checkPasswordPolicy is every rule this package applies to a chosen password.
//
// Nothing is trimmed and nothing is normalised. Trimming would silently change
// what somebody typed, and a leading space is a legitimate character. Unicode
// normalisation is a harder omission and is a decision rather than an oversight:
// it would need golang.org/x/text, and adding it later would stop every
// non-ASCII password set before that day from verifying, with no way to tell
// the difference between that and a wrong password. Anybody tempted to tidy
// this up later should read this paragraph first.
func checkPasswordPolicy(plain, email string) error {
	if utf8.RuneCountInString(plain) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(plain) > maxPasswordBytes {
		return ErrPasswordTooLong
	}
	if strings.EqualFold(strings.TrimSpace(email), plain) {
		return ErrPasswordIsEmail
	}
	return nil
}

// dummyHash is what a password is verified against when there is nothing to
// verify it against.
//
// "No such address", "that address exists but has no password" and "that
// password is wrong" have to be one answer and, just as importantly, one cost.
// Returning early on either of the first two would make an unregistered address
// the fast one, and a stopwatch would then enumerate the user table however
// carefully the response was worded.
//
// The plaintext is drawn from crypto/rand and thrown away, so no input can match
// it. It is hashed at currentParams, so raising the cost keeps the two paths
// equal without anybody having to remember to update this.
//
// Computed lazily rather than at NewService, which would put ~80ms on every
// process start including every test binary. The first sign-in after a boot pays
// it once, and a timing sample nobody can take twice is not a sample.
var dummyHash = sync.OnceValue(func() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Unreachable in practice, and a panic here would be a way to take the
		// process down from the sign-in form. A fixed string is still a hash no
		// input matches, which is the only property this value needs.
		b = []byte("yacht: unreachable dummy password material")
	}
	encoded, err := hashPassword(string(b))
	if err != nil {
		return "$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$" +
			"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}
	return encoded
})
