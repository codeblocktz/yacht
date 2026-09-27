package app

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The rule every entry point leans on, as for a quota: refuse only a rise, and
// only past what is sellable.
func TestCapacityOnlyRefusesARisePastWhatIsSellable(t *testing.T) {
	const cpu, mem = 1000, 1 << 30
	for _, tc := range []struct {
		name      string
		storage   int64
		now, next Usage
		want      shortfall
		refused   bool
	}{
		{"under", 0, Usage{CPUMillis: 200}, Usage{CPUMillis: 900}, shortfall{}, false},
		{"exactly at it", 0, Usage{CPUMillis: 200}, Usage{CPUMillis: 1000}, shortfall{}, false},
		{"past it", 0, Usage{CPUMillis: 800}, Usage{CPUMillis: 1300}, shortfall{"cpu", 300}, true},
		// Already past a lowered ratio: the whole rise is short, not only
		// the part past the line.
		{"already over and rising", 0, Usage{MemoryBytes: 2 << 30}, Usage{MemoryBytes: 3 << 30},
			shortfall{"memory", 1 << 30}, true},
		// The way back under must stay open.
		{"already over and falling", 0, Usage{CPUMillis: 1500}, Usage{CPUMillis: 1200}, shortfall{}, false},
		{"already over and unchanged", 0, Usage{CPUMillis: 1500}, Usage{CPUMillis: 1500}, shortfall{}, false},
		{"storage not counted", 0, Usage{}, Usage{StorageBytes: 1 << 40}, shortfall{}, false},
		{"storage counted", 10 << 30, Usage{StorageBytes: 9 << 30}, Usage{StorageBytes: 12 << 30},
			shortfall{"storage", 2 << 30}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, refused := capacityAdmit(cpu, mem, tc.storage, tc.now, tc.next)
			if refused != tc.refused || got != tc.want {
				t.Fatalf("capacityAdmit = %+v, %v; want %+v, %v", got, refused, tc.want, tc.refused)
			}
		})
	}
}

// Said to a customer, so it says what was short and what to try — and nothing
// about how much of the install anybody else holds.
func TestACapacityRefusalSaysHowMuchWasShortAndNothingElse(t *testing.T) {
	err := shortfall{"memory", 3 << 29}.refusal()
	if !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("refusal = %v, want ErrCapacityFull", err)
	}
	msg := err.Error()
	for _, want := range []string{"no room on this install", "1.5 GiB more memory than is free",
		"have been told", "fewer replicas or smaller limits"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not say %q", msg, want)
		}
	}
	for _, leak := range []string{"committed", "sellable", "team", "of "} {
		if strings.Contains(msg, leak) {
			t.Errorf("refusal %q mentions %q — the install's totals are not the customer's", msg, leak)
		}
	}
	if msg := (shortfall{"storage", 1 << 30}).refusal().Error(); !strings.Contains(msg, "smaller volume") {
		t.Errorf("a storage refusal %q does not suggest a smaller volume", msg)
	}
}

func TestSellableIsAllocatableTimesTheRatioLessTheReserve(t *testing.T) {
	room := Room{Known: true, CPUMillis: 4000, MemoryBytes: 8 << 30}
	p := CapacityPolicy{CPURatio: 2, MemoryRatio: 1, ReservePercent: 25, StorageBytes: 100 << 30}
	cpu, mem, storage := p.sellable(room)
	if cpu != 6000 || mem != 6<<30 || storage != 100<<30 {
		t.Fatalf("sellable = %dm, %d, %d; want 6000m (4 × 2 × 0.75), 6 GiB, and the 100 GiB entered",
			cpu, mem, storage)
	}
}

