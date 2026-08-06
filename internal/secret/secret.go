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
//
// A sealed value carries the format it was written in and the identity of the
// key that wrote it. Without those, replacing a key means every stored value
// fails at once with an error that can only guess at why, and there is no way
// to hold the old key alongside the new one while values are rewritten.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// keyBytes is the AES-256 key size. Nothing shorter is accepted: a 128-bit key
// is fine cryptographically, but accepting two lengths means one of them is
// chosen by accident.
const keyBytes = 32

// The envelope written today: a magic string, a format version, and the ID of
// the key that sealed the value, ahead of the nonce and ciphertext that GCM
// has always produced.
//
// The magic exists to make the common case cheap and unambiguous. It does not
// decide anything on its own, and must not: a value written before this format
// existed begins with a nonce, and a nonce is twelve random bytes, so it can
// begin with these four by chance. Deciding on the prefix alone would mean
// roughly one value in four billion is read as the wrong format and refused
// even though the key to open it is right there. See Open for what decides
// instead.
const (
	magic       = "YSK"
	version1    = 1
	keyIDBytes  = 8
	headerBytes = len(magic) + 1 + keyIDBytes
)

// keyIDDomain separates this digest from any other use of the same key.
//
// The fingerprint is stored beside the ciphertext, so it is public; a digest
// computed the same way for some other purpose would then be public too,
// whether or not that was ever intended.
const keyIDDomain = "yacht/secret/key-id\x00"

// ErrUnknownKey means the value names a key this install does not hold.
//
// Kept apart from ErrAltered because the two have different fixes and only one
// of them is alarming. A key retired a step too early is put back in
// YACHT_SECRET_KEY_PREVIOUS and everything opens again; a value that will not
// authenticate under a key that should open it has been changed underneath us.
// One error covering both is what the operator used to get, and it left them
// guessing at which had happened.
var ErrUnknownKey = errors.New("secret: sealed with a key this install does not hold")

// ErrAltered means the bytes did not authenticate under any key held.
var ErrAltered = errors.New("secret: the stored value did not authenticate")

// ErrFutureVersion means the envelope names a format this build cannot read.
//
// Its own error rather than ErrAltered, because the likely answer is to stop
// running the older binary. Reported as altered it would read as "this value is
// gone, set it again" — the one response that would actually lose it, since the
// newer binary could have opened it all along. Altered bytes remain a real
// possibility here and the message says so rather than picking.
var ErrFutureVersion = errors.New("secret: the stored value uses a newer envelope format")

// sealingKey is one key and the name derived from it.
type sealingKey struct {
	id   [keyIDBytes]byte
	aead cipher.AEAD
}

// name is the key's ID as it appears in errors and in a retirement check.
func (k sealingKey) name() string { return hex.EncodeToString(k.id[:]) }

// Keeper seals values with one key and opens them with any key it holds.
type Keeper struct {
	// active seals. Exactly one key does, always: sealing with whichever key
	// happened to match would make the stored format depend on read history.
	active sealingKey

	// previous open and never seal. They are how a key is replaced without a
	// flag day — both keys are held until every value sealed with the old one
	// has been written again under the new one.
	previous []sealingKey
}

// NewKeeper builds a Keeper from a base64-encoded 32-byte key, plus any keys
// retained only to open values sealed before it.
//
// Generate one with:
//
//	openssl rand -base64 32
func NewKeeper(encodedKey string, encodedPrevious ...string) (*Keeper, error) {
	if encodedKey == "" {
		return nil, errors.New("secret: no key configured")
	}
	active, err := newSealingKey(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}

	k := &Keeper{active: active}
	for i, encoded := range encodedPrevious {
		previous, err := newSealingKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("secret: YACHT_SECRET_KEY_PREVIOUS entry %d: %w", i+1, err)
		}
		// A key listed twice, or listed here while it is also the active one,
		// is an operator part-way through a rotation rather than a mistake to
		// refuse over. Holding a key twice is the same as holding it once.
		if _, held := k.key(previous.name()); held {
			continue
		}
		k.previous = append(k.previous, previous)
	}
	return k, nil
}

