package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// erasingOrchestrator records what it is asked to erase, and answers as told.
type erasingOrchestrator struct {
	*recordingOrchestrator
	mu     sync.Mutex
	erased []string
	answer error
}

func (o *erasingOrchestrator) EraseVolume(ctx context.Context, ref orchestrator.Ref, volume string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.answer != nil {
		return o.answer
	}
	o.erased = append(o.erased, ref.Name+"/"+volume)
	return o.recordingOrchestrator.EraseVolume(ctx, ref, volume)
}

// cannotErase is a cluster that cannot erase a volume.
type cannotErase struct{ orchestrator.Orchestrator }

// A stopped database's data volume is erased; nothing else about it changes:
// the app, its settings and variables, and the volume, still attached at the
// same path and size. Running, or with a deploy in flight, it is refused;
// erased again, nothing happens.
func TestEraseVolumeDeletesTheDataAndKeepsTheApp(t *testing.T) {
	ctx := context.Background()
	s, rec, pool := testService(t, Options{Keeper: testKeeper(t)})
	orch := &erasingOrchestrator{recordingOrchestrator: rec}
	s.orch = orch
	id := owner(t, s, pool, "erase-volume")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "pg", Replicas: 1}); err != nil {
		t.Fatal(err)
	}

	if err := s.EraseVolume(ctx, id, "pg", "data"); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("erasing a running database's volume: %v, want ErrVolumeInUse", err)
	}
	if err := s.EraseVolume(ctx, id, "pg", "nope"); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("erasing a volume it does not have: %v", err)
	}

	// Scaled to zero, with that deploy still in flight: refused.
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
	if len(orch.erased) != 2 || orch.erased[0] != "pg/data" {
		t.Errorf("the cluster was asked to erase %v", orch.erased)
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

	// Started again, it runs with the volume as it was defined.
	if _, err := s.Scale(ctx, id, "pg", 1); err != nil {
		t.Fatal(err)
	}
	mustExecute(t, s, id, "pg")
	if spec := rec.lastAppSpec(); spec.Replicas != 1 || len(spec.Volumes) != 1 || spec.Volumes[0].MountPath != before.Volumes[0].MountPath {
		t.Errorf("started again with %+v", spec.Volumes)
	}
}

// What the cluster refuses is said as the engine's own errors, and a
// cluster that cannot erase says so.
func TestEraseVolumeSaysWhatTheClusterRefused(t *testing.T) {
	ctx := context.Background()
	s, rec, pool := testService(t, Options{})
	orch := &erasingOrchestrator{recordingOrchestrator: rec}
	s.orch = orch
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
		orch.answer = answer
		if err := s.EraseVolume(ctx, id, "web", "data"); !errors.Is(err, want) {
			t.Errorf("the cluster answering %v: %v, want %v", answer, err, want)
		}
	}
	s.orch = cannotErase{rec}
	if err := s.EraseVolume(ctx, id, "web", "data"); !errors.Is(err, orchestrator.ErrNotSupported) {
		t.Errorf("a cluster that cannot erase: %v", err)
	}
}
