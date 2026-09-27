package k8s

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A small cluster: the node being retired, and one with room to take its work.

func retireNode(name, cpu, mem string) *corev1.Node {
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"kubernetes.io/hostname":      name,
			"topology.kubernetes.io/zone": "z1",
		}},
		Status: corev1.NodeStatus{
			Allocatable: alloc, Capacity: alloc,
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func retirePod(ns, name, node, cpu, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpu),
					corev1.ResourceMemory: resource.MustParse(mem),
				},
			}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// appPod is a pod of one of the engine's Deployments.
func appPod(ns, app, node string) *corev1.Pod {
	p := retirePod(ns, app+"-abc12", node, "100m", "128Mi")
	p.Labels = map[string]string{
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		orchestrator.LabelApp:       app,
		orchestrator.LabelOwner:     "owner-1",
	}
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: app + "-rs"}}
	return p
}

func appDeployment(ns, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1, Labels: map[string]string{
			orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
			orchestrator.LabelOwner:     "owner-1",
		}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: replicas, UpdatedReplicas: replicas,
			ReadyReplicas: replicas, AvailableReplicas: replicas,
		},
	}
}

func daemonSetPod(node string) *corev1.Pod {
	p := retirePod("kube-system", "svclb-"+node, node, "0", "0")
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "svclb"}}
	return p
}

func retireCluster(t *testing.T, objs ...runtime.Object) (*Orchestrator, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset(objs...)
	return NewWithClient(client, slog.New(slog.NewTextHandler(io.Discard, nil))), client
}

func evictions(client *fake.Clientset) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetSubresource() == "eviction" {
			n++
		}
	}
	return n
}

// The plan sorts a node's pods by what happens to them. The engine's apps are
// restarted, not evicted; what runs on every node is left alone; builds are
// left to finish; everything else is evicted as a drain would.
func TestARetirementRestartsTheEnginesAppsAndEvictsOnlyTheRest(t *testing.T) {
	ctx := context.Background()
	build := retirePod("yacht-builds", "build-1", "old", "100m", "64Mi")
	build.Labels = map[string]string{
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue, buildLabel: "true",
	}
	build.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "build-1"}}
	coredns := retirePod("kube-system", "coredns-1", "old", "100m", "64Mi")
	coredns.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "coredns"}}

	o, client := retireCluster(t,
		retireNode("old", "2", "4Gi"), retireNode("new", "4", "8Gi"),
		appDeployment("yacht-demo", "web", 1), appPod("yacht-demo", "web", "old"),
		daemonSetPod("old"), build, coredns,
	)

	plan, err := o.RetirePlan(ctx, "old")
	if err != nil {
		t.Fatalf("RetirePlan: %v", err)
	}
	if len(plan.Apps) != 1 || plan.Apps[0].Name != "web" || plan.Apps[0].PodsHere != 1 {
		t.Fatalf("apps = %+v, want web with one pod here", plan.Apps)
	}
	if !plan.Apps[0].Rolling {
		t.Error("a rolling Deployment is reported as one that recreates")
	}
	if len(plan.Evict) != 1 || plan.Evict[0].Name != "coredns-1" {
		t.Errorf("evict = %+v, want only the pod Yacht does not manage", plan.Evict)
	}
	if len(plan.Finishing) != 1 || plan.Finishing[0].Name != "build-1" {
		t.Errorf("finishing = %+v, want the build left to finish", plan.Finishing)
	}
	for _, e := range plan.Evict {
		if e.Name == "svclb-old" {
			t.Error("a DaemonSet pod is marked for eviction; it would be recreated here at once")
		}
	}
	if !plan.Room.Fits() {
		t.Errorf("room = %+v, want it to fit on a node twice the size", plan.Room)
	}

	// Moving the app is a rolling restart: nothing is evicted.
	if err := o.RestartApp(ctx, "yacht-demo", "web", "old@1"); err != nil {
		t.Fatalf("RestartApp: %v", err)
	}
	if n := evictions(client); n != 0 {
		t.Errorf("%d evictions made to move an app; a restart should need none", n)
	}
	dep, _ := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if got := dep.Spec.Template.Annotations[orchestrator.AnnotationRetiredFrom]; got != "old@1" {
		t.Errorf("restart marker = %q, want the retirement's token", got)
	}
}

