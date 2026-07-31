// Package account owns people: users, the teams they belong to, the role they
// hold in each, and the sessions and invitations that get them there.
package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// tokenBytes is the entropy behind every credential this package issues.
// 32 bytes is beyond brute force and costs nothing.
const tokenBytes = 32

// NewToken returns a fresh credential and the hash to store for it.
//
// One implementation shared by sessions, magic links and invitations, so all
// three get the same entropy and the same storage rule rather than three
// chances to get it wrong.
//
// The caller sends raw to the person and stores hash. Never the reverse: a
// database dump must not be a set of working credentials.
func NewToken() (raw string, hash []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("account: generate token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashToken(raw), nil
}

// HashToken returns the stored form of a token.
//
// Plain SHA-256, deliberately: unlike a password this is full-entropy random,
// so there is nothing to brute-force and a slow KDF would only add latency to
// every request that carries a session cookie.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
