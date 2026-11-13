package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/database"
)

// blockingBrowser holds every read until it is let go, so a test can fill
// the gate's slots and see what the next read is told.
type blockingBrowser struct {
	database.Browser
	entered chan struct{}
	release chan struct{}
}

func (b blockingBrowser) Query(context.Context, string) (database.Result, error) {
	b.entered <- struct{}{}
	<-b.release
	return database.Result{Command: "SELECT"}, nil
}

// One team's tabs cannot take every slot, and the engine as a whole holds a
// bounded number of reads: past either, a read waits a moment and is then
// refused, while another team's read still runs. A slot given back is a slot
// the next read gets.
func TestDataBrowserReadsAreBoundedPerTeamAndOverall(t *testing.T) {
	gate := newBrowseGate(2, 3, 50*time.Millisecond)
	inner := blockingBrowser{entered: make(chan struct{}, 8), release: make(chan struct{})}
	team := func(id string) gatedBrowser { return gatedBrowser{b: inner, gate: gate, ownerID: id} }
	ctx := context.Background()

	done := make(chan error, 8)
	run := func(b gatedBrowser) {
		go func() { _, err := b.Query(ctx, "SELECT 1"); done <- err }()
		<-inner.entered
	}
	run(team("a"))
	run(team("a"))

	if _, err := team("a").Query(ctx, "SELECT 1"); !errors.Is(err, ErrDatabaseBusy) {
		t.Fatalf("a third read for one team gave %v, want ErrDatabaseBusy", err)
	}
	// Another team still has its own slots, while the engine has one left.
	run(team("b"))
	if _, err := team("c").Query(ctx, "SELECT 1"); !errors.Is(err, ErrDatabaseBusy) {
		t.Fatalf("a read past the engine's total gave %v, want ErrDatabaseBusy", err)
	}

	for range 3 {
		inner.release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatalf("a held read: %v", err)
		}
	}
	go func() { inner.release <- struct{}{} }()
	go func() { <-inner.entered }()
	if _, err := team("a").Query(ctx, "SELECT 1"); err != nil {
		t.Errorf("a read once the slots were given back: %v", err)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.teams) != 0 {
		t.Errorf("%d teams still hold slot state with nothing running", len(gate.teams))
	}
}

// A read waiting for a slot ends with its request: a page whose visitor has
// gone does not sit on the queue for the rest of the wait.
func TestAReadWaitingForASlotEndsWithItsRequest(t *testing.T) {
	gate := newBrowseGate(1, 4, time.Minute)
	release, err := gate.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err = gate.acquire(ctx, "a")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait gave %v, want context.Canceled", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("a cancelled wait took %v", waited)
	}
	release()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.teams) != 0 {
		t.Errorf("%d teams still hold slot state", len(gate.teams))
	}
}
