package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// The raw keys these tests build keepers from. Held as bytes as well as
// base64, because the legacy writer below needs the key itself.
var (
	keyA = bytes.Repeat([]byte("k"), 32)
	keyB = bytes.Repeat([]byte("j"), 32)
	keyC = bytes.Repeat([]byte("m"), 32)
)

func encode(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

func testKeeper(t *testing.T) *Keeper {
	t.Helper()
	return keeperFor(t, keyA)
}

func keeperFor(t *testing.T, active []byte, previous ...[]byte) *Keeper {
	t.Helper()
	encoded := make([]string, 0, len(previous))
	for _, p := range previous {
		encoded = append(encoded, encode(p))
	}
	k, err := NewKeeper(encode(active), encoded...)
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	return k
}

// sealLegacy writes a value the way this package did before the envelope
// carried a version: a bare nonce followed by the ciphertext, no header, and
// nothing authenticated alongside it.
//
// Built from crypto/aes here rather than by calling Seal, because bytes
// produced by today's writer prove nothing about the bytes already sitting in
// somebody's database. This is the format the upgrade has to keep reading.
//
// nonce may be supplied to pin the coincidence a test is about; nil means a
// random one, as the old writer used.
func sealLegacy(t *testing.T, key []byte, plain string, nonce []byte) []byte {
	t.Helper()

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	if nonce == nil {
		nonce = make([]byte, gcm.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			t.Fatalf("nonce: %v", err)
		}
	}
	if len(nonce) != gcm.NonceSize() {
		t.Fatalf("nonce is %d bytes, want %d", len(nonce), gcm.NonceSize())
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil)
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
	other := keeperFor(t, keyB)

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
	if _, err := k.Open([]byte("anything at all, long enough to be a value")); err == nil {
		t.Fatal("a nil keeper opened a value")
	}
	if k.ActiveKeyID() != "" || k.KeyIDs() != nil {
		t.Fatal("a nil keeper named a key")
	}
	if !k.Configured() {
		return
	}
	t.Fatal("a nil keeper reported itself configured")
}

// The header is the format contract. Anything stored from now on begins with
// it, and a later reader — a backup, a restore, a rotation job — is entitled
// to rely on that.
func TestSealedValueCarriesTheVersionAndTheKeyID(t *testing.T) {
	k := testKeeper(t)

	sealed, err := k.Seal("hunter2")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got := string(sealed[:len(magic)]); got != magic {
		t.Errorf("envelope begins %q, want %q", got, magic)
	}
	if sealed[len(magic)] != version1 {
		t.Errorf("version byte = %d, want %d", sealed[len(magic)], version1)
	}
	if got := hex.EncodeToString(sealed[len(magic)+1 : headerBytes]); got != k.ActiveKeyID() {
		t.Errorf("envelope names key %s, want the active key %s", got, k.ActiveKeyID())
	}
}

// The ID has to come from the key, not from the order keys were listed in or
// anything else about this process. A rotation job on another machine reads
// the same bytes and must reach the same name for them.
func TestKeyIDIsDerivedFromTheKey(t *testing.T) {
	first := keeperFor(t, keyA)
	again := keeperFor(t, keyA, keyB)
	if first.ActiveKeyID() != again.ActiveKeyID() {
		t.Errorf("the same key was named %s and %s", first.ActiveKeyID(), again.ActiveKeyID())
	}
	if keeperFor(t, keyB).ActiveKeyID() == first.ActiveKeyID() {
		t.Error("two different keys were given the same ID")
	}
}

// Values written before this change are the whole reason the format has a
// fallback. These bytes are built by the old writer, not by Seal.
func TestOpenReadsCiphertextWrittenInTheOldFormat(t *testing.T) {
	k := testKeeper(t)

	for _, plain := range []string{"", "hunter2", strings.Repeat("x", 4096), "üñïçø∂é"} {
		got, err := k.Open(sealLegacy(t, keyA, plain, nil))
		if err != nil {
			t.Fatalf("Open of a legacy value: %v", err)
		}
		if got != plain {
			t.Fatalf("legacy round trip = %q, want %q", got, plain)
		}
	}
}

// The coincidence the format is built around: a legacy nonce is twelve random
// bytes, so one in roughly four billion of them begins with the magic and a
// version byte. If the prefix decided the format, that value would be refused
// with the key that opens it sitting right there.
//
// Three shapes are covered: a fake key ID this install does not hold, one that
// happens to be the active key's — which gets past the ID lookup and is caught
// only by authentication failing — and a version byte no build reads, which is
// the likeliest of the three at one in sixteen million, because it needs only
// the three magic bytes to land rather than four.
func TestOpenReadsALegacyValueThatLooksVersioned(t *testing.T) {
	k := testKeeper(t)

	// Long enough that the versioned reading has a full nonce and tag to chew
	// on, which is what makes it a real attempt rather than a length check.
	const plain = "postgres://user:hunter2@db.internal/prod?sslmode=require"

	unheld := append([]byte(magic), version1)
	unheld = append(unheld, []byte{0xde, 0xad, 0xbe, 0xef, 0xde, 0xad, 0xbe, 0xef}...)

	held := append([]byte(magic), version1)
	held = append(held, k.active.id[:]...)

	// The same coincidence against the future-version path, which needs only
	// the three magic bytes to match. A legacy value must not be refused as
	// "written by a later Yacht" on the strength of a random fourth byte.
	future := append([]byte(magic), version1+1)
	future = append(future, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}...)

	for name, nonce := range map[string][]byte{
		"naming a key nobody holds":  unheld,
		"naming the active key":      held,
		"naming a version we cannot": future,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := k.Open(sealLegacy(t, keyA, plain, nonce))
			if err != nil {
				t.Fatalf("a legacy value whose nonce looks like a header was refused: %v", err)
			}
			if got != plain {
				t.Fatalf("Open = %q, want %q", got, plain)
			}
		})
	}
}