func TestACapacityPolicyIsValidated(t *testing.T) {
	if err := DefaultCapacityPolicy().Validate(); err != nil {
		t.Fatalf("the default policy is refused: %v", err)
	}
	d := DefaultCapacityPolicy()
	if d.Enforce || d.CPURatio != 1 || d.MemoryRatio != 1 || d.ReservePercent != 0 ||
		d.WarnPercent != 80 || d.StorageBytes != 0 {
		t.Fatalf("defaults = %+v; want off, 1, 1, 0%%, 80%%, storage not counted", d)
	}
	for name, change := range map[string]func(*CapacityPolicy){
		"a ratio below 1":      func(p *CapacityPolicy) { p.CPURatio = 0.5 },
		"a ratio above 10":     func(p *CapacityPolicy) { p.MemoryRatio = 11 },
		"a ratio of NaN":       func(p *CapacityPolicy) { p.CPURatio = math.NaN() },
		"a negative reserve":   func(p *CapacityPolicy) { p.ReservePercent = -1 },
		"a reserve of 95":      func(p *CapacityPolicy) { p.ReservePercent = 95 },
		"a warning of 0":       func(p *CapacityPolicy) { p.WarnPercent = 0 },
		"a warning past 100":   func(p *CapacityPolicy) { p.WarnPercent = 101 },
		"negative storage":     func(p *CapacityPolicy) { p.StorageBytes = -1 },
		"zero ratio (unset)":   func(p *CapacityPolicy) { p.CPURatio = 0 },
		"zero warning (unset)": func(p *CapacityPolicy) { *p = CapacityPolicy{CPURatio: 1, MemoryRatio: 1} },
	} {
		p := DefaultCapacityPolicy()
		change(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	ok := CapacityPolicy{Enforce: true, CPURatio: 2.5, MemoryRatio: 1.2, ReservePercent: 20,
		WarnPercent: 90, StorageBytes: 1 << 40}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a sensible policy was refused: %v", err)
	}
}

func TestTheSnapshotSaysHowFullTheInstallIs(t *testing.T) {
	room := Room{Known: true, Nodes: 2, Schedulable: 2, CPUMillis: 4000, MemoryBytes: 8 << 30}
	p := DefaultCapacityPolicy()

	s := NewCapacitySnapshot(p, room, Usage{CPUMillis: 1000, MemoryBytes: 1 << 30}, 0)
	if s.Level != CapacityOK || s.CPU.Free != 3000 || s.Memory.Free != 7<<30 || s.CPU.Percent() != 25 {
		t.Fatalf("a quarter full = %s, free %dm and %d; want ok, 3000m and 7 GiB", s.Level, s.CPU.Free, s.Memory.Free)
	}
	if s.Storage.Counted || s.RoomShort {
		t.Error("storage with no capacity entered is counted, or room is short while not enforcing")
	}

	if s := NewCapacitySnapshot(p, room, Usage{CPUMillis: 3200}, 0); s.Level != CapacityWarn {
		t.Errorf("80%% of CPU = %s, want warn", s.Level)
	}
	s = NewCapacitySnapshot(p, room, Usage{MemoryBytes: 9 << 30}, 2)
	if s.Level != CapacityFull || s.Memory.Free != 0 || s.RefusedLast24h != 2 {
		t.Errorf("memory past sellable = %s, free %d; want full and none free", s.Level, s.Memory.Free)
	}

	// Every node cordoned is full, however little is committed.
	if s := NewCapacitySnapshot(p, Room{Known: true, Nodes: 1}, Usage{}, 0); s.Level != CapacityFull {
		t.Errorf("no schedulable node = %s, want full", s.Level)
	}
	// An unread cluster outranks a storage figure that would say full.
	p.StorageBytes = 1 << 30
	if s := NewCapacitySnapshot(p, Room{Reason: "down"}, Usage{StorageBytes: 2 << 30}, 0); s.Level != CapacityUnknown || s.Known {
		t.Errorf("an unread cluster = %s, want unknown", s.Level)
	}

	// Short of room, told to customers only while enforcing.
	p = DefaultCapacityPolicy()
	p.Enforce = true
	if s := NewCapacitySnapshot(p, room, Usage{CPUMillis: 1000}, 0); s.RoomShort {
		t.Error("room is short with three quarters of the CPU free")
	}
	if s := NewCapacitySnapshot(p, room, Usage{CPUMillis: 3950}, 0); !s.RoomShort {
		t.Error("room is not short with less than a default container of CPU free")
	}
	if s := NewCapacitySnapshot(p, Room{Reason: "down"}, Usage{CPUMillis: 3950}, 0); s.RoomShort {
		t.Error("room is short on a cluster nobody could read")
	}
}

// Admission must not call the Kubernetes API on every change, and a cluster
// that cannot be read is unknown — logged when it becomes so, not every time.
func TestTheRoomIsReadOnceAPeriodAndAnUnreadableClusterIsLoggedOnce(t *testing.T) {
	var reads atomic.Int32
	var fail atomic.Bool
	clock := time.Unix(0, 0)
	var logged countingHandler
	r := &roomSource{
		log: slog.New(&logged),
		now: func() time.Time { return clock },
		read: func(context.Context) ([]orchestrator.NodeInfo, error) {
			reads.Add(1)
			if fail.Load() {
				return nil, errors.New("connection refused")
			}
			return []orchestrator.NodeInfo{{Name: "a", Ready: true, CPUAllocatableMillis: 2000}}, nil
		},
	}
	ctx := context.Background()

	for range 3 {
		if room := r.get(ctx); !room.Known || room.CPUMillis != 2000 {
			t.Fatalf("room = %+v, want the one node's 2000m", room)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("three readings inside the TTL read the cluster %d times, want once", reads.Load())
	}

	fail.Store(true)
	for range 3 {
		clock = clock.Add(roomTTL)
		if room := r.get(ctx); room.Known || !strings.Contains(room.Reason, "connection refused") {
			t.Fatalf("an unreadable cluster = %+v, want unknown with the reason", room)
		}
	}
	if logged.warns.Load() != 1 {
		t.Fatalf("an unreadable cluster was logged %d times across three readings, want once", logged.warns.Load())
	}

	// No nodes at all is an orchestrator with no cluster, not a full one.
	if room := RoomOf(nil); room.Known {
		t.Error("no nodes reads as a known, empty cluster")
	}
}

type countingHandler struct{ warns atomic.Int32 }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.warns.Add(1)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// ------------------------------------------------ against a real database

// capacityService is a service whose cluster is whatever the test says, with
// the policy put back and refusals written before the pool closes.
//
// The database is shared with every other test, and the install's committed
// use is every team's in it — so each test sizes its room from what is
// already there rather than assuming it starts empty.
func capacityService(t *testing.T) *Service {
	t.Helper()
	s, _, _ := testService(t, Options{})
	t.Cleanup(func() {
		s.refusals.Wait()
		if err := s.SetCapacityPolicy(context.Background(), DefaultCapacityPolicy()); err != nil {
			t.Errorf("restore the capacity policy: %v", err)
		}
	})
	return s
}

// setRoom gives the install one schedulable node with this much allocatable,
// and forgets whatever was read before.
func setRoom(s *Service, cpuMillis, memoryBytes int64) {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	s.room.read = func(context.Context) ([]orchestrator.NodeInfo, error) {
		return []orchestrator.NodeInfo{{Name: "node", Ready: true,
			CPUAllocatableMillis: cpuMillis, MemAllocatableBytes: memoryBytes}}, nil
	}
	s.room.at = time.Time{}
}

// roomAbove gives the install this much room past what it has committed now.
func roomAbove(t *testing.T, s *Service, cpuMillis, memoryBytes int64) {
	t.Helper()
	u, err := s.installUsageWith(context.Background(), s.q, nil, DefaultCapacityPolicy().WakeReservePercent)
	if err != nil {
		t.Fatalf("install usage: %v", err)
	}
	setRoom(s, u.CPUMillis+cpuMillis, u.MemoryBytes+memoryBytes)
}

func enforce(t *testing.T, s *Service, on bool) {
	t.Helper()
	p := DefaultCapacityPolicy()
	p.Enforce = on
	if err := s.SetCapacityPolicy(context.Background(), p); err != nil {
		t.Fatalf("SetCapacityPolicy: %v", err)
	}
}

func wantCapacityRefusal(t *testing.T, what string, err error, mentions string) {
	t.Helper()
	if !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("%s = %v, want ErrCapacityFull", what, err)
	}
	if !strings.Contains(err.Error(), mentions) {
		t.Errorf("%s refused with %q, which does not say it was %s", what, err, mentions)
	}
}

func TestTheInstallIsHeldToWhatItSells(t *testing.T) {
	ctx := context.Background()
	s := capacityService(t)
	id := owner(t, s, s.pool, "svc-capacity-admit")
	since := time.Now().Add(-time.Second)

	roomAbove(t, s, 1000, 1<<30)
	enforce(t, s, true)

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 2, Port: 8080,
		CPULimit: "400m", MemoryLimit: "256Mi",
	}); err != nil {
		t.Fatalf("a create inside the room: %v", err)
	}
	_, err := s.Create(ctx, id, CreateInput{
		Name: "wide", Image: "nginx:alpine", Replicas: 1, CPULimit: "300m", MemoryLimit: "128Mi",
	})
	wantCapacityRefusal(t, "300m more with 200m free", err, "0.1 vCPU more CPU")
	if n, _ := s.Count(ctx, id); n != 1 {
		t.Fatalf("the refused create left %d apps, want 1", n)
	}
	_, err = s.Scale(ctx, id, "web", 3)
	wantCapacityRefusal(t, "a third replica with 200m free", err, "CPU")

	// Scaling down, lowering limits, and changes that raise nothing all go
	// through — here with the room shrunk under what is already committed.
	roomAbove(t, s, -300, 0)
	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: "nginx:1.27", Port: 8080, CPULimit: "400m", MemoryLimit: "256Mi",
	}); err != nil {
		t.Fatalf("an image change over capacity was refused: %v", err)
	}
	mustExecute(t, s, id, "web")
	if _, err := s.Scale(ctx, id, "web", 1); err != nil {
		t.Fatalf("scaling down over capacity was refused: %v", err)
	}
	mustExecute(t, s, id, "web")
	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: "nginx:1.27", Port: 8080, CPULimit: "200m", MemoryLimit: "256Mi",
	}); err != nil {
		t.Fatalf("lowering a limit over capacity was refused: %v", err)
	}
	mustExecute(t, s, id, "web")
	if err := s.Delete(ctx, id, "web"); err != nil {
		t.Fatalf("deleting over capacity was refused: %v", err)
	}

	// Each refusal is recorded for the operator, with who and how short.
	s.refusals.Wait()
	refusals, err := s.CapacityRefusals(ctx, since, 50)
	if err != nil {
		t.Fatalf("CapacityRefusals: %v", err)
	}
	var mine []CapacityRefusal
	for _, r := range refusals {
		if r.TeamID == id {
			mine = append(mine, r)
		}
	}
	if len(mine) != 2 || mine[0].Resource != "cpu" || mine[1].Shortfall != 100 {
		t.Fatalf("refusals recorded = %+v, want the two CPU refusals, 100m and 200m short", mine)
	}
	snap, err := s.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if snap.RefusedLast24h < 2 || !snap.Enforced || !snap.Known {
		t.Fatalf("snapshot = %+v, want enforced, known, and the refusals counted", snap)
	}
}