// Restarting twice with the same token writes the same template, which is
// what makes a resumed retirement start no second rollout.
func TestRestartingWithTheSameTokenChangesNothing(t *testing.T) {
	ctx := context.Background()
	o, client := retireCluster(t, appDeployment("yacht-demo", "web", 1))

	if err := o.RestartApp(ctx, "yacht-demo", "web", "old@1"); err != nil {
		t.Fatalf("first restart: %v", err)
	}
	first, _ := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err := o.RestartApp(ctx, "yacht-demo", "web", "old@1"); err != nil {
		t.Fatalf("second restart: %v", err)
	}
	second, _ := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})

	if first.Spec.Template.String() != second.Spec.Template.String() {
		t.Error("the same token changed the pod template, which would start a second rollout")
	}
}

// A pod whose volume lives on this machine is not restarted and not evicted:
// either way, its replacement could only go back to this node.
func TestAPodOnALocalVolumeIsPinnedNotMoved(t *testing.T) {
	ctx := context.Background()
	db := appPod("yacht-demo", "db", "old")
	db.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "db-data"},
	}}}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "yacht-demo", Name: "db-data"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-123"},
		Status: corev1.PersistentVolumeClaimStatus{Capacity: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse("10Gi"),
		}},
	}
	// What local-path provisions: the directory's machine, by hostname.
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-123"},
		Spec: corev1.PersistentVolumeSpec{NodeAffinity: &corev1.VolumeNodeAffinity{
			Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"old"},
				}},
			}}},
		}},
	}

	o, _ := retireCluster(t,
		retireNode("old", "2", "4Gi"), retireNode("new", "4", "8Gi"),
		appDeployment("yacht-demo", "db", 1), db, pvc, pv,
	)
	plan, err := o.RetirePlan(ctx, "old")
	if err != nil {
		t.Fatalf("RetirePlan: %v", err)
	}
	if len(plan.Pinned) != 1 {
		t.Fatalf("pinned = %+v, want the pod on the local volume", plan.Pinned)
	}
	pin := plan.Pinned[0]
	if pin.App != "db" || pin.Claim != "db-data" || pin.SizeBytes != 10<<30 {
		t.Errorf("pinned = %+v, want db's 10 GiB claim named", pin)
	}
	if len(plan.Apps) != 0 || len(plan.Evict) != 0 {
		t.Errorf("apps = %+v, evict = %+v; a pinned pod must be neither restarted nor evicted",
			plan.Apps, plan.Evict)
	}
	if !plan.Empty() {
		t.Error("a node holding only a pinned pod is not reported as retired down to what stays")
	}
}

// A volume another node could also serve is not pinned: the pod can follow it.
func TestAVolumeAnotherNodeCanServeDoesNotPin(t *testing.T) {
	pv := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{
		NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"z1"},
			}}}},
		}},
	}}
	nodes := []corev1.Node{*retireNode("old", "1", "1Gi"), *retireNode("new", "1", "1Gi")}
	if onlyNode(pv, "old", nodes) {
		t.Error("a zonal volume both nodes can reach was counted as pinning its pod")
	}
}

// Refused before it starts when the rest of the cluster is too small, with
// the shortfall rather than a bare no.
func TestTheRoomCheckReportsTheShortfall(t *testing.T) {
	ctx := context.Background()
	big := appPod("yacht-demo", "web", "old")
	big.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1500m"), corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	o, _ := retireCluster(t,
		retireNode("old", "2", "4Gi"), retireNode("new", "2", "4Gi"),
		retirePod("default", "busy", "new", "1", "512Mi"),
		appDeployment("yacht-demo", "web", 1), big,
	)
	plan, err := o.RetirePlan(ctx, "old")
	if err != nil {
		t.Fatalf("RetirePlan: %v", err)
	}
	if plan.Room.Fits() {
		t.Fatalf("room = %+v; 1.5 CPU does not fit in the 1 CPU left", plan.Room)
	}
	if got := plan.Room.ShortCPUMillis(); got != 500 {
		t.Errorf("short by %dm CPU, want 500m", got)
	}
	if plan.Room.ShortMemBytes() != 0 {
		t.Error("memory reported short when there is plenty")
	}
}

