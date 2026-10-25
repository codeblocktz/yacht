package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// What a wrapper writing DNS does: describe its regions, place an app, read
// every hostname with its region's edge, and hear when one moves.
func TestAWrapperRoutesEachAppToItsRegionsEdge(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run engine composition tests")
	}
	ctx := context.Background()
	var (
		mu      sync.Mutex
		changes []RouteChange
	)
	heard := make(chan struct{}, 16)
	e, err := New(ctx, Config{
		DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
		OwnerID: "wrapper-regions", OwnerName: "Wrapper", MaxConcurrentBuilds: 2,
		AppDomain: "apps.example.test", DefaultRegion: "fsn",
	}, Overrides{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Orchestrator: machines{Noop: orchestrator.NewNoop(), nodes: []orchestrator.NodeInfo{
			{Name: "fsn-1", Ready: true, Region: "fsn", CPUAllocatableMillis: 4000, MemAllocatableBytes: 8 << 30},
			{Name: "dar-1", Ready: true, Region: "dar", CPUAllocatableMillis: 4000, MemAllocatableBytes: 8 << 30},
		}},
		Routes: RouteObserverFunc(func(_ context.Context, c RouteChange) {
			mu.Lock()
			changes = append(changes, c)
			mu.Unlock()
			heard <- struct{}{}
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(e.Close)
	clear := func() {
		if _, err := e.Pool.Exec(context.Background(), `DELETE FROM regions WHERE name IN ('fsn', 'dar')`); err != nil {
			t.Errorf("clear regions: %v", err)
		}
		if _, err := e.Pool.Exec(context.Background(), `DELETE FROM apps WHERE owner_id = 'wrapper-regions'`); err != nil {
			t.Errorf("clear apps: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	for _, r := range []Region{
		{Name: "fsn", Open: true, Edge: Edge{Host: "edge-fsn.example.test", Addresses: []string{"203.0.113.10"}}},
		{Name: "dar", Open: true, Edge: Edge{Host: "edge-dar.example.test"}},
	} {
		if err := e.Apps.SetRegion(ctx, r); err != nil {
			t.Fatalf("SetRegion: %v", err)
		}
	}
	a, err := e.Apps.Create(ctx, "wrapper-regions", app.CreateInput{Name: "shop", Image: "nginx:alpine", Port: 8080})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := e.Apps.GenerateDomain(ctx, "wrapper-regions", "shop", 0); err != nil {
		t.Fatalf("GenerateDomain: %v", err)
	}
	if region, err := e.Apps.AppRegion(ctx, "wrapper-regions", "shop"); err != nil || region != "fsn" {
		t.Fatalf("AppRegion = %q, %v; want the default", region, err)
	}

	routes, err := e.Routes(ctx)
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	var shop *Route
	for i, r := range routes {
		if r.AppID == a.ID {
			shop = &routes[i]
		}
	}
	if shop == nil || shop.Host != "shop-production.up.apps.example.test" || shop.Region != "fsn" ||
		shop.Edge.Host != "edge-fsn.example.test" || shop.Custom || !shop.Routable {
		t.Fatalf("shop's route = %+v", shop)
	}

	if err := e.Apps.MoveApp(ctx, "wrapper-regions", "shop", "dar"); err != nil {
		t.Fatalf("MoveApp: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		moved := 0
		for _, c := range changes {
			if c.Kind == RouteChangeApp && c.AppID == a.ID {
				moved++
			}
		}
		mu.Unlock()
		if moved >= 2 {
			break
		}
		select {
		case <-heard:
		case <-deadline:
			t.Fatalf("the observer heard %+v, want shop's create and its move", changes)
		}
	}
	routes, _ = e.Routes(ctx)
	for _, r := range routes {
		if r.AppID == a.ID && (r.Region != "dar" || r.Edge.Host != "edge-dar.example.test") {
			t.Fatalf("after the move shop's route = %+v", r)
		}
	}
}
