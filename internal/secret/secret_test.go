package secret

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func testKeeper(t *testing.T) *Keeper {
	t.Helper()
	k, err := NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	return k
}

func TestSealAndOpenRoundTrip(t *testing.T) {
	k := testKeeper(t)

	for _, plain := range []string{"", "hunter2", strings.Repeat("x", 4096), "üñïçø∂é"} {
		sealed, err := k.Seal(plain)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		got, err := k.Open(sealed)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got != plain {
			t.Fatalf("round trip = %q, want %q", got, plain)
		}
	}
}

// The stored form must not contain the value. That is the whole point: a
// database dump has to be useless without the key.
func TestSealedBytesDoNotContainThePlaintext(t *testing.T) {
	k := testKeeper(t)

	const plain = "postgres://user:hunter2@db.internal/prod"
	sealed, err := k.Seal(plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte(plain)) {
		t.Fatal("the sealed value contains the plaintext")
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("the sealed value contains the password")
	}
}

// Sealing the same value twice must not produce the same bytes. Otherwise a
// dump reveals which apps share a secret, and which ones changed.
func TestSealIsNotDeterministic(t *testing.T) {
	k := testKeeper(t)

	a, _ := k.Seal("same")
	b, _ := k.Seal("same")
	if bytes.Equal(a, b) {
		t.Fatal("sealing the same value twice produced identical bytes")
	}
}

// A tampered value must fail rather than decrypt to something else.
func TestOpenRejectsTampering(t *testing.T) {
	k := testKeeper(t)

	sealed, _ := k.Seal("hunter2")
	sealed[len(sealed)-1] ^= 0xff

	if _, err := k.Open(sealed); err == nil {
		t.Fatal("Open accepted a value whose ciphertext was altered")
	}
}

// The wrong key must fail, not return rubbish.
func TestOpenWithTheWrongKeyFails(t *testing.T) {
	k := testKeeper(t)
	other, err := NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("j"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}

	sealed, _ := k.Seal("hunter2")
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("a value sealed with one key opened with another")
	}
}

func TestNewKeeperRejectsWeakKeys(t *testing.T) {
	for name, key := range map[string]string{
		"empty":      "",
		"not base64": "!!!!not base64!!!!",
		"too short":  base64.StdEncoding.EncodeToString([]byte("short")),
		"16 bytes":   base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 16)),
		"all zeroes": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewKeeper(key); err == nil {
				t.Fatalf("NewKeeper accepted %s", name)
			}
		})
	}
}

// Without a key configured there is no keeper, and the caller has to say so
// rather than quietly storing plaintext.
func TestNilKeeperRefusesToSeal(t *testing.T) {
	var k *Keeper
	if _, err := k.Seal("hunter2"); err == nil {
		t.Fatal("a nil keeper sealed a value")
	}
	if !k.Configured() {
		return
	}
	t.Fatal("a nil keeper reported itself configured")
}
