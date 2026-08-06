package app

import (
	"context"
	"os"
	"testing"
	"time"

	k8sorch "github.com/codeblocktz/yacht/internal/orchestrator/k8s"
	"github.com/codeblocktz/yacht/internal/registry"
)

// This opt-in test is the Ticket 9 acceptance path that a fake client cannot
// prove: a real Deployment controller sees its managed Deployment deleted and
// Yacht recreates it from the database without a request or Redeploy click.
func TestK3sReconcilesADeletedActiveDeployment(t *testing.T) {
	if os.Getenv("YACHT_TEST_K3S") == "" {
		t.Skip("set YACHT_TEST_K3S=1 to run against the current disposable K3s cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, _, pool := testService(t, Options{
		RolloutTimeout: 2 * time.Minute, RolloutPollInterval: 250 * time.Millisecond,
	})
	orch, err := k8sorch.New(ctx, k8sorch.Config{}, s.log)
	if err != nil {
		t.Fatalf("connect to K3s: %v", err)
	}
	s.orch = orch
	s.manifests = registry.New(pool, nil, s.log)
	ownerID := owner(t, s, pool, "ticket-9-k3s")
	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "recover", Image: "nginxinc/nginx-unprivileged:1.27-alpine",
		Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := orch.DeleteNamespace(cleanup, a.Namespace); err != nil {
			t.Errorf("delete integration namespace: %v", err)
		}
	})
	if err := orch.DeleteApp(ctx, a.Ref()); err != nil {
		t.Fatalf("delete active Deployment: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	deadline := time.NewTicker(250 * time.Millisecond)
	defer deadline.Stop()
	for {
		status, statusErr := orch.AppStatus(ctx, a.Ref())
		if statusErr == nil && a.ActiveReleaseID != nil &&
			convergenceMatches(status, *a.ActiveReleaseID, a.ConfigVersion) &&
			rolloutHealthy(status) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("deleted workload did not return: %v; last=%#v; statusErr=%v",
				ctx.Err(), status, statusErr)
		case <-deadline.C:
		}
	}
}
