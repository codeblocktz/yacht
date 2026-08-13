package app

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/secret"
)

// The upgrade as an app actually experiences it: a secret written by a build
// that had never heard of the envelope, opened on the way to the cluster by
// one that has.
//
// The row is sealed here with crypto/aes rather than with Seal, because bytes
// from today's writer say nothing about the bytes already in somebody's
// database — and this path is the one where getting it wrong means every app
// with a secret stops deploying.
func TestASecretSealedBeforeTheEnvelopeStillDeploys(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte("k"), 32)
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, orch, pool := testService(t, Options{Keeper: keeper})
	id := owner(t, s, pool, "svc-legacy-secret")

	const password = "postgres://app:hunter2@db.internal/prod"
	seedLegacySecret(t, s, pool, id, key, password)

	// Any apply opens every secret the app has, so setting an unrelated
	// variable is enough to put the old one on the path to the cluster.
	if err := s.SetVariable(ctx, id, "web", VariableInput{
		Key: "LOG_LEVEL", Value: "info",
	}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("worker variable rollout: %v", err)
	}

	spec := orch.lastAppSpec()
	if spec.Secrets["DATABASE_URL"] != password {
		t.Fatalf("the workload did not receive the pre-upgrade secret: %+v", spec.Secrets)
	}
	if spec.Env["DATABASE_URL"] != "" {
		t.Fatal("the secret was also placed in the plain environment")
	}
}

// A secret that will not open stops the whole apply, and the failure names the
// variable and says which kind of failure it was.
//
// Deploying the rest without it is the alternative, and it is worse: the app
// comes up missing one variable and fails somewhere further along, at which
// point nothing connects the symptom to the key.
func TestAnUnopenableSecretStopsTheApplyAndSaysWhy(t *testing.T) {
	ctx := context.Background()
	sealing, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, _, pool := testService(t, Options{Keeper: sealing})
	id := owner(t, s, pool, "svc-lost-key")

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{
		Key: "DATABASE_URL", Value: "hunter2", Secret: true,
	}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}

	// The same install after the sealing key was dropped rather than retired.
	replaced, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("j"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	after := NewService(pool, &recordingOrchestrator{Noop: orchestrator.NewNoop()},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{Keeper: replaced, Manifests: staticManifests{}})

	if err := after.SetVariable(ctx, id, "web", VariableInput{Key: "LOG_LEVEL", Value: "info"}); err != nil {
		t.Fatalf("admit variable rollout: %v", err)
	}
	err = executeNamedOperation(after, ctx, id, "web")
	if !errors.Is(err, secret.ErrUnknownKey) {
		t.Errorf("apply failed with %v, want ErrUnknownKey — the operator needs to be told "+
			"the key is missing rather than that something might be corrupt", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("DATABASE_URL")) {
		t.Errorf("the failure does not name the variable: %v", err)
	}

	// Retaining the old key instead of dropping it is the fix, and it must need
	// nothing else — no rewrite, no migration.
	retained, err := secret.NewKeeper(
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("j"), 32)),
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper with a retired key: %v", err)
	}
	orch := &recordingOrchestrator{Noop: orchestrator.NewNoop()}
	rotated := NewService(pool, orch, slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{Keeper: retained, Manifests: staticManifests{}})
	if err := rotated.SetVariable(ctx, id, "web", VariableInput{
		Key: "LOG_LEVEL", Value: "debug",
	}); err != nil {
		t.Fatalf("admit with the old key retained: %v", err)
	}
	if err := executeNamedOperation(rotated, ctx, id, "web"); err != nil {
		t.Fatalf("worker apply with the old key retained: %v", err)
	}
	if got := orch.lastAppSpec().Secrets["DATABASE_URL"]; got != "hunter2" {
		t.Fatalf("the secret came through as %q after rotation, want %q", got, "hunter2")
	}
}

// seedLegacySecret creates the app and writes a secret variable straight into
// the table in the layout used before the envelope carried a version.
func seedLegacySecret(
	t *testing.T, s *Service, pool *pgxpool.Pool, ownerID string, key []byte, plain string,
) {
	t.Helper()
	ctx := context.Background()

	if _, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var appID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM apps WHERE owner_id = $1 AND name = 'web'`, ownerID).Scan(&appID); err != nil {
		t.Fatalf("find the app: %v", err)
	}

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

	if _, err := pool.Exec(ctx,
		`INSERT INTO variables (owner_id, app_id, key, sealed, secret)
		 VALUES ($1, $2, 'DATABASE_URL', $3, true)`,
		ownerID, appID, gcm.Seal(nonce, nonce, []byte(plain), nil)); err != nil {
		t.Fatalf("seed the pre-upgrade secret: %v", err)
	}
}
