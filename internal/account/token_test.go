package account

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestNewTokenIsUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		raw, hash, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[raw] {
			t.Fatal("NewToken returned a duplicate")
		}
		seen[raw] = true

		// Enough entropy that guessing is not a strategy.
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			t.Fatalf("token is not URL-safe base64: %v", err)
		}
		if len(decoded) < 32 {
			t.Fatalf("token entropy = %d bytes, want at least 32", len(decoded))
		}
		if len(hash) != 32 {
			t.Fatalf("hash = %d bytes, want 32 (SHA-256)", len(hash))
		}
	}
}

func TestHashTokenMatchesNewToken(t *testing.T) {
	raw, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !bytes.Equal(HashToken(raw), hash) {
		t.Fatal("HashToken does not reproduce the hash NewToken returned")
	}
}

// The raw token must never be derivable from what is stored.
func TestHashIsNotTheToken(t *testing.T) {
	raw, hash, _ := NewToken()
	if bytes.Contains(hash, []byte(raw)) {
		t.Fatal("the stored hash contains the token")
	}
}