// Off is the default, and off is exactly what the install did before.
func TestAnInstallNotEnforcingRefusesNothingForCapacity(t *testing.T) {
	ctx := context.Background()
	s := capacityService(t)
	id := owner(t, s, s.pool, "svc-capacity-off")

	roomAbove(t, s, 0, 0)
	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 10, CPULimit: "2", MemoryLimit: "4Gi",
	}); err != nil {
		t.Fatalf("a create past capacity on an install not enforcing: %v", err)
	}
}

// A cluster that cannot be read lets changes through rather than stopping
// every customer deploying while the API server is away.
func TestAnUnreadableClusterFailsOpen(t *testing.T) {
	ctx := context.Background()
	s := capacityService(t)
	id := owner(t, s, s.pool, "svc-capacity-unknown")
	enforce(t, s, true)

	s.room.mu.Lock()
	s.room.read = func(context.Context) ([]orchestrator.NodeInfo, error) {
		return nil, errors.New("the server has asked for the client to provide credentials")
	}
	s.room.at = time.Time{}
	s.room.mu.Unlock()

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 10, CPULimit: "2",
	}); err != nil {
		t.Fatalf("a create with the cluster unreadable: %v", err)
	}
	if snap, err := s.Capacity(ctx); err != nil || snap.Known || snap.Level != CapacityUnknown {
		t.Fatalf("snapshot = %+v, %v; want unknown", snap, err)
	}
}