// A value from a later Yacht must say so rather than be reported as an old
// value or a missing key.
//
// The operator response to each is different and only one of them is
// recoverable by acting on the key: told the value was altered, they set it
// again and lose what was there; told the format is newer, they put the newer
// binary back and it opens. The failure is still decided by authentication —
// the legacy reading is attempted first and this is only how the failure that
// already happened gets worded.
func TestOpenSaysWhenTheEnvelopeIsNewerThanThisBuild(t *testing.T) {
	k := testKeeper(t)

	sealed, err := k.Seal("hunter2")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	fromTheFuture := append([]byte(nil), sealed...)
	fromTheFuture[len(magic)] = version1 + 1

	_, err = k.Open(fromTheFuture)
	if !errors.Is(err, ErrFutureVersion) {
		t.Fatalf("Open of a newer envelope = %v, want ErrFutureVersion", err)
	}
	// Reporting it as either of the other two would send the operator somewhere
	// that cannot help, and one of those places loses the value.
	if errors.Is(err, ErrAltered) || errors.Is(err, ErrUnknownKey) {
		t.Errorf("a newer envelope was also reported as altered or key-missing: %v", err)
	}
	// Both honest causes, because three matching magic bytes is all it took to
	// get here and the bytes could equally have been damaged.
	for _, want := range []string{"version 2", "later Yacht", "altered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// The transition this format exists for: a new key becomes active, the old one
// is kept, and nothing stored under the old one stops working.
func TestValuesOpenUnderARetiredKey(t *testing.T) {
	a := keeperFor(t, keyA)

	versioned, err := a.Seal("sealed-by-a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	legacy := sealLegacy(t, keyA, "written-before-the-envelope", nil)

	// B active, A retained.
	rotated := keeperFor(t, keyB, keyA)
	for name, tc := range map[string]struct {
		sealed []byte
		want   string
	}{
		"versioned under A": {versioned, "sealed-by-a"},
		"legacy under A":    {legacy, "written-before-the-envelope"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := rotated.Open(tc.sealed)
			if err != nil {
				t.Fatalf("Open after rotation: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Open = %q, want %q", got, tc.want)
			}
		})
	}

	// And what a value sealed after the rotation is sealed with. Holding an old
	// key must not mean writing with it.
	fresh, err := rotated.Seal("sealed-by-b")
	if err != nil {
		t.Fatalf("Seal after rotation: %v", err)
	}
	if got, err := rotated.KeyIDFor(fresh); err != nil || got != rotated.ActiveKeyID() {
		t.Fatalf("a value sealed after rotation names key %s (%v), want the active key %s",
			got, err, rotated.ActiveKeyID())
	}
}

// The defect being fixed. A key dropped too early and a value someone edited
// used to produce the same sentence, and the two have different answers: put
// the key back, or the value is gone.
func TestOpenSaysWhichFailureItWas(t *testing.T) {
	a := keeperFor(t, keyA)
	sealed, err := a.Seal("hunter2")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// A dropped once B is active and A is no longer held.
	dropped := keeperFor(t, keyB)
	_, err = dropped.Open(sealed)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Open with the sealing key dropped = %v, want ErrUnknownKey", err)
	}
	// The ID has to be in the message: "which key do I need back" is the only
	// thing the operator can act on.
	if !strings.Contains(err.Error(), a.ActiveKeyID()) {
		t.Errorf("the error does not name the missing key %s: %v", a.ActiveKeyID(), err)
	}

	// The same value, altered, with every key still held.
	altered := append([]byte(nil), sealed...)
	altered[len(altered)-1] ^= 0xff
	if _, err := a.Open(altered); !errors.Is(err, ErrAltered) {
		t.Fatalf("Open of an altered value = %v, want ErrAltered", err)
	}

	// Editing the key ID must fail rather than look like a different key's
	// work, because the header is authenticated along with the value.
	relabelled := append([]byte(nil), sealed...)
	relabelled[len(magic)+1] ^= 0xff
	if _, err := a.Open(relabelled); err == nil {
		t.Fatal("Open accepted a value whose key ID was edited")
	}

	// Too short to be either format. Not a key problem, and it should not be
	// reported as one.
	if _, err := a.Open([]byte("short")); !errors.Is(err, ErrAltered) {
		t.Fatalf("Open of a truncated value = %v, want ErrAltered", err)
	}
}

// Which key a stored value needs, so retiring one is a query over the rows
// rather than a guess.
func TestKeyIDForNamesTheKeyThatOpenedTheValue(t *testing.T) {
	rotated := keeperFor(t, keyB, keyA)
	a, b := keeperFor(t, keyA), keeperFor(t, keyB)

	for name, tc := range map[string]struct {
		sealed []byte
		want   string
	}{
		"sealed under the active key": {mustSeal(t, b, "now"), b.ActiveKeyID()},
		"sealed under the old key":    {mustSeal(t, a, "before"), a.ActiveKeyID()},
		// A legacy value has no ID in its bytes, so the answer is the key that
		// actually authenticates it — which is the answer a retirement check
		// needs, and one no header could have given.
		"written before the envelope": {sealLegacy(t, keyA, "long ago", nil), a.ActiveKeyID()},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := rotated.KeyIDFor(tc.sealed)
			if err != nil {
				t.Fatalf("KeyIDFor: %v", err)
			}
			if got != tc.want {
				t.Fatalf("KeyIDFor = %s, want %s", got, tc.want)
			}
		})
	}

	// And the list to compare those answers against.
	if got := rotated.KeyIDs(); len(got) != 2 || got[0] != b.ActiveKeyID() || got[1] != a.ActiveKeyID() {
		t.Errorf("KeyIDs = %v, want the active key first then the retired one", got)
	}
}

