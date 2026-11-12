package app

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// eraseCluster is a cluster as EraseVolume meets it: an app's claim mounted
// while the release it runs has replicas — none while it sleeps, as its
// Deployment would have — and gone once erased. It records what it erased,
// and whether the app's convergence lock was held when it was asked.
type eraseCluster struct {
	*recordingOrchestrator
	s *Service

	mu   sync.Mutex
	gone map[string]bool
	// kept are claims a detached volume left behind; an attached one's
	// claim is there until erased, and nothing else's is.
	kept     map[string]bool
	erased   []string
	unlocked int
	answer   error
}

func newEraseCluster(s *Service, rec *recordingOrchestrator) *eraseCluster {
	c := &eraseCluster{recordingOrchestrator: rec, s: s, gone: map[string]bool{}, kept: map[string]bool{}}
	s.orch = c
	return c
}

func (c *eraseCluster) EraseVolume(ctx context.Context, ref orchestrator.Ref, volume string) (orchestrator.VolumeErasure, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	erasure := orchestrator.VolumeErasure{Claim: ref.Name + "-" + volume}
	if c.answer != nil {
		return erasure, c.answer
	}
	a, err := c.s.Get(ctx, string(ref.Owner), ref.Name)
	if err != nil {
		return erasure, err
	}
	// Held by EraseVolume's own session, the lock cannot be taken here.
	var free bool
	key := int64(binary.BigEndian.Uint64(a.ID[:8]))
	if err := c.s.pool.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&free); err != nil {
		return erasure, err
	}
	if free {
		c.unlocked++
		_, _ = c.s.pool.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key)
	}
	mounted := false
	for _, v := range a.Volumes {
		mounted = mounted || v.Name == volume
	}
	if mounted && !a.Sleep.Asleep() && a.ActiveReleaseID != nil {
		r, err := c.s.ReleaseByID(ctx, a.OwnerID, a.ID, *a.ActiveReleaseID)
		if err != nil {
			return erasure, err
		}
		if r.Replicas > 0 {
			return erasure, orchestrator.ErrVolumeInUse
		}
	}
	key2 := string(ref.Owner) + "/" + erasure.Claim
	if c.gone[key2] || (!mounted && !c.kept[erasure.Claim]) {
		return erasure, nil
	}
	c.gone[key2], erasure.Deleted = true, true
	c.erased = append(c.erased, erasure.Claim)
	return erasure, nil
}

func (c *eraseCluster) WaitVolumeGone(context.Context, orchestrator.Ref, string) error { return nil }

// cannotErase is a cluster that cannot erase a volume.
type cannotErase struct{ orchestrator.Orchestrator }

// A stopped database's data volume is erased; nothing else about it changes:
// the app, its settings and variables, and the volume, still attached at the
// same path and size. Running, with a deploy in flight, or by a name that is
// not a volume's, it is refused; erased again, nothing happens.
func TestEraseVolumeDeletesTheDataAndKeepsTheApp(t *testing.T) {
	ctx := context.Background()
	s, rec, pool := testService(t, Options{Keeper: testKeeper(t)})
	cluster := newEraseCluster(s, rec)
	id := owner(t, s, pool, "erase-volume")
	other := owner(t, s, pool, "erase-volume-other")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "pg", Replicas: 1}); err != nil {
		t.Fatal(err)
	}

	if err := s.EraseVolume(ctx, id, "pg", "data"); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("erasing a running database's volume: %v, want ErrVolumeInUse", err)
	}
	if err := s.EraseVolume(ctx, id, "pg", "nope"); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("erasing a volume it does not have: %v", err)
	}
	if err := s.EraseVolume(ctx, id, "pg", "backups"); !errors.Is(err, ErrVolumeReserved) {
		t.Fatalf("erasing the backups claim: %v", err)
	}
	if err := s.EraseVolume(ctx, other, "pg", "data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another team erasing it: %v", err)
	}

	if _, err := s.Scale(ctx, id, "pg", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.EraseVolume(ctx, id, "pg", "data"); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("erasing with a deploy in flight: %v, want ErrVolumeInUse", err)
	}
	mustExecute(t, s, id, "pg")

	before, _ := s.Get(ctx, id, "pg")
	vars := variablesOf(t, s, before)
	if err := s.EraseVolume(ctx, id, "pg", "data"); err != nil {
		t.Fatalf("EraseVolume: %v", err)
	}
	if err := s.EraseVolume(ctx, id, "pg", "data"); err != nil {
		t.Fatalf("erasing it again: %v", err)
	}
	if len(cluster.erased) != 1 || cluster.erased[0] != "pg-data" {
		t.Errorf("the cluster erased %v, want pg-data once", cluster.erased)
	}
	if cluster.unlocked != 0 {
		t.Errorf("the cluster was asked to erase %d times without the app's convergence lock held", cluster.unlocked)
	}
	after, _ := s.Get(ctx, id, "pg")
	if after.ConfigVersion != before.ConfigVersion || after.Image != before.Image ||
		after.ActiveReleaseID == nil || *after.ActiveReleaseID != *before.ActiveReleaseID {
		t.Errorf("the app changed: %+v\nwas %+v", after, before)
	}
	if len(after.Volumes) != 1 || after.Volumes[0] != before.Volumes[0] {
		t.Errorf("the volume is now %+v, was %+v", after.Volumes, before.Volumes)
	}
	afterVars := variablesOf(t, s, after)
	for k, v := range vars {
		if afterVars[k] != v {
			t.Errorf("variable %s changed", k)
		}
	}

	if _, err := s.Scale(ctx, id, "pg", 1); err != nil {
		t.Fatal(err)
	}
	mustExecute(t, s, id, "pg")
	if spec := rec.lastAppSpec(); spec.Replicas != 1 || len(spec.Volumes) != 1 || spec.Volumes[0].MountPath != before.Volumes[0].MountPath {
		t.Errorf("started again with %+v", spec.Volumes)
	}
}

