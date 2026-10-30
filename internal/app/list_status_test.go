package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/orchestrator/k8s"
)

// liveSizedApps is how many services the live workspace had when its pages
// took seconds to load.
const liveSizedApps = 15

// slowCluster is a fake cluster that answers as a real one does from a
// dashboard: each call waits its turn on client-go's token bucket and then
// for the API server. It counts what it was asked.
type slowCluster struct {
	*fake.Clientset
	mu    sync.Mutex
	calls map[string]int
}

func newSlowCluster(qps float32, burst int, latency time.Duration) *slowCluster {
	c := &slowCluster{Clientset: fake.NewClientset(), calls: map[string]int{}}
	var limiter flowcontrol.RateLimiter
	if qps > 0 {
		limiter = flowcontrol.NewTokenBucketRateLimiter(qps, burst)
	}
	c.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		c.calls[a.GetVerb()+" "+a.GetResource().Resource]++
		c.mu.Unlock()
		if limiter != nil {
			limiter.Accept()
		}
		time.Sleep(latency)
		return false, nil, nil // and on to the tracker that holds the objects
	})
	return c
}

func (c *slowCluster) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = map[string]int{}
}

func (c *slowCluster) count() (total int, byKind map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	byKind = map[string]int{}
	for k, n := range c.calls {
		byKind[k] = n
		total += n
	}
	return total, byKind
}

// liveSizedWorkspace is a team with liveSizedApps running services, each
// with its Deployment in the cluster, listed through the Kubernetes
// orchestrator rather than the no-op one.
func liveSizedWorkspace(tb testing.TB, cluster *slowCluster) (*Service, string) {
	tb.Helper()
	ctx := context.Background()
	s, _, pool := testService(tb, Options{})
	id := owner(tb, s, pool, "svc-list-status")
	for i := range liveSizedApps {
		a, err := createAndDeploy(tb, s, ctx, id, CreateInput{
			Name: fmt.Sprintf("svc-%02d", i), Image: "nginx:alpine", Replicas: 1,
		})
		if err != nil {
			tb.Fatalf("create: %v", err)
		}
		// As ApplyApp writes one, labels and all, ready.
		ref := a.Ref()
		one := int32(1)
		if _, err := cluster.Clientset.AppsV1().Deployments(ref.Namespace).Create(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: ref.Name, Namespace: ref.Namespace, Labels: orchestrator.ObjectLabels(ref),
			},
			Spec:   appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
		}, metav1.CreateOptions{}); err != nil {
			tb.Fatalf("create deployment: %v", err)
		}
	}
	// Reading the team's projects adopts them into its default one, where
	// a project's canvas lists them.
	if _, err := s.Projects(ctx, id); err != nil {
		tb.Fatalf("projects: %v", err)
	}
	s.orch = k8s.NewWithClient(cluster, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cluster.reset()
	return s, id
}

// A list page asks the cluster about its services once per kind, not once
// per service. It was a GET per service: fifteen of them, one after another,
// each waiting on the client's rate limit, and the page waited seconds with
// the cluster idle (DEPLOY-13).
func TestListReadsTheClusterOncePerKind(t *testing.T) {
	ctx := context.Background()
	cluster := newSlowCluster(0, 0, 0)
	s, id := liveSizedWorkspace(t, cluster)

	list, err := s.List(ctx, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != liveSizedApps {
		t.Fatalf("listed %d apps, want %d", len(list), liveSizedApps)
	}
	for _, a := range list {
		if !a.StatusKnown || a.Status.Phase != "running" {
			t.Errorf("%s: status known %v, phase %q; want running, as the cluster has it",
				a.Name, a.StatusKnown, a.Status.Phase)
		}
	}
	// A list of Deployments and one of CronJobs; pods only for a rollout
	// short of ready, and none is.
	if total, byKind := cluster.count(); total > 2 {
		t.Errorf("List made %d cluster calls for %d apps, want at most 2: %v",
			total, liveSizedApps, byKind)
	}

	cluster.reset()
	inProject, err := s.ListInProject(ctx, id, list[0].ProjectID)
	if err != nil {
		t.Fatalf("ListInProject: %v", err)
	}
	if len(inProject) != liveSizedApps {
		t.Fatalf("listed %d apps in the project, want %d", len(inProject), liveSizedApps)
	}
	if total, byKind := cluster.count(); total > 2 {
		t.Errorf("ListInProject made %d cluster calls for %d apps, want at most 2: %v",
			total, liveSizedApps, byKind)
	}
}

// BenchmarkListPages times the list behind the workspace home and the one
// behind a project's canvas, against a cluster with ~3 ms of latency a call
// and client-go's rate limiting: the defaults the dashboard had (5 a second,
// bursts of 10) and the limits it has now.
//
// Each iteration is one page load. They run back to back, as tabs polling
// do, so the bucket's burst is spent after the first few and what is
// measured is the steady state somebody with the dashboard open sees.
func BenchmarkListPages(b *testing.B) {
	for _, limits := range []struct {
		name  string
		qps   float32
		burst int
	}{{"qps5-burst10", 5, 10}, {"qps50-burst100", 50, 100}} {
		b.Run(limits.name, func(b *testing.B) {
			ctx := context.Background()
			cluster := newSlowCluster(limits.qps, limits.burst, 3*time.Millisecond)
			s, id := liveSizedWorkspace(b, cluster)
			list, err := s.List(ctx, id)
			if err != nil {
				b.Fatal(err)
			}
			project := list[0].ProjectID
			cluster.reset()
			for b.Loop() {
				if _, err := s.List(ctx, id); err != nil {
					b.Fatal(err)
				}
				if _, err := s.ListInProject(ctx, id, project); err != nil {
					b.Fatal(err)
				}
			}
			total, _ := cluster.count()
			b.ReportMetric(float64(total)/float64(b.N), "calls/op")
		})
	}
}
