package app

import (
	"context"
	"errors"
	"testing"
)

// A team's pool reaches what is already running: the change bumps each of its
// apps, and the reconciler re-applies them with the pool — not the request
// that changed it. Setting the same pool again rolls nothing.
func TestATeamsPoolRollsItsRunningApps(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{})
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 2, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Pool != "" {
		t.Fatalf("a team never placed was applied on pool %q", spec.Pool)
	}
	if p, err := s.TeamPool(ctx, ownerID); err != nil || p != "" {
		t.Fatalf("TeamPool = %q, %v; want anywhere", p, err)
	}

	before := orch.applyCalls.Load()
	if err := s.SetTeamPool(ctx, ownerID, "pro"); err != nil {
		t.Fatalf("SetTeamPool: %v", err)
	}
	if got := orch.applyCalls.Load(); got != before {
		t.Fatalf("the request applied %d workloads, want the reconciler to", got-before)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if got := orch.applyCalls.Load(); got != before+1 {
		t.Fatalf("reconciler applied %d workloads, want the one app", got-before)
	}
	spec := orch.lastAppSpec()
	if spec.Pool != "pro" || spec.Replicas != 2 || spec.ConfigVersion <= a.ConfigVersion {
		t.Fatalf("applied pool %q x%d at version %d; want pro x2 past %d",
			spec.Pool, spec.Replicas, spec.ConfigVersion, a.ConfigVersion)
	}
	if p, _ := s.TeamPool(ctx, ownerID); p != "pro" {
		t.Errorf("TeamPool = %q, want pro", p)
	}
	if u, err := s.TeamUsage(ctx, ownerID); err != nil || u.Pool != "pro" {
		t.Errorf("TeamUsage pool = %q, %v; want the operator to see pro", u.Pool, err)
	}

	// The same pool again is nothing to do.
	version := mustGet(t, s, ownerID, "web").ConfigVersion
	if err := s.SetTeamPool(ctx, ownerID, " pro "); err != nil {
		t.Fatalf("SetTeamPool again: %v", err)
	}
	if got := mustGet(t, s, ownerID, "web").ConfigVersion; got != version {
		t.Fatalf("setting the pool the team had bumped its app %d -> %d", version, got)
	}

	// An app created on the pool is deployed onto it.
	if _, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "api", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create on the pool: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Name != "api" || spec.Pool != "pro" {
		t.Fatalf("a new app was deployed as %s on %q, want api on pro", spec.Name, spec.Pool)
	}

	// Anywhere again takes it off.
	if err := s.SetTeamPool(ctx, ownerID, ""); err != nil {
		t.Fatalf("SetTeamPool anywhere: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Pool != "" {
		t.Fatalf("pool cleared, still applied on %q", spec.Pool)
	}
}

// An app at zero replicas is re-applied at zero: it picks the pool up in its
// template, and starts on the pool when it is next scaled up or woken.
func TestAStoppedAppTakesThePoolWithoutStarting(t *testing.T) {
	ctx := context.Background()
	s, orch, ownerID := convergenceService(t, Options{})
	if _, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Scale(ctx, ownerID, "web", 0); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if err := executeNamedOperation(s, ctx, ownerID, "web"); err != nil {
		t.Fatalf("scale operation: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Replicas != 0 {
		t.Fatalf("scaled to %d, want none", spec.Replicas)
	}
	if err := s.SetTeamPool(ctx, ownerID, "hobby"); err != nil {
		t.Fatalf("SetTeamPool: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Pool != "hobby" || spec.Replicas != 0 {
		t.Fatalf("applied %d replicas on %q; want none, on hobby", spec.Replicas, spec.Pool)
	}
}

// A pool is held to the rule a node's label is, and only a team can have one.
func TestATeamPoolIsChecked(t *testing.T) {
	ctx := context.Background()
	s, _, ownerID := convergenceService(t, Options{})
	for _, bad := range []string{"Pro", "pro pool", "pro;id", "-pro"} {
		if err := s.SetTeamPool(ctx, ownerID, bad); err == nil {
			t.Errorf("pool %q was accepted", bad)
		}
	}
	if err := s.SetTeamPool(ctx, "svc-pool-no-such-team", "pro"); !errors.Is(err, ErrNoSuchTeam) {
		t.Errorf("a pool for a team that does not exist = %v, want ErrNoSuchTeam", err)
	}
}
