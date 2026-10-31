package app

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// tagManifests is a registry whose tags a test moves.
type tagManifests struct {
	mu      sync.Mutex
	digests map[string]string
	tags    []string
}

func (m *tagManifests) ResolveDigest(_ context.Context, ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.digests[ref]; ok {
		return d, nil
	}
	return "", ErrNotFound
}

func (m *tagManifests) Tags(context.Context, string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tags, nil
}

func (m *tagManifests) set(ref, digest string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.digests[ref] = digest
}

func digestOf(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// pgRegistry is postgres with 16.15 the newest of 16, and 17 out.
func pgRegistry() *tagManifests {
	return &tagManifests{
		digests: map[string]string{
			"postgres:16-alpine": digestOf("a"), "postgres:16.15-alpine": digestOf("a"),
			"postgres:16.4-alpine": digestOf("4"),
		},
		tags: []string{"16-alpine", "16.4-alpine", "16.15-alpine", "16.16-alpine", "17-alpine", "17.2-alpine"},
	}
}

// running says what the database answers when asked its version, as a
// connected server would.
func running(s *Service, a App, version string) {
	s.dbVersionsMu.Lock()
	defer s.dbVersionsMu.Unlock()
	if s.dbVersions == nil {
		s.dbVersions = map[uuid.UUID]string{}
	}
	s.dbVersions[*a.ActiveReleaseID] = version
}

func lastDeployment(t *testing.T, s *Service, ownerID, name string) Deployment {
	t.Helper()
	a, err := s.Get(ctx(), ownerID, name)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := s.Deployments(ctx(), ownerID, a.ID, 5)
	if err != nil || len(deps) == 0 {
		t.Fatalf("deployments: %v, %d", err, len(deps))
	}
	return deps[0]
}

func ctx() context.Context { return context.Background() }

// The 16-alpine tag moves to 16.16: Update backs the database up first, and
// once the backup has succeeded deploys the tag again as a deployment called
// what it updated to. Afterwards there is nothing to update, and 17 is only
// ever said.
func TestUpdateBacksUpThenDeploysTheNewerImage(t *testing.T) {
	reg := pgRegistry()
	s, orch, id := backupService(t, func(o *Options) { o.Manifests = reg })
	a, err := createAndDeploy(t, s, ctx(), id, CreateInput{Source: SourcePostgres, Name: "pg"})
	if err != nil {
		t.Fatal(err)
	}
	running(s, a, "16.15 (Debian 16.15-1.pgdg120+1)")

	rel, err := s.DatabaseRelease(ctx(), id, "pg")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Update != nil || rel.Version != "16.15" || rel.NextMajor != "17" {
		t.Fatalf("before the tag moved: update %+v, version %q, next %q", rel.Update, rel.Version, rel.NextMajor)
	}
	if _, err := s.UpdateDatabase(ctx(), id, "pg", "ana@example.com"); err != ErrNoDatabaseUpdate {
		t.Fatalf("an update with nothing newer: %v", err)
	}

	// 16.16 is published, and the tag names it. What a tag named is kept
	// for hours; a test does not wait them out.
	reg.set("postgres:16-alpine", digestOf("b"))
	reg.set("postgres:16.16-alpine", digestOf("b"))
	s.tagDigests = nil
	rel, _ = s.DatabaseRelease(ctx(), id, "pg")
	if rel.Update == nil || rel.Update.Label != "PostgreSQL 16.16" || rel.Update.Image != "postgres:16-alpine" ||
		!rel.Update.Backup {
		t.Fatalf("update = %+v", rel.Update)
	}

	before := lastDeployment(t, s, id, "pg")
	u, err := s.UpdateDatabase(ctx(), id, "pg", "ana@example.com")
	if err != nil || !u.Backup {
		t.Fatalf("update: %+v, %v", u, err)
	}
	if got := lastDeployment(t, s, id, "pg"); got.ID != before.ID {
		t.Fatalf("deployed %q before the backup finished", got.Revision)
	}
	job := orch.only(t, "-backup-")
	over, _ := s.Backups(ctx(), id, "pg")
	if len(over.Items) != 1 || over.Items[0].Trigger != BackupBeforeUpdate {
		t.Fatalf("backups = %+v", over.Items)
	}

	orch.finish(job.Name, orchestrator.JobState{Message: "4096"})
	s.settleBackups(ctx(), uuid.Nil)
	dep := lastDeployment(t, s, id, "pg")
	if dep.Revision != DatabaseUpdateTriggerPrefix+"PostgreSQL 16.16" || dep.Image != "postgres:16-alpine" {
		t.Fatalf("deployment = %+v", dep)
	}
	// A second settle — another engine process — deploys nothing more.
	s.settleBackups(ctx(), uuid.Nil)
	if again := lastDeployment(t, s, id, "pg"); again.ID != dep.ID {
		t.Fatalf("the update deployed twice: %+v", again)
	}
	if err := executeNamedOperation(s, ctx(), id, "pg"); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Get(ctx(), id, "pg")
	rel2, err := s.ReleaseByID(ctx(), id, a.ID, *a.ActiveReleaseID)
	if err != nil || rel2.ImageDigest != digestOf("b") {
		t.Fatalf("active release runs %q, %v; want the new image", rel2.ImageDigest, err)
	}
	if got := lastDeployment(t, s, id, "pg"); got.Status != DeploySucceeded {
		t.Fatalf("update deployment %s: %s", got.Status, got.Message)
	}

	running(s, a, "16.16")
	rel, _ = s.DatabaseRelease(ctx(), id, "pg")
	if rel.Update != nil || rel.Version != "16.16" || rel.NextMajor != "17" {
		t.Fatalf("after: update %+v, version %q, next %q", rel.Update, rel.Version, rel.NextMajor)
	}
}

// A database pinned to 16.4 moves to the 16.16 tag, never to 17's. Without
// backups it deploys straight away.
func TestUpdateMovesAPinnedTagWithinItsMajorVersion(t *testing.T) {
	reg := pgRegistry()
	reg.set("postgres:16.16-alpine", digestOf("c"))
	s, _, id := databaseService(t, "")
	s = NewService(s.pool, s.orch, s.log, func() Options { o := s.opts; o.Manifests = reg; return o }())
	a, err := createAndDeploy(t, s, ctx(), id, CreateInput{Source: SourcePostgres, Name: "pg"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.q.SetAppImage(ctx(), dbgen.SetAppImageParams{OwnerID: id, ID: a.ID, Image: "postgres:16.4-alpine"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Redeploy(ctx(), id, "pg"); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx(), id, "pg"); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Get(ctx(), id, "pg")
	running(s, a, "16.4")

	u, err := s.UpdateDatabase(ctx(), id, "pg", "")
	if err != nil || u.Backup || u.Image != "postgres:16.16-alpine" || u.Label != "PostgreSQL 16.16" {
		t.Fatalf("update = %+v, %v", u, err)
	}
	dep := lastDeployment(t, s, id, "pg")
	if dep.Revision != DatabaseUpdateTriggerPrefix+"PostgreSQL 16.16" || dep.Image != "postgres:16.16-alpine" {
		t.Fatalf("deployment = %+v", dep)
	}
	if a, _ = s.Get(ctx(), id, "pg"); a.Image != "postgres:16.16-alpine" {
		t.Fatalf("image = %q", a.Image)
	}
	// While it deploys, the database still runs what the active release
	// names, whatever the app's image says now. Read off the tag, as for a
	// database that cannot be asked.
	s.dbVersions = nil
	if rel, _ := s.DatabaseRelease(ctx(), id, "pg"); rel.Version != "16.4" {
		t.Fatalf("mid-update the version reads %q, want the active release's 16.4", rel.Version)
	}
}

// A backup that fails deploys nothing, and the deployments list says why.
func TestAFailedBackupTakesNoUpdate(t *testing.T) {
	reg := pgRegistry()
	s, orch, id := backupService(t, func(o *Options) { o.Manifests = reg })
	a, err := createAndDeploy(t, s, ctx(), id, CreateInput{Source: SourcePostgres, Name: "pg"})
	if err != nil {
		t.Fatal(err)
	}
	running(s, a, "16.15")
	reg.set("postgres:16-alpine", digestOf("b"))
	reg.set("postgres:16.16-alpine", digestOf("b"))

	if _, err := s.UpdateDatabase(ctx(), id, "pg", ""); err != nil {
		t.Fatal(err)
	}
	orch.finish(orch.only(t, "-backup-").Name, orchestrator.JobState{Failed: true, Reason: "the dump step exited 1"})
	s.settleBackups(ctx(), uuid.Nil)
	dep := lastDeployment(t, s, id, "pg")
	if dep.Revision != DatabaseUpdateTriggerPrefix+"PostgreSQL 16.16" || dep.Status != DeployFailed ||
		!strings.Contains(dep.Message, "backup taken first failed") {
		t.Fatalf("deployment = %+v", dep)
	}
	after, _ := s.Get(ctx(), id, "pg")
	if *after.ActiveReleaseID != *a.ActiveReleaseID {
		t.Fatal("a failed backup changed the release")
	}
}
