package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/registry"
	"github.com/codeblocktz/yacht/internal/secret"
)

// rollbackManifests gives every image its own digest, so two releases of two
// tags are distinguishable, and can be told an image has left the registry.
type rollbackManifests struct {
	mu   sync.Mutex
	gone map[string]bool
}

func (m *rollbackManifests) ResolveDigest(_ context.Context, ref string) (string, error) {
	sum := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (m *rollbackManifests) CheckDigest(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	repo, _, _ := strings.Cut(ref, "@")
	if m.gone[repo] {
		return registry.ErrManifestNotFound
	}
	return nil
}

// rollbackFixture is an app that ran nginx:1.26 with two replicas and a plain
// variable, then moved to nginx:1.27 with one replica, a changed variable, a
// new one and a secret. first is the release to go back to.
func rollbackFixture(t *testing.T, name string) (*Service, *recordingOrchestrator, *rollbackManifests, string, App, Release) {
	t.Helper()
	ctx := context.Background()
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	manifests := &rollbackManifests{gone: map[string]bool{}}
	s, orch, pool := testService(t, Options{Keeper: keeper, Manifests: manifests})
	ownerID := owner(t, s, pool, name)

	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.26", Replicas: 2, Port: 8080,
		Env: map[string]string{"MODE": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := activeRelease(t, s, ownerID, a.Name)

	if _, err := s.Update(ctx, ownerID, a.Name, UpdateInput{Image: "nginx:1.27", Port: 8080}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	mustExecute(t, s, ownerID, a.Name)
	if _, err := s.Scale(ctx, ownerID, a.Name, 1); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	mustExecute(t, s, ownerID, a.Name)
	for _, v := range []VariableInput{
		{Key: "MODE", Value: "new"},
		{Key: "ADDED", Value: "later"},
		{Key: "TOKEN", Value: "s3cret", Secret: true},
	} {
		if err := s.SetVariable(ctx, ownerID, a.Name, v); err != nil {
			t.Fatalf("SetVariable %s: %v", v.Key, err)
		}
		if !v.Secret {
			mustExecute(t, s, ownerID, a.Name)
		}
	}
	return s, orch, manifests, ownerID, a, first
}

func activeRelease(t *testing.T, s *Service, ownerID, name string) Release {
	t.Helper()
	a, err := s.Get(context.Background(), ownerID, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a.ActiveReleaseID == nil {
		t.Fatal("no active release")
	}
	r, err := s.ReleaseByID(context.Background(), ownerID, a.ID, *a.ActiveReleaseID)
	if err != nil {
		t.Fatalf("ReleaseByID: %v", err)
	}
	return r
}

func mustExecute(t *testing.T, s *Service, ownerID, name string) {
	t.Helper()
	if err := executeNamedOperation(s, context.Background(), ownerID, name); err != nil {
		t.Fatalf("execute operation for %s: %v", name, err)
	}
}

func variablesOf(t *testing.T, s *Service, a App) map[string]Variable {
	t.Helper()
	vars, err := s.variablesFor(context.Background(), s.q, a.ID)
	if err != nil {
		t.Fatalf("variablesFor: %v", err)
	}
	out := map[string]Variable{}
	for _, v := range vars {
		out[v.Key] = v
	}
	return out
}

// Rolling back runs the old release, with current secrets, and makes the app's
// settings the old ones too.
func TestRollbackRestoresTheReleaseAndTheSettings(t *testing.T) {
	ctx := context.Background()
	s, orch, _, ownerID, a, first := rollbackFixture(t, "rollback-restores")

	if err := s.Rollback(ctx, ownerID, a.Name, first.ID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	mustExecute(t, s, ownerID, a.Name)

	if got := activeRelease(t, s, ownerID, a.Name); got.ID != first.ID {
		t.Fatalf("active release = %s, want the one rolled back to (%s)", got.ID, first.ID)
	}
	spec := orch.lastAppSpec()
	if spec.Image != pinImage(first.ImageRef, first.ImageDigest) || spec.Replicas != 2 {
		t.Errorf("applied %s x%d, want %s x2", spec.Image, spec.Replicas, pinImage(first.ImageRef, first.ImageDigest))
	}
	if spec.Env["MODE"] != "old" || spec.Env["ADDED"] != "" {
		t.Errorf("applied env = %v, want the release's", spec.Env)
	}
	if spec.Secrets["TOKEN"] != "s3cret" {
		t.Error("the current secret did not survive the rollback")
	}

	got, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Image != "nginx:1.26" || got.Replicas != 2 {
		t.Errorf("app settings = %s x%d, want nginx:1.26 x2", got.Image, got.Replicas)
	}
	vars := variablesOf(t, s, got)
	if vars["MODE"].Value != "old" {
		t.Errorf("MODE = %q, want old", vars["MODE"].Value)
	}
	if _, ok := vars["ADDED"]; ok {
		t.Error("a variable added after the release survived the rollback")
	}
	if !vars["TOKEN"].Secret {
		t.Error("the secret was removed by the rollback")
	}
}

// The property the settings restore exists for: the next change builds on what
// is running, rather than quietly rolling forward to what was replaced.
func TestTheNextChangeAfterARollbackStaysRolledBack(t *testing.T) {
	ctx := context.Background()
	s, orch, _, ownerID, a, first := rollbackFixture(t, "rollback-sticks")

	if err := s.Rollback(ctx, ownerID, a.Name, first.ID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	mustExecute(t, s, ownerID, a.Name)
	if _, err := s.Scale(ctx, ownerID, a.Name, 3); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	mustExecute(t, s, ownerID, a.Name)

	spec := orch.lastAppSpec()
	if !strings.HasPrefix(spec.Image, "nginx@") || spec.Image != pinImage("nginx:1.26", first.ImageDigest) {
		t.Errorf("scaling after the rollback applied %s, want the rolled-back image", spec.Image)
	}
	if spec.Env["MODE"] != "old" {
		t.Errorf("scaling after the rollback applied MODE=%q, want old", spec.Env["MODE"])
	}
}

// A release whose image has gone cannot be restored, and nothing is touched in
// finding that out.
func TestRollbackToAMissingImageChangesNothing(t *testing.T) {
	ctx := context.Background()
	s, _, manifests, ownerID, a, first := rollbackFixture(t, "rollback-gone")
	before, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	manifests.gone["nginx"] = true
	if err := s.Rollback(ctx, ownerID, a.Name, first.ID); !errors.Is(err, ErrReleaseImageGone) {
		t.Fatalf("Rollback = %v, want ErrReleaseImageGone", err)
	}
	after, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.ConfigVersion != before.ConfigVersion || after.Image != before.Image {
		t.Errorf("settings moved on a refused rollback: %s v%d -> %s v%d",
			before.Image, before.ConfigVersion, after.Image, after.ConfigVersion)
	}
	if _, err := s.liveOperation(ctx, after); err == nil {
		t.Error("a refused rollback admitted a deployment")
	}
}

// A deploy already in flight refuses the rollback, and the settings restore
// that shares its transaction is abandoned with it.
func TestRollbackDuringADeployChangesNothing(t *testing.T) {
	ctx := context.Background()
	s, _, _, ownerID, a, first := rollbackFixture(t, "rollback-inflight")

	if err := s.Redeploy(ctx, ownerID, a.Name); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	before, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.Rollback(ctx, ownerID, a.Name, first.ID); !errors.Is(err, ErrOperationInFlight) {
		t.Fatalf("Rollback = %v, want ErrOperationInFlight", err)
	}
	after, err := s.Get(ctx, ownerID, a.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.ConfigVersion != before.ConfigVersion || after.Image != before.Image {
		t.Errorf("settings moved under a refused rollback: %s -> %s", before.Image, after.Image)
	}
	if vars := variablesOf(t, s, after); vars["MODE"].Value != "new" {
		t.Errorf("MODE = %q after a refused rollback, want new", vars["MODE"].Value)
	}
}

func TestRollbackRefusesTheRunningReleaseAndAnother(t *testing.T) {
	ctx := context.Background()
	s, _, _, ownerID, a, _ := rollbackFixture(t, "rollback-refusals")

	current := activeRelease(t, s, ownerID, a.Name)
	if err := s.Rollback(ctx, ownerID, a.Name, current.ID); !errors.Is(err, ErrReleaseActive) {
		t.Errorf("rollback to the running release = %v, want ErrReleaseActive", err)
	}
	if err := s.Rollback(ctx, ownerID, a.Name, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("rollback to an unknown release = %v, want ErrNotFound", err)
	}
}