// newSealingKey decodes and checks one key.
//
// Errors here are deliberately unprefixed: the caller knows whether this is
// the active key or a retired one, and says so.
func newSealingKey(encodedKey string) (sealingKey, error) {
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return sealingKey{}, fmt.Errorf("key is not base64: %w", err)
	}
	if len(key) != keyBytes {
		return sealingKey{}, fmt.Errorf("key is %d bytes, want %d — generate one with `openssl rand -base64 32`",
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
		return sealingKey{}, errors.New("key is all zeroes")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return sealingKey{}, fmt.Errorf("cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealingKey{}, fmt.Errorf("gcm: %w", err)
	}

	return sealingKey{id: deriveKeyID(key), aead: aead}, nil
}

// deriveKeyID names a key from the key itself.
//
// Derived rather than assigned by the operator. An ID somebody types can be
// attached to the wrong key — and once it can, "which key opened this" is a
// claim rather than a check, which is precisely the question a retirement
// decision turns on. A fingerprint cannot be mislabelled.
//
// Publishing a truncated digest of the key next to the ciphertext gives
// nothing away: the key is 256 bits out of a CSPRNG, so there is nothing to
// guess back from eight bytes of its hash. Eight bytes is far more than enough
// to tell apart the handful of keys an install ever holds.
func deriveKeyID(key []byte) [keyIDBytes]byte {
	h := sha256.New()
	h.Write([]byte(keyIDDomain))
	h.Write(key)

	var id [keyIDBytes]byte
	copy(id[:], h.Sum(nil))
	return id
}

// Configured reports whether values can be sealed.
//
// A nil Keeper is the shape of "no key set", so callers can hold one without a
// separate flag and this stays safe to call.
func (k *Keeper) Configured() bool { return k != nil && k.active.aead != nil }

// ActiveKeyID is the ID of the key values are sealed with now.
func (k *Keeper) ActiveKeyID() string {
	if !k.Configured() {
		return ""
	}
	return k.active.name()
}

// KeyIDs lists every key held, the active one first.
//
// The other half of a retirement check: KeyIDFor says which key a value needs,
// this says which are still being kept for it.
func (k *Keeper) KeyIDs() []string {
	if !k.Configured() {
		return nil
	}
	out := make([]string, 0, 1+len(k.previous))
	for _, key := range k.all() {
		out = append(out, key.name())
	}
	return out
}

// all returns the keys to try, active first — it is the one most values were
// sealed with, and trying it first keeps the common case to one GCM open.
func (k *Keeper) all() []sealingKey {
	return append([]sealingKey{k.active}, k.previous...)
}

// key finds a held key by ID.
func (k *Keeper) key(id string) (sealingKey, bool) {
	if !k.Configured() {
		return sealingKey{}, false
	}
	for _, key := range k.all() {
		if key.name() == id {
			return key, true
		}
	}
	return sealingKey{}, false
}

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

	header := make([]byte, headerBytes)
	copy(header, magic)
	header[len(magic)] = version1
	copy(header[len(magic)+1:], k.active.id[:])

	nonce := make([]byte, k.active.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secret: nonce: %w", err)
	}

	sealed := make([]byte, 0, headerBytes+len(nonce)+len(plain)+k.active.aead.Overhead())
	sealed = append(sealed, header...)
	sealed = append(sealed, nonce...)

	// The header is authenticated as well as stored. Nothing in it is secret;
	// what that buys is that a value cannot be relabelled — editing the key ID
	// on a stored row makes it fail to open rather than making it look like it
	// was sealed by a key that never touched it.
	return k.active.aead.Seal(sealed, nonce, []byte(plain), header), nil
}

// Open decrypts a stored value.
//
// GCM authenticates as well as encrypts, so a value altered in the database
// fails here rather than decrypting to something else.
func (k *Keeper) Open(sealed []byte) (string, error) {
	plain, _, err := k.open(sealed)
	return plain, err
}

// KeyIDFor reports which held key opens a stored value.
//
// This is what turns "can this key be retired" into a query: a value that
// answers with some other key's ID does not need the one being dropped.
//
// It answers by opening the value rather than by reading its header, because
// the header is only what the bytes claim. A value written before the envelope
// existed carries no ID at all, and the only truthful answer for one of those
// is the key that actually authenticates it.
func (k *Keeper) KeyIDFor(sealed []byte) (string, error) {
	_, id, err := k.open(sealed)
	return id, err
}