// The reason the policy row is locked. Two creates that each fit in the last
// of the room must not both be admitted.
func TestConcurrentCreatesCannotBothTakeTheLastRoom(t *testing.T) {
	ctx := context.Background()
	s := capacityService(t)
	id := owner(t, s, s.pool, "svc-capacity-race")

	roomAbove(t, s, 500, 512<<20)
	enforce(t, s, true)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Create(ctx, id, CreateInput{
				Name: "racer-" + string(rune('a'+i)), Image: "nginx:alpine", Replicas: 1,
				CPULimit: "500m", MemoryLimit: "512Mi",
			})
		}()
	}
	wg.Wait()

	admitted := 0
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case !errors.Is(err, ErrCapacityFull):
			t.Errorf("a racing create failed for another reason: %v", err)
		}
	}
	if n, _ := s.Count(ctx, id); admitted != 1 || n != 1 {
		t.Fatalf("%d creates admitted and %d apps stored in room for one", admitted, n)
	}
}

// Storage is counted only where the operator has said how much there is.
func TestStorageIsHeldToTheCapacityEntered(t *testing.T) {
	ctx := context.Background()
	s := capacityService(t)
	id := owner(t, s, s.pool, "svc-capacity-storage")

	roomAbove(t, s, 1000, 1<<30)
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "db", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	u, err := s.installUsageWith(ctx, s.q, nil, DefaultCapacityPolicy().WakeReservePercent)
	if err != nil {
		t.Fatalf("install usage: %v", err)
	}
	p := DefaultCapacityPolicy()
	p.Enforce, p.StorageBytes = true, u.StorageBytes+(1<<30)
	if err := s.SetCapacityPolicy(ctx, p); err != nil {
		t.Fatalf("SetCapacityPolicy: %v", err)
	}

	_, err = s.AttachVolume(ctx, id, "db", VolumeInput{Name: "data", MountPath: "/data", SizeBytes: 2 << 30})
	wantCapacityRefusal(t, "2 GiB of volume with 1 GiB free", err, "1 GiB more storage")
	if _, err := s.AttachVolume(ctx, id, "db", VolumeInput{
		Name: "data", MountPath: "/data", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("a volume landing exactly on the capacity: %v", err)
	}
}
