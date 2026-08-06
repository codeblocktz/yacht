package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/registry"
)

type changingBackfillManifests struct {
	digest string
	err    error
}

func (m *changingBackfillManifests) ResolveDigest(context.Context, string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.digest, nil
}

func seedBackfillCandidate(
	t *testing.T, s *Service, ownerID, name, image, status string,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var appID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO apps (owner_id, name, namespace, image, replicas, port)
		VALUES ($1, $2, $2 || '-ns', $3, 1, 8080)
		RETURNING id`, ownerID, name, image).Scan(&appID); err != nil {
		t.Fatalf("seed %s app: %v", name, err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO deployments (owner_id, app_id, image, revision, status)
		VALUES ($1, $2, $3, 'legacy', $4)`, ownerID, appID, image, status); err != nil {
		t.Fatalf("seed %s deployment: %v", name, err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO app_release_backfills (owner_id, app_id, state)
		VALUES ($1, $2, 'pending')`, ownerID, appID); err != nil {
		t.Fatalf("seed %s cohort: %v", name, err)
	}
	return appID
}

func TestReleaseBackfillIsResumableAndLeavesLegacyAttemptsUnlinked(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "release-backfill")

	var appID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO apps (owner_id, name, namespace, image, replicas, port)
		VALUES ($1, 'legacy', 'legacy-ns', 'nginx:1.27', 2, 8080)
		RETURNING id`, ownerID).Scan(&appID); err != nil {
		t.Fatalf("seed legacy app: %v", err)
	}
	var deploymentID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO deployments (owner_id, app_id, image, revision, status)
		VALUES ($1, $2, 'nginx:1.27', 'legacy', 'active')
		RETURNING id`, ownerID, appID).Scan(&deploymentID); err != nil {
		t.Fatalf("seed legacy deployment: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO app_release_backfills (owner_id, app_id, state)
		VALUES ($1, $2, 'pending')`, ownerID, appID); err != nil {
		t.Fatalf("seed cohort: %v", err)
	}

	if err := s.BackfillReleases(ctx); err != nil {
		t.Fatalf("BackfillReleases: %v", err)
	}
	if err := s.BackfillReleases(ctx); err != nil {
		t.Fatalf("BackfillReleases again: %v", err)
	}
	var releaseID uuid.UUID
	var releases int
	if err := pool.QueryRow(ctx, `
		SELECT active_release_id,
		       (SELECT count(*) FROM app_releases WHERE app_id = apps.id)
		FROM apps WHERE id = $1`, appID).Scan(&releaseID, &releases); err != nil {
		t.Fatalf("read backfill result: %v", err)
	}
	if releases != 1 {
		t.Fatalf("backfill releases = %d, want exactly one", releases)
	}
	var origin, status string
	var linked *uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT r.origin, d.status, d.release_id
		FROM app_releases r CROSS JOIN deployments d
		WHERE r.id = $1 AND d.id = $2`, releaseID, deploymentID).
		Scan(&origin, &status, &linked); err != nil {
		t.Fatalf("read release and attempt: %v", err)
	}
	if origin != "backfill" || status != DeploySucceeded || linked != nil {
		t.Fatalf("origin/status/link = %q/%q/%v", origin, status, linked)
	}
}

func TestOverlayApplyUsesTheActiveReleaseNotFailedDesiredFields(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "release-active-authority")
	created, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE apps SET image = 'nginx:broken', port = 9090, internal = true,
		                config_version = config_version + 1
		WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("seed failed desired edit: %v", err)
	}
	if err := s.SetNetworking(ctx, ownerID, created.Name, false, false); err != nil {
		t.Fatalf("network overlay apply: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	got := orch.lastAppSpec()
	if got.Image != "nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		got.Port != 8080 || got.Internal {
		t.Fatalf("overlay leaked desired state into active spec: %#v", got)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE apps SET active_release_id = NULL WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("clear baseline: %v", err)
	}
	before := orch.lastAppSpec()
	if err = s.SetNetworking(ctx, ownerID, created.Name, true, false); err != nil {
		t.Fatalf("record nil-baseline overlay: %v", err)
	}
	if err = s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps without baseline: %v", err)
	}
	if got := orch.lastAppSpec(); got.ConfigVersion != before.ConfigVersion {
		t.Fatalf("nil baseline reached the cluster: before=%d after=%d",
			before.ConfigVersion, got.ConfigVersion)
	}
}

func TestAnUnavailableBaselineStaysVisibleAndCanRecover(t *testing.T) {
	ctx := context.Background()
	resolver := &changingBackfillManifests{
		digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		err:    fmt.Errorf("legacy image disappeared: %w", registry.ErrManifestNotFound),
	}
	s, _, pool := testService(t, Options{Manifests: resolver})
	ownerID := owner(t, s, pool, "release-backfill-missing")
	appID := seedBackfillCandidate(t, s, ownerID, "missing", "registry.example/missing:v1", "active")

	if err := s.BackfillReleases(ctx); err != nil {
		t.Fatalf("BackfillReleases missing image: %v", err)
	}
	got, err := s.Get(ctx, ownerID, "missing")
	if err != nil {
		t.Fatalf("Get missing baseline: %v", err)
	}
	if got.ActiveReleaseID != nil || got.BaselineState != BaselineImageUnavailable ||
		!errors.Is(resolver.err, registry.ErrManifestNotFound) || got.BaselineError == "" {
		t.Fatalf("missing baseline state = pointer %v, state %q, error %q",
			got.ActiveReleaseID, got.BaselineState, got.BaselineError)
	}

	// Restoring the manifest is not a permanent exclusion. Advance the durable
	// retry clock and prove a later pass creates exactly one baseline.
	resolver.err = nil
	if _, err := pool.Exec(ctx, `
		UPDATE app_release_backfills SET next_attempt_at = now()
		WHERE app_id = $1`, appID); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	if err := s.BackfillReleases(ctx); err != nil {
		t.Fatalf("BackfillReleases after recovery: %v", err)
	}
	if err := s.BackfillReleases(ctx); err != nil {
		t.Fatalf("BackfillReleases idempotent recovery: %v", err)
	}
	var releases int
	var active *uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT active_release_id,
		       (SELECT count(*) FROM app_releases WHERE app_id = apps.id)
		FROM apps WHERE id = $1`, appID).Scan(&active, &releases); err != nil {
		t.Fatalf("read recovered baseline: %v", err)
	}
	if active == nil || releases != 1 {
		t.Fatalf("recovered baseline = pointer %v, releases %d", active, releases)
	}
}