// Totals are not enough: two nodes with 1 GiB free each do not hold a pod
// that asks for 1.5.
func TestTheRoomCheckPlacesPodsNotTotals(t *testing.T) {
	nodes := []corev1.Node{
		*retireNode("old", "4", "8Gi"), *retireNode("a", "4", "1Gi"), *retireNode("b", "4", "1Gi"),
	}
	pod := retirePod("x", "big", "old", "100m", "1536Mi")
	room := retireRoom(nodes, []corev1.Pod{*pod}, "old", []*corev1.Pod{pod})

	if room.Fits() {
		t.Fatalf("room = %+v; the pod fits in neither node", room)
	}
	if room.Unplaced != "x/big" {
		t.Errorf("unplaced = %q, want the pod named", room.Unplaced)
	}
}

// A cordoned or unready node is not room. Counting it would pass a check the
// scheduler then fails.
func TestRoomOnANodeThatTakesNoWorkIsNotCounted(t *testing.T) {
	closed := retireNode("closed", "8", "16Gi")
	closed.Spec.Unschedulable = true
	pod := retirePod("x", "p", "old", "100m", "64Mi")
	room := retireRoom([]corev1.Node{*retireNode("old", "1", "1Gi"), *closed},
		[]corev1.Pod{*pod}, "old", []*corev1.Pod{pod})
	if room.Fits() || room.FreeCPUMillis != 0 {
		t.Errorf("room = %+v, want nothing counted on a cordoned node", room)
	}
}

// The retirement mark round-trips through the node, which is what lets it
// survive the process that set it.
func TestTheRetirementMarkLivesOnTheNode(t *testing.T) {
	ctx := context.Background()
	o, _ := retireCluster(t, retireNode("old", "1", "1Gi"))
	since := time.Unix(1_790_000_000, 0)

	if err := o.MarkRetiring(ctx, "old", since); err != nil {
		t.Fatalf("mark: %v", err)
	}
	nodes, _ := o.Nodes(ctx)
	if !nodes[0].RetiringSince.Equal(since) {
		t.Errorf("RetiringSince = %v, want %v", nodes[0].RetiringSince, since)
	}
	if err := o.MarkRetiring(ctx, "old", time.Time{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	nodes, _ = o.Nodes(ctx)
	if !nodes[0].RetiringSince.IsZero() {
		t.Error("clearing the mark left the node retiring")
	}
}

// Progress is read back from the cluster: an app this retirement restarted
// and that has left counts as moved even though it has no pods here any more.
func TestAMovedAppStillCountsTowardProgress(t *testing.T) {
	ctx := context.Background()
	node := retireNode("old", "2", "4Gi")
	since := time.Unix(1_790_000_000, 0)
	node.Annotations = map[string]string{orchestrator.AnnotationRetiringSince: "1790000000"}
	node.Spec.Unschedulable = true
	moved := appDeployment("yacht-demo", "web", 1)
	moved.Spec.Template.Annotations = map[string]string{
		orchestrator.AnnotationRetiredFrom: orchestrator.RetireToken("old", since),
	}

	o, _ := retireCluster(t, node, retireNode("new", "4", "8Gi"),
		moved, appDeployment("yacht-demo", "api", 1), appPod("yacht-demo", "api", "old"))
	plan, err := o.RetirePlan(ctx, "old")
	if err != nil {
		t.Fatalf("RetirePlan: %v", err)
	}
	if len(plan.Apps) != 2 || plan.Moved() != 1 {
		t.Fatalf("apps = %+v, want 1 of 2 moved", plan.Apps)
	}
	if next, ok := plan.Next(); !ok || next.Name != "api" {
		t.Errorf("next = %+v, want api", next)
	}
}

// A single-replica app must never be without a serving pod during a restart:
// one surge pod, none unavailable.
func TestASingleReplicaAppRollsWithoutAGap(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	spec.Replicas = 1

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	dep, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	ru := dep.Spec.Strategy.RollingUpdate
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil {
		t.Fatalf("strategy = %+v, want a rolling update", dep.Spec.Strategy)
	}
	if ru.MaxSurge == nil || ru.MaxSurge.IntValue() < 1 {
		t.Errorf("maxSurge = %v, want at least one pod started before the old one stops", ru.MaxSurge)
	}
	if ru.MaxUnavailable == nil || ru.MaxUnavailable.IntValue() != 0 {
		t.Errorf("maxUnavailable = %v, want 0 — the only replica must keep serving", ru.MaxUnavailable)
	}
}
