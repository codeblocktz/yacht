// Package secret keeps values that must survive a database dump.
//
// The engine stores what it needs to re-apply a workload, and some of that is
// a password somebody pasted in. Stored plainly it is in Postgres, in every
// backup, and in whatever the backup was copied to — none of which anybody
// thinks about at the moment they paste it.
//
// So values are sealed here and only opened to apply. A dump is then useless
// without the key, which lives in configuration rather than in the database it
// protects.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// keyBytes is the AES-256 key size. Nothing shorter is accepted: a 128-bit key
// is fine cryptographically, but accepting two lengths means one of them is
// chosen by accident.
const keyBytes = 32

// Keeper seals and opens values with one key.
type Keeper struct {
	aead cipher.AEAD
}

// NewKeeper builds a Keeper from a base64-encoded 32-byte key.
//
// Generate one with:
//
//	openssl rand -base64 32
func NewKeeper(encodedKey string) (*Keeper, error) {
	if encodedKey == "" {
		return nil, errors.New("secret: no key configured")
	}

	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("secret: key is not base64: %w", err)
	}
	if len(key) != keyBytes {
		return nil, fmt.Errorf("secret: key is %d bytes, want %d — generate one with `openssl rand -base64 32`",
			len(key), keyBytes)
	}

	// An all-zero key is what a caller gets from an unset variable that was
	// decoded anyway, or from a placeholder nobody replaced. It encrypts
	// perfectly well, which is exactly why it has to be refused here.
	var nonzero byte
	for _, b := range key {
		nonzero |= b
	}
	if nonzero == 0 {
		return nil, errors.New("secret: key is all zeroes")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return &Keeper{aead: aead}, nil
}

// Configured reports whether values can be sealed.
//
// A nil Keeper is the shape of "no key set", so callers can hold one without a
// separate flag and this stays safe to call.
func (k *Keeper) Configured() bool { return k != nil && k.aead != nil }

// Seal encrypts a value for storage.
//
// The nonce is random per call and stored ahead of the ciphertext, so sealing
// one value twice gives different bytes. Deterministic output would let anyone
// reading a dump tell which apps share a secret and which ones changed —
// without opening any of them.
func (k *Keeper) Seal(plain string) ([]byte, error) {
	if !k.Configured() {
		return nil, errors.New("secret: no key configured, so nothing can be stored safely")
	}

	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return k.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Open decrypts a stored value.
//
// GCM authenticates as well as encrypts, so a value altered in the database
// fails here rather than decrypting to something else.
func (k *Keeper) Open(sealed []byte) (string, error) {
	if !k.Configured() {
		return "", errors.New("secret: no key configured")
	}
	if len(sealed) < k.aead.NonceSize() {
		return "", errors.New("secret: stored value is too short to be valid")
	}

	nonce, ciphertext := sealed[:k.aead.NonceSize()], sealed[k.aead.NonceSize():]
	plain, err := k.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Deliberately not wrapped: the underlying message says only
		// "authentication failed", and dressing it up as something more
		// specific would be guessing at which of the causes it was.
		return "", errors.New("secret: could not open the stored value — " +
			"either it was altered, or YACHT_SECRET_KEY is not the key it was sealed with")
	}
	return string(plain), nil
}