// open decrypts and reports which key did it.
//
// The order here is the whole design. A versioned parse is attempted first and
// its failure is not conclusive — the bytes are then tried as the layout this
// package wrote before there was a version. That fallback is what makes the
// magic prefix safe to have: a legacy nonce that happens to begin with it
// fails to authenticate under the versioned reading and opens under the legacy
// one, instead of being refused on a coincidence. Authentication decides,
// never the prefix, so a misparse produces a failure rather than the wrong
// plaintext.
func (k *Keeper) open(sealed []byte) (plain, keyID string, err error) {
	if !k.Configured() {
		return "", "", errors.New("secret: no key configured")
	}

	nonceSize, overhead := k.active.aead.NonceSize(), k.active.aead.Overhead()
	if len(sealed) < nonceSize+overhead {
		// Shorter than the smallest thing either layout can produce, so there
		// is nothing to try and nothing to be uncertain about.
		return "", "", fmt.Errorf("%w: the stored value is too short to be one", ErrAltered)
	}

	// Named here rather than returned immediately: a value claiming a key we do
	// not hold, or a version this build cannot read, is the likely story — but
	// only once the legacy reading has failed too, since either claim may be a
	// few bytes of a nonce.
	var claimed string

	id, versioned, newer := headerKeyID(sealed, nonceSize, overhead)
	if versioned {
		if key, held := k.key(id); held {
			nonce := sealed[headerBytes : headerBytes+nonceSize]
			if plain, err := key.aead.Open(nil, nonce, sealed[headerBytes+nonceSize:],
				sealed[:headerBytes]); err == nil {
				return string(plain), key.name(), nil
			}
		} else {
			claimed = id
		}
	}

	// The legacy layout: a bare nonce and ciphertext, with nothing to say which
	// key sealed it.
	//
	// Tried against every key held rather than only the active one. There is no
	// ID to go on, so trying is the only way to attribute one of these — and
	// binding them to the active key alone would strand every row not yet
	// rewritten at the first rotation after an upgrade, which is exactly the
	// install this format change exists to protect. GCM decides, so a wrong key
	// here fails rather than returning something else.
	for _, key := range k.all() {
		if plain, err := key.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], nil); err == nil {
			return string(plain), key.name(), nil
		}
	}

	if claimed != "" {
		return "", "", fmt.Errorf("%w: it names key %s and this install holds %s — put the "+
			"missing key back in YACHT_SECRET_KEY_PREVIOUS, or the value cannot be recovered",
			ErrUnknownKey, claimed, strings.Join(k.KeyIDs(), ", "))
	}
	if newer {
		// Both causes named, and in that order. Getting here took only the three
		// magic bytes matching, which a random nonce manages about once in
		// sixteen million — so a value from a later Yacht and a value whose
		// leading bytes were altered are genuinely indistinguishable at this
		// point. Naming only the first would send an operator hunting for a
		// binary to roll back to over a row that is simply corrupt.
		return "", "", fmt.Errorf("%w: it declares envelope version %d and this build reads "+
			"version %d — either it was written by a later Yacht, in which case that binary "+
			"opens it and no key here will, or these bytes were altered",
			ErrFutureVersion, sealed[len(magic)], version1)
	}
	// Deliberately two possibilities and not one: a value in the old layout
	// carries no key identity, so a lost key and a corrupted byte are
	// indistinguishable here and saying otherwise would be guessing.
	return "", "", fmt.Errorf("%w: it was altered, or it was sealed in the older format with "+
		"a key this install has never held", ErrAltered)
}

// headerKeyID returns the key ID a value's header claims, if it has one, and
// otherwise whether the bytes claim a version this build cannot read.
//
// "Claims" is the operative word for both answers — the caller must treat
// either as a hint and let authentication settle it. The version is the weaker
// of the two: reaching it needs only the three magic bytes to match, which a
// random nonce does about once in sixteen million, so it decides nothing on its
// own and is used only to word a failure that has already happened.
func headerKeyID(sealed []byte, nonceSize, overhead int) (id string, versioned, newer bool) {
	if len(sealed) < headerBytes+nonceSize+overhead {
		return "", false, false
	}
	if string(sealed[:len(magic)]) != magic {
		return "", false, false
	}
	// An unrecognised version falls through to the legacy reading and fails
	// there. That is the correct answer for bytes from a future release: this
	// binary cannot open them, and adding a version means teaching this line
	// about it. What saying so buys is the difference between an operator
	// putting the older binary back and an operator concluding the value is
	// gone — the newer one could have opened it the whole time.
	if sealed[len(magic)] != version1 {
		return "", false, true
	}
	return hex.EncodeToString(sealed[len(magic)+1 : headerBytes]), true, false
}
