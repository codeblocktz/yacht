package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// machines is an orchestrator with nodes, and nothing else a test here needs.
type machines struct {
	*orchestrator.Noop
	nodes []orchestrator.NodeInfo
}

func (m machines) Nodes(context.Context) ([]orchestrator.NodeInfo, error) { return m.nodes, nil }

// What Kilicore reads to decide how many more of a plan it can sell: the
// install's room under its policy, from the engine it composed.
func TestAWrapperReadsTheInstallsCapacity(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run engine composition tests")
	}
	ctx := context.Background()
	e, err := New(ctx, Config{
		DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
		OwnerID: "kilicore-capacity", OwnerName: "Kilicore", MaxConcurrentBuilds: 2,
	}, Overrides{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Orchestrator: machines{Noop: orchestrator.NewNoop(), nodes: []orchestrator.NodeInfo{
			{Name: "a", Ready: true, CPUAllocatableMillis: 4000, MemAllocatableBytes: 8 << 30},
			{Name: "b", Ready: true, CPUAllocatableMillis: 4000, MemAllocatableBytes: 8 << 30},
			// Cordoned: nothing new lands here, so none of it is for sale.
			{Name: "c", Ready: true, Unschedulable: true, CPUAllocatableMillis: 4000, MemAllocatableBytes: 8 << 30},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Closed after the policy is put back, which needs the pool.
	t.Cleanup(e.Close)
	defaults := func() {
		if err := e.Apps.SetCapacityPolicy(context.Background(), CapacityPolicy{
			CPURatio: 1, MemoryRatio: 1, WarnPercent: 80, WakeReservePercent: 25,
		}); err != nil {
			t.Errorf("reset the capacity policy: %v", err)
		}
	}
	defaults()
	t.Cleanup(defaults)

	snap, err := e.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if !snap.Known || snap.Enforced || snap.Nodes != 3 || snap.Schedulable != 2 {
		t.Fatalf("snapshot = %+v; want known, not enforced by default, 2 of 3 nodes schedulable", snap)
	}
	if snap.CPU.Allocatable != 8000 || snap.CPU.Sellable != 8000 || snap.Memory.Sellable != 16<<30 {
		t.Fatalf("CPU %+v, memory %+v; want 8 vCPU and 16 GiB sellable at the defaults", snap.CPU, snap.Memory)
	}
	if snap.CPU.Free != max(snap.CPU.Sellable-snap.CPU.Committed, 0) || snap.Storage.Counted {
		t.Fatalf("free = %d of %d sellable with %d committed; storage counted = %v",
			snap.CPU.Free, snap.CPU.Sellable, snap.CPU.Committed, snap.Storage.Counted)
	}

	// The operator oversells CPU twice, keeps a quarter back, and enforces.
	if err := e.Apps.SetCapacityPolicy(ctx, CapacityPolicy{
		Enforce: true, CPURatio: 2, MemoryRatio: 1, ReservePercent: 25, WarnPercent: 90,
		StorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("SetCapacityPolicy: %v", err)
	}
	snap, err = e.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if !snap.Enforced || snap.CPU.Sellable != 12000 || snap.Memory.Sellable != 12<<30 ||
		snap.Storage.Sellable != 1<<40 || snap.WarnPercent != 90 {
		t.Fatalf("snapshot = %+v; want enforced, 12 vCPU and 12 GiB and 1 TiB sellable, warned at 90%%", snap)
	}
	switch snap.Level {
	case CapacityOK, CapacityWarn, CapacityFull:
	default:
		t.Fatalf("level = %q on a cluster that was read", snap.Level)
	}
}
