package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The cluster views were written for a single-owner engine, where everything in
// the cluster belonged to the person looking. Teams made that false: a member of
// one team must not be shown another team's workload names, namespaces or
// volumes, and a cluster page is the easiest place to forget that.

func seedForeign(t *testing.T, o *Orchestrator) {
	t.Helper()
	ctx := context.Background()

	mine := orchestrator.AppSpec{
		Ref:      orchestrator.Ref{Owner: "team-a", Namespace: "yacht-a", Name: "mine"},
		Image:    "nginx:alpine",
		Replicas: 1,
		Port:     8080,
	}
	theirs := orchestrator.AppSpec{
		Ref:      orchestrator.Ref{Owner: "team-b", Namespace: "yacht-b", Name: "theirs"},
		Image:    "nginx:alpine",
		Replicas: 1,
		Port:     8080,
	}
	for _, spec := range []orchestrator.AppSpec{mine, theirs} {
		if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{
			Owner: spec.Ref.Owner, Name: spec.Ref.Namespace,
		}); err != nil {
			t.Fatalf("EnsureNamespace %s: %v", spec.Ref.Namespace, err)
		}
		if err := o.ApplyApp(ctx, spec); err != nil {
			t.Fatalf("ApplyApp %s: %v", spec.Ref.Name, err)
		}
	}
}

func TestPodsAreScopedToTheirOwner(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	seedForeign(t, o)

	// Pods are created by the Deployment in a real cluster; the fake makes none,
	// so stand them up with the labels the pod template carries.
	for _, p := range []struct{ ns, name, owner, app string }{
		{"yacht-a", "mine-abc", "team-a", "mine"},
		{"yacht-b", "theirs-xyz", "team-b", "theirs"},
	} {
		if _, err := client.CoreV1().Pods(p.ns).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: p.name, Namespace: p.ns,
				Labels: map[string]string{
					orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
					orchestrator.LabelApp:       p.app,
					orchestrator.LabelOwner:     p.owner,
				},
			},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod %s: %v", p.name, err)
		}
	}

	pods, err := o.Pods(ctx, orchestrator.PodListOptions{Owner: "team-a"})
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	for _, p := range pods {
		if p.Namespace == "yacht-b" || p.Name == "theirs-xyz" {
			t.Fatalf("team-a was shown team-b's pod %s/%s", p.Namespace, p.Name)
		}
	}
	if len(pods) != 1 {
		t.Fatalf("pods = %d, want only team-a's one", len(pods))
	}
}

func TestVolumesAreScopedToTheirOwner(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	for _, v := range []struct{ ns, name, owner string }{
		{"yacht-a", "data-a", "team-a"},
		{"yacht-b", "data-b", "team-b"},
	} {
		if _, err := client.CoreV1().PersistentVolumeClaims(v.ns).Create(ctx,
			&corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: v.name, Namespace: v.ns,
					Labels: map[string]string{
						orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
						orchestrator.LabelOwner:     v.owner,
					},
				},
			}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pvc %s: %v", v.name, err)
		}
	}

	vols, err := o.Volumes(ctx, "team-a")
	if err != nil {
		t.Fatalf("Volumes: %v", err)
	}
	for _, v := range vols {
		if v.Name == "data-b" {
			t.Fatal("team-a was shown team-b's volume")
		}
	}
	if len(vols) != 1 {
		t.Fatalf("volumes = %d, want only team-a's one", len(vols))
	}
}

// A volume the engine did not create carries no owner label. It belongs to
// whoever runs the cluster, not to any team, and must not appear for either.
func TestUnmanagedVolumesAreShownToNobody(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if _, err := client.CoreV1().PersistentVolumeClaims("default").Create(ctx,
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: "default"},
		}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pvc: %v", err)
	}

	vols, err := o.Volumes(ctx, "team-a")
	if err != nil {
		t.Fatalf("Volumes: %v", err)
	}
	if len(vols) != 0 {
		t.Fatalf("volumes = %v, want none — an unmanaged claim is not a team's", vols)
	}
}
