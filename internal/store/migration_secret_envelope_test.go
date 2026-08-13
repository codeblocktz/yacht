package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/codeblocktz/yacht/internal/secret"
)

// The sealed columns are the one thing in this schema that no migration may
// touch, and the envelope gaining a version is when that gets tested.
//
// Nothing in the upgrade rewrites them: the bytes in variables.sealed and
// platform_registry.password_sealed were written by a build that had never
// heard of a header, and they stay exactly as they are until something writes
// that row again. So the claim under test is not that a migration converted
// them — it is that it left them alone and the new reader still opens them.
// Getting this wrong means every app fails to deploy and the registry becomes
// unusable, with an error that points at the key rather than at the upgrade.
//
// The ciphertext here is built with crypto/aes directly. Sealing with today's
// writer and reading it back would prove nothing about the bytes already in
// somebody's database, which are the only bytes that matter.
func TestSealedColumnsStayReadableAcrossTheUpgrade(t *testing.T) {
	ctx := context.Background()
	dsn := migrationSandbox(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	// A database from before the change, filled the way a running install
	// would be, then brought all the way up.
	if err := goose.UpToContext(ctx, db, migrationsDir, 21); err != nil {
		t.Fatalf("migrate to 21: %v", err)
	}
	key := seedLegacySealedRows(t, ctx, db)
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	// The install that has never rotated: one key in YACHT_SECRET_KEY, exactly
	// as it was before the upgrade. This is the entire upgrade path, and it has
	// to need no action at all.
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}

	var sealed []byte
	if err := db.QueryRowContext(ctx,
		`SELECT sealed FROM variables WHERE key = 'DATABASE_URL'`).Scan(&sealed); err != nil {
		t.Fatalf("read the sealed variable: %v", err)
	}
	if got, err := keeper.Open(sealed); err != nil || got != legacyDatabaseURL {
		t.Fatalf("the app's secret variable came back %q (%v), want %q — every app with a "+
			"secret would fail to deploy", got, err, legacyDatabaseURL)
	}

	var password []byte
	if err := db.QueryRowContext(ctx,
		`SELECT password_sealed FROM platform_registry WHERE id = 1`).Scan(&password); err != nil {
		t.Fatalf("read the registry password: %v", err)
	}
	if got, err := keeper.Open(password); err != nil || got != legacyRegistryPassword {
		t.Fatalf("the registry password came back %q (%v), want %q — nothing could push or "+
			"pull an image", got, err, legacyRegistryPassword)
	}

	// The other half of why the envelope exists: those same untouched rows must
	// keep opening once a second key is in use, because rotating is what an
	// operator does next and the rows have not been rewritten yet.
	rotated, err := secret.NewKeeper(
		base64.StdEncoding.EncodeToString([]byte("a-second-key-thirty-two-bytes-ok")),
		base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewKeeper with a retired key: %v", err)
	}
	if got, err := rotated.Open(sealed); err != nil || got != legacyDatabaseURL {
		t.Fatalf("after rotation the untouched row came back %q (%v), want %q", got, err, legacyDatabaseURL)
	}
	// And the retirement check the rows have to answer before the old key can
	// go: a value written before the envelope still names the key it needs.
	if id, err := rotated.KeyIDFor(sealed); err != nil || id != keeper.ActiveKeyID() {
		t.Errorf("KeyIDFor on a pre-upgrade row = %s (%v), want %s", id, err, keeper.ActiveKeyID())
	}

	// Rewriting the row is what moves it to the new format, and the column and
	// its constraint have to take the longer value. This is the lazy migration
	// the plan calls for — no SQL touches key material, a write does.
	resealed, err := rotated.Seal(legacyDatabaseURL)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE variables SET sealed = $1 WHERE key = 'DATABASE_URL'`, resealed); err != nil {
		t.Fatalf("re-seal the variable in place: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT sealed FROM variables WHERE key = 'DATABASE_URL'`).Scan(&sealed); err != nil {
		t.Fatalf("read the re-sealed variable: %v", err)
	}
	if got, err := rotated.Open(sealed); err != nil || got != legacyDatabaseURL {
		t.Fatalf("the re-sealed variable came back %q (%v), want %q", got, err, legacyDatabaseURL)
	}
	if id, err := rotated.KeyIDFor(sealed); err != nil || id != rotated.ActiveKeyID() {
		t.Errorf("a re-sealed row names key %s (%v), want the active key %s — a row rewritten "+
			"after rotation still holding the old key would never let it be retired",
			id, err, rotated.ActiveKeyID())
	}
}

// What the rows hold. Checked by value on the way out, so a read that silently
// returned something else is a failure rather than a pass.
const (
	legacyDatabaseURL      = "postgres://app:hunter2@db.internal:5432/prod?sslmode=require"
	legacyRegistryPassword = "ghp_a-registry-push-token"
)

// seedLegacySealedRows fills a pre-upgrade schema with a secret variable and a
// registry credential, both sealed the way the old writer did it, and returns
// the key they were sealed with.
func seedLegacySealedRows(t *testing.T, ctx context.Context, db *sql.DB) []byte {
	t.Helper()

	key := []byte("a-key-that-is-thirty-two-bytes!!")
	const owner = "team-envelope"

	if _, err := db.ExecContext(ctx,
		`INSERT INTO teams (id, display_name) VALUES ($1, 'Envelope') ON CONFLICT DO NOTHING`,
		owner); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	var appID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO apps (owner_id, name, namespace, image, replicas, port)
		 VALUES ($1, 'web', 'ns-envelope-web', 'nginx:alpine', 1, 8080)
		 RETURNING id`, owner).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	// A sealed variable and a readable one beside it, because the constraint
	// that keeps those apart is the thing an upgrade could break without
	// anybody noticing until an app is applied.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO variables (owner_id, app_id, key, sealed, secret)
		 VALUES ($1, $2, 'DATABASE_URL', $3, true)`,
		owner, appID, sealLegacy(t, key, legacyDatabaseURL)); err != nil {
		t.Fatalf("seed the sealed variable: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO variables (owner_id, app_id, key, value, secret)
		 VALUES ($1, $2, 'LOG_LEVEL', 'info', false)`,
		owner, appID); err != nil {
		t.Fatalf("seed the readable variable: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO platform_registry (id, host, repository, username, password_sealed)
		 VALUES (1, 'ghcr.io', 'acme', 'yacht', $1)`,
		sealLegacy(t, key, legacyRegistryPassword)); err != nil {
		t.Fatalf("seed the registry credential: %v", err)
	}

	return key
}

// sealLegacy produces the layout internal/secret wrote before the envelope had
// a version: a bare nonce followed by ciphertext, with no header.
func sealLegacy(t *testing.T, key []byte, plain string) []byte {
	t.Helper()

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil)
}
