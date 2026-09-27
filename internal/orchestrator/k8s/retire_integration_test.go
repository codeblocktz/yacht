package k8s

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/retire"
)

// Opt-in like the rollout test: it needs a real scheduler and Deployment
// controller, and at least two worker nodes to move between. It retires the
// node running a single-replica app and watches the app's ready endpoints the
// whole time — the claim under test is that the count never reaches zero —
// while a second app, whose local-path volume lives on that node, is left
// exactly where it is.
//
//	k3d cluster create retire --agents 2
//	YACHT_TEST_K3S=1 go test ./internal/orchestrator/k8s -run K3sRetiring -v
//
// It cordons and evicts on the cluster it is pointed at. Use a throwaway one.
func TestK3sRetiringMovesAnAppWithoutAGap(t *testing.T) {
	if os.Getenv("YACHT_TEST_K3S") == "" {
		t.Skip("set YACHT_TEST_K3S=1 to run against a disposable K3s cluster with two agents")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, err := New(ctx, Config{}, log)
	if err != nil {
		t.Fatalf("connect to K3s: %v", err)
	}
	r, ok := retire.For(o, log)
	if !ok {
		t.Fatal("the Kubernetes orchestrator cannot retire nodes")
	}

	ns := fmt.Sprintf("yacht-retire-%d", time.Now().UnixNano())
	web := orchestrator.Ref{Owner: "retire-test", Namespace: ns, Name: "web"}
	db := orchestrator.Ref{Owner: "retire-test", Namespace: ns, Name: "db"}
	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: "retire-test", Name: ns}); err != nil {
		t.Fatalf("ensure namespace: %v", err)
	}
	spec := func(ref orchestrator.Ref) orchestrator.AppSpec {
		return orchestrator.AppSpec{
			Ref: ref, Image: "nginxinc/nginx-unprivileged:1.27-alpine",
			Replicas: 1, Port: 8080, RunAsUser: 101, HealthPath: "/",
			CPURequest: "50m", MemoryRequest: "32Mi",
		}
	}
	if err := o.ApplyApp(ctx, spec(web)); err != nil {
		t.Fatalf("apply web: %v", err)
	}
	waitAvailable(t, ctx, o, web)
	node := podNode(t, ctx, o, ns, "web")

	// The database is placed on the same node by closing every other one
	// while it starts: local-path provisions where the pod first runs.
	others := otherNodes(t, ctx, o, node)
	for _, n := range others {
		_ = o.Cordon(ctx, n, true)
	}
	dbSpec := spec(db)
	dbSpec.Volumes = []orchestrator.VolumeSpec{{Name: "data", MountPath: "/data", SizeBytes: 64 << 20}}
	if err := o.ApplyApp(ctx, dbSpec); err != nil {
		t.Fatalf("apply db: %v", err)
	}
	waitAvailable(t, ctx, o, db)
	for _, n := range others {
		_ = o.Cordon(ctx, n, false)
	}

	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_ = r.Stop(cleanup, node)
		_ = o.DeleteNamespace(cleanup, ns)
	})

	// Watch the app's ready endpoints for the whole retirement.
	var lowest atomic.Int32
	lowest.Store(1 << 20)
	watching, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go func() {
		for watching.Err() == nil {
			if n, err := readyEndpoints(watching, o, ns, "web"); err == nil && n < lowest.Load() {
				lowest.Store(n)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	plan, err := r.Begin(ctx, node)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for !(plan.Retiring() && plan.Empty()) {
		select {
		case <-ctx.Done():
			t.Fatalf("retirement did not finish: %+v", plan)
		case <-time.After(time.Second):
		}
		if plan, err = r.Step(ctx, node); err != nil {
			t.Logf("step: %v", err)
		}
	}
	stopWatching()

	switch got := lowest.Load(); {
	case got == 1<<20:
		t.Fatal("the app's endpoints were never read, so nothing was proved about a gap")
	case got < 1:
		t.Errorf("web had %d ready endpoints at some point during the retirement, want never fewer than 1", got)
	}
	if moved := podNode(t, ctx, o, ns, "web"); moved == node {
		t.Errorf("web is still on %s after the retirement finished", node)
	}
	if len(plan.Pinned) != 1 || plan.Pinned[0].App != "db" {
		t.Fatalf("pinned = %+v, want db held by its volume", plan.Pinned)
	}
	if still := podNode(t, ctx, o, ns, "db"); still != node {
		t.Errorf("db moved to %s; its volume is on %s, so it should have been left alone", still, node)
	}
	t.Logf("retired %s: web moved without a gap, db (%d bytes) left pinned", node, plan.Pinned[0].SizeBytes)
}

func waitAvailable(t *testing.T, ctx context.Context, o *Orchestrator, ref orchestrator.Ref) {
	t.Helper()
	waitForStatus(t, ctx, o, ref, func(s orchestrator.AppStatus) bool {
		return s.ObservedGeneration >= s.Generation && s.Available == s.Desired && s.Desired > 0
	})
}

// podNode is where an app's one running pod is.
func podNode(t *testing.T, ctx context.Context, o *Orchestrator, ns, app string) string {
	t.Helper()
	pods, err := o.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: orchestrator.LabelApp + "=" + app,
	})
	if err != nil {
		t.Fatalf("list %s pods: %v", app, err)
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			return p.Spec.NodeName
		}
	}
	t.Fatalf("%s has no running pod", app)
	return ""
}

func otherNodes(t *testing.T, ctx context.Context, o *Orchestrator, node string) []string {
	t.Helper()
	nodes, err := o.Nodes(ctx)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	var out []string
	for _, n := range nodes {
		if n.Name != node && n.Ready && !n.Unschedulable {
			out = append(out, n.Name)
		}
	}
	if len(out) == 0 {
		t.Skip("needs a second schedulable node to move work onto")
	}
	return out
}

// readyEndpoints is how many pods the app's Service would send traffic to.
func readyEndpoints(ctx context.Context, o *Orchestrator, ns, svc string) (int32, error) {
	slices, err := o.client.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=" + svc,
	})
	if err != nil {
		return 0, err
	}
	var n int32
	for _, s := range slices.Items {
		for _, e := range s.Endpoints {
			if e.Conditions.Ready != nil && *e.Conditions.Ready {
				n++
			}
		}
	}
	return n, nil
}