// The app row says what was asked for, not what runs. A scale to zero whose
// deploy is refused leaves the row as it was; one whose deploy failed leaves
// the release running its replicas — none while the app sleeps, all of them
// when it wakes. Either way the volume is in use.
func TestEraseVolumeRefusesAnAppThatMayStillRun(t *testing.T) {
	ctx := context.Background()
	s, rec, pool := testService(t, Options{})
	newEraseCluster(s, rec)
	id := owner(t, s, pool, "erase-volume-running")
	web, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{Name: "data", MountPath: "/data", SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}

	// The reviewer's sequence: a deploy in flight refuses the scale, and the
	// row keeps its count.
	if err := s.Redeploy(ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	releases := func() int {
		rs, err := s.Releases(ctx, id, web.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs)
	}
	n := releases()
	if _, err := s.Scale(ctx, id, "web", 0); !errors.Is(err, ErrOperationInFlight) {
		t.Fatalf("scaling during a deploy: %v, want ErrOperationInFlight", err)
	}
	if got, _ := s.Get(ctx, id, "web"); got.Replicas != 1 || releases() != n {
		t.Fatalf("a refused scale left the app at %d replicas and %d releases, want 1 and %d", got.Replicas, releases(), n)
	}
	mustExecute(t, s, id, "web")

	// A scale to zero that failed: the row at zero, the release at one.
	p := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, web.ID); err != nil {
			t.Fatal(err)
		}
	}
	p(`UPDATE apps SET replicas = 0 WHERE id = $1`)
	p(`UPDATE apps SET sleep_state = 'asleep', sleeping_since = now() WHERE id = $1`)
	if err := s.EraseVolume(ctx, id, "web", "data"); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("erasing an asleep app's volume: %v, want ErrVolumeInUse", err)
	}
	p(`UPDATE apps SET sleep_state = 'awake', sleeping_since = NULL WHERE id = $1`)
	if err := s.EraseVolume(ctx, id, "web", "data"); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("erasing while the release runs a replica: %v, want ErrVolumeInUse", err)
	}

	// Stopped for real.
	p(`UPDATE apps SET replicas = 1 WHERE id = $1`)
	if _, err := s.Scale(ctx, id, "web", 0); err != nil {
		t.Fatal(err)
	}
	mustExecute(t, s, id, "web")
	if err := s.EraseVolume(ctx, id, "web", "data"); err != nil {
		t.Fatalf("erasing a stopped app's volume: %v", err)
	}
}

// A volume detached with DeleteVolume keeps its claim, which can be erased
// by its name; a name with no claim either is no volume. "backups" is no
// volume's name.
func TestADetachedVolumeCanBeErased(t *testing.T) {
	ctx := context.Background()
	s, rec, pool := testService(t, Options{})
	cluster := newEraseCluster(s, rec)
	id := owner(t, s, pool, "erase-volume-detached")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{Name: "backups", MountPath: "/b", SizeBytes: 1 << 30}); err == nil ||
		!strings.Contains(err.Error(), "backups") {
		t.Fatalf("attaching a volume named backups: %v", err)
	}
	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{Name: "cache", MountPath: "/cache", SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVolume(ctx, id, "web", "cache", true); err != nil {
		t.Fatal(err)
	}
	cluster.kept["web-cache"] = true
	// Detached, its claim is erased while the app runs: nothing mounts it.
	if err := s.EraseVolume(ctx, id, "web", "cache"); err != nil {
		t.Fatalf("erasing a detached volume: %v", err)
	}
	if len(cluster.erased) != 1 || cluster.erased[0] != "web-cache" {
		t.Errorf("erased %v", cluster.erased)
	}
	if err := s.EraseVolume(ctx, id, "web", "cache"); !errors.Is(err, ErrVolumeNotFound) {
		t.Errorf("erasing a detached volume again, with no claim: %v", err)
	}
}

// What the cluster refuses is said as the engine's own errors, and a
// cluster that cannot erase says so.
func TestEraseVolumeSaysWhatTheClusterRefused(t *testing.T) {
	ctx := context.Background()
	s, rec, pool := testService(t, Options{})
	cluster := newEraseCluster(s, rec)
	id := owner(t, s, pool, "erase-volume-refused")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{Name: "data", MountPath: "/data", SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scale(ctx, id, "web", 0); err != nil {
		t.Fatal(err)
	}
	mustExecute(t, s, id, "web")

	for answer, want := range map[error]error{
		orchestrator.ErrVolumeInUse:    ErrVolumeInUse,
		orchestrator.ErrVolumeRetained: ErrVolumeRetained,
	} {
		cluster.answer = answer
		if err := s.EraseVolume(ctx, id, "web", "data"); !errors.Is(err, want) {
			t.Errorf("the cluster answering %v: %v, want %v", answer, err, want)
		}
	}
	s.orch = cannotErase{rec}
	if err := s.EraseVolume(ctx, id, "web", "data"); !errors.Is(err, orchestrator.ErrNotSupported) {
		t.Errorf("a cluster that cannot erase: %v", err)
	}
}