// A retired key is checked as strictly as the active one, because a malformed
// entry means values silently stop opening at the first rotation.
func TestNewKeeperChecksRetiredKeys(t *testing.T) {
	for name, previous := range map[string]string{
		"not base64": "!!!!not base64!!!!",
		"too short":  encode([]byte("short")),
		"all zeroes": encode(make([]byte, 32)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewKeeper(encode(keyA), previous); err == nil {
				t.Fatalf("NewKeeper accepted a retired key that is %s", name)
			}
		})
	}
}

// An operator part-way through a rotation who lists a key twice, or lists the
// active key among the retired ones, is not making a mistake worth refusing.
func TestNewKeeperHoldsEachKeyOnce(t *testing.T) {
	k, err := NewKeeper(encode(keyA), encode(keyA), encode(keyB), encode(keyB), encode(keyC))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	if got := k.KeyIDs(); len(got) != 3 {
		t.Errorf("KeyIDs = %v, want three distinct keys", got)
	}
	// Four entries were configured and two keys are retired. This is the number
	// startup reports, and it is the one an operator reads to decide whether the
	// old key is still loaded — the configured length would say four and answer
	// a different question than the one being asked.
	if retired := len(k.KeyIDs()) - 1; retired != 2 {
		t.Errorf("the keeper holds %d retired keys, want 2 from four configured entries", retired)
	}
	if _, err := k.Open(sealLegacy(t, keyC, "still opens", nil)); err != nil {
		t.Errorf("a value under the third key did not open: %v", err)
	}
}

func mustSeal(t *testing.T, k *Keeper, plain string) []byte {
	t.Helper()
	sealed, err := k.Seal(plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return sealed
}
