package k8s

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// This test is intentionally opt-in: it creates real workloads and relies on
// a Deployment controller, which the fake clientset cannot simulate. CI or a
// developer can point it at a disposable K3s cluster with YACHT_TEST_K3S=1.
func TestK3sDistinguishesASlowRolloutFromATerminalOne(t *testing.T) {
	if os.Getenv("YACHT_TEST_K3S") == "" {
		t.Skip("set YACHT_TEST_K3S=1 to run against the current disposable K3s cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, err := New(ctx, Config{}, log)
	if err != nil {
		t.Fatalf("connect to K3s: %v", err)
	}
	namespace := fmt.Sprintf("yacht-t7-%d", time.Now().UnixNano())
	ref := orchestrator.Ref{Owner: "ticket-7", Namespace: namespace, Name: "web"}
	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{
		Owner: "ticket-7", Name: namespace,
	}); err != nil {
		t.Fatalf("ensure namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := o.DeleteNamespace(cleanup, namespace); err != nil {
			t.Errorf("delete integration namespace: %v", err)
		}
	})

	spec := orchestrator.AppSpec{
		Ref: ref, Image: "nginxinc/nginx-unprivileged:1.27-alpine",
		Replicas: 1, Port: 8080, RunAsUser: 101,
	}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("apply healthy workload: %v", err)
	}
	seenSlow := false
	waitForStatus(t, ctx, o, ref, func(status orchestrator.AppStatus) bool {
		if !status.Terminal && (status.ObservedGeneration < status.Generation ||
			status.Available < status.Desired) {
			seenSlow = true
		}
		return !status.Terminal && status.ObservedGeneration >= status.Generation &&
			status.AvailableCondition && status.Available == 1
	})
	if !seenSlow {
		t.Log("healthy image was cached and became available before the first observation")
	}

	spec.Image = "yacht.invalid/ticket-7/never-exists:missing"
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("apply terminal workload: %v", err)
	}
	dep, err := o.client.AppsV1().Deployments(namespace).
		Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get terminal deployment: %v", err)
	}
	deadline := int32(2)
	dep.Spec.ProgressDeadlineSeconds = &deadline
	if _, err := o.client.AppsV1().Deployments(namespace).
		Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("shorten progress deadline: %v", err)
	}
	terminal := waitForStatus(t, ctx, o, ref, func(status orchestrator.AppStatus) bool {
		return status.Terminal
	})
	if terminal.Reason != "ProgressDeadlineExceeded" || terminal.Message == "" {
		t.Fatalf("terminal rollout = %#v", terminal)
	}
}

func waitForStatus(
	t *testing.T, ctx context.Context, o *Orchestrator, ref orchestrator.Ref,
	done func(orchestrator.AppStatus) bool,
) orchestrator.AppStatus {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last orchestrator.AppStatus
	for {
		status, err := o.AppStatus(ctx, ref)
		if err == nil {
			last = status
			if done(status) {
				return status
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for rollout status: %v; last=%#v", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}
