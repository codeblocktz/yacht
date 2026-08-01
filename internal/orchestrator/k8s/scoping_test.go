package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

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

func specWithVolume() orchestrator.AppSpec {
	s := testSpec()
	s.Replicas = 1
	s.Volumes = []orchestrator.VolumeSpec{
		{Name: "data", MountPath: "/var/lib/data", SizeBytes: 2 << 30},
	}
	return s
}

func TestApplyAppCreatesAndMountsTheVolume(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := specWithVolume()

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	pvc, err := client.CoreV1().PersistentVolumeClaims(spec.Namespace).
		Get(ctx, spec.Name+"-data", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if got := pvc.Labels[orchestrator.LabelOwner]; got != string(spec.Ref.Owner) {
		t.Errorf("pvc owner label = %q, want %q — the team scoping reads this",
			got, spec.Ref.Owner)
	}

	dep, err := client.AppsV1().Deployments(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	var mounted bool
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/var/lib/data" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatal("the container has no mount for the volume that was created")
	}
}

// A rolling update starts the new pod before the old one stops. With a
// ReadWriteOnce claim the new pod cannot mount what the old one holds, so it
// waits forever and the deploy never finishes.
func TestVolumesForceRecreateStrategy(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if err := o.ApplyApp(ctx, specWithVolume()); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	dep, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %q, want Recreate — a rolling update deadlocks on a "+
			"ReadWriteOnce claim", dep.Spec.Strategy.Type)
	}
}

// And without storage it must stay rolling: recreating every deploy would be
// downtime nobody asked for.
func TestNoVolumesLeavesTheStrategyRolling(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if err := o.ApplyApp(ctx, testSpec()); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	dep, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		t.Fatal("a workload with no storage was made to recreate on every deploy")
	}
}

// Detaching the last volume must return the workload to rolling updates.
func TestRemovingTheLastVolumeRestoresRollingUpdates(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if err := o.ApplyApp(ctx, specWithVolume()); err != nil {
		t.Fatalf("ApplyApp with volume: %v", err)
	}
	if err := o.ApplyApp(ctx, testSpec()); err != nil {
		t.Fatalf("ApplyApp without: %v", err)
	}

	dep, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		t.Fatal("the workload still recreates after its last volume was detached")
	}
}

// The claim outlives the apply that stopped mentioning it. Deleting storage is
// a separate act with its own confirmation, never a side effect of an edit.
func TestDetachingAVolumeDoesNotDeleteTheClaim(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if err := o.ApplyApp(ctx, specWithVolume()); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	if err := o.ApplyApp(ctx, testSpec()); err != nil {
		t.Fatalf("ApplyApp without: %v", err)
	}

	if _, err := client.CoreV1().PersistentVolumeClaims("yacht-demo").
		Get(ctx, "web-data", metav1.GetOptions{}); err != nil {
		t.Fatalf("the claim was destroyed by an edit that merely detached it: %v", err)
	}
}

// The live sequence: an app is created, and storage is attached afterwards.
// Applying a volume to a Deployment that already exists is the ordinary case,
// and it is not the same code path as creating one with a volume already on it.
func TestAttachingAVolumeToAnExistingWorkload(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if err := o.ApplyApp(ctx, testSpec()); err != nil {
		t.Fatalf("ApplyApp without volume: %v", err)
	}
	if err := o.ApplyApp(ctx, specWithVolume()); err != nil {
		t.Fatalf("ApplyApp with volume: %v", err)
	}

	dep, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	var mounted bool
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/var/lib/data" {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("mounts = %+v, want one at /var/lib/data", dep.Spec.Template.Spec.Containers[0].VolumeMounts)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("strategy = %q, want Recreate", dep.Spec.Strategy.Type)
	}
}