func TestARegistryOutagePausesStatusCutoverUntilBackfillResumes(t *testing.T) {
	ctx := context.Background()
	resolver := &changingBackfillManifests{
		digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		err:    fmt.Errorf("registry is down: %w", registry.ErrUnreachable),
	}
	s, _, pool := testService(t, Options{Manifests: resolver})
	ownerID := owner(t, s, pool, "release-backfill-outage")
	appID := seedBackfillCandidate(t, s, ownerID, "paused", "registry.example/paused:v1", "active")

	if err := s.BackfillReleases(ctx); err != nil {
		t.Fatalf("BackfillReleases during outage: %v", err)
	}
	var state, status string
	if err := pool.QueryRow(ctx, `
		SELECT b.state, d.status FROM app_release_backfills b
		JOIN deployments d ON d.app_id = b.app_id WHERE b.app_id = $1`, appID).
		Scan(&state, &status); err != nil {
		t.Fatalf("read paused state: %v", err)
	}
	if state != BaselinePending || status != DeployActive {
		t.Fatalf("outage state/status = %q/%q, want pending/active", state, status)
	}

	resolver.err = nil
	if _, err := pool.Exec(ctx, `
		UPDATE app_release_backfills SET next_attempt_at = now()
		WHERE app_id = $1`, appID); err != nil {
		t.Fatalf("make outage retry due: %v", err)
	}
	if err := s.BackfillReleases(ctx); err != nil {
		t.Fatalf("BackfillReleases after outage: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status FROM deployments WHERE app_id = $1`, appID).Scan(&status); err != nil {
		t.Fatalf("read normalized status: %v", err)
	}
	if status != DeploySucceeded {
		t.Fatalf("status after resumed cutover = %q, want %q", status, DeploySucceeded)
	}
}

func TestActivationFailureIsReturnedAndRecordedAsAFailedAttempt(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "release-activation-failure")
	one, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "one", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create one: %v", err)
	}
	two, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "two", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create two: %v", err)
	}
	oneReleases, err := s.Releases(ctx, ownerID, one.ID, 1)
	if err != nil || len(oneReleases) != 1 {
		t.Fatalf("Releases one: %v (%d rows)", err, len(oneReleases))
	}
	var attemptID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO deployments (owner_id, app_id, image, revision, status)
		VALUES ($1, $2, 'nginx:1.27', 'bad-link', 'running') RETURNING id`,
		ownerID, two.ID).Scan(&attemptID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}

	err = s.endDeployment(ctx, ownerID, two.ID, oneReleases[0].ID, attemptID, nil)
	// SetActiveRelease reports a zero-row guarded update rather than a typed
	// not-found error; the load-bearing contract is that it is not success.
	if err == nil {
		t.Fatal("cross-app activation returned success")
	}
	var status, message string
	if err := pool.QueryRow(ctx,
		`SELECT status, message FROM deployments WHERE id = $1`, attemptID).
		Scan(&status, &message); err != nil {
		t.Fatalf("read failed activation attempt: %v", err)
	}
	if status != DeployFailed || message == "" {
		t.Fatalf("activation attempt = status %q, message %q", status, message)
	}
}
