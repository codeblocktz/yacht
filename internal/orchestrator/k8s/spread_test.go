package k8s

import (
	"context"
	"io"
	"log/slog"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// sitedNode is a machine, at a site when site is not empty.
func sitedNode(name, site string) *corev1.Node {
	labels := map[string]string{corev1.LabelHostname: name}
	if site != "" {
		labels[corev1.LabelTopologyZone] = site
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func spreadOrchestrator(t *testing.T, nodes ...runtime.Object) (*Orchestrator, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset(nodes...)
	return NewWithClient(client, slog.New(slog.NewTextHandler(io.Discard, nil))), client
}

func appliedDeployment(t *testing.T, o *Orchestrator, client *fake.Clientset, spec orchestrator.AppSpec) *appsv1.Deployment {
	t.Helper()
	ctx := context.Background()
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	dep, err := client.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return dep
}

func constraintOn(dep *appsv1.Deployment, key string) *corev1.TopologySpreadConstraint {
	for i, c := range dep.Spec.Template.Spec.TopologySpreadConstraints {
		if c.TopologyKey == key {
			return &dep.Spec.Template.Spec.TopologySpreadConstraints[i]
		}
	}
	return nil
}

// A preference and never a requirement: an install on one machine, or a site
// that is full, has to run every replica even when it cannot keep them apart.
// And it counts this app's pods, not everything in the namespace.
func TestReplicasPreferDifferentMachines(t *testing.T) {
	o, client := spreadOrchestrator(t, sitedNode("only", ""))
	spec := testSpec()

	dep := appliedDeployment(t, o, client, spec)

	c := constraintOn(dep, corev1.LabelHostname)
	if c == nil {
		t.Fatalf("no spread by machine: %+v", dep.Spec.Template.Spec.TopologySpreadConstraints)
	}
	if c.WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("whenUnsatisfiable = %s — a one-machine install could not schedule a second replica",
			c.WhenUnsatisfiable)
	}
	if c.MaxSkew != 1 {
		t.Errorf("maxSkew = %d, want 1", c.MaxSkew)
	}
	if c.LabelSelector == nil ||
		c.LabelSelector.MatchLabels[orchestrator.LabelApp] != spec.Name {
		t.Errorf("selector = %+v, want this app's own pods", c.LabelSelector)
	}
	for k, v := range c.LabelSelector.MatchLabels {
		if dep.Spec.Selector.MatchLabels[k] != v {
			t.Errorf("selector label %s=%s is not the Deployment's own", k, v)
		}
	}
	if len(c.MatchLabelKeys) != 1 || c.MatchLabelKeys[0] != appsv1.DefaultDeploymentUniqueLabelKey {
		t.Errorf("matchLabelKeys = %v — a rollout would count the pods it is replacing", c.MatchLabelKeys)
	}
}

// With no sites anywhere, a site constraint would leave every node out of
// spreading — the scheduler skips nodes missing any key a pod names — and
// switch off the machine spreading along with it.
func TestAnInstallInOnePlaceIsNotSpreadBySite(t *testing.T) {
	o, client := spreadOrchestrator(t, sitedNode("a", ""), sitedNode("b", ""))

	dep := appliedDeployment(t, o, client, testSpec())

	if c := constraintOn(dep, corev1.LabelTopologyZone); c != nil {
		t.Fatalf("spread by site with no sites: %+v", c)
	}
}

// Once machines are in more than one place, losing one place should take some
// of an app's replicas rather than all of them.
func TestReplicasPreferDifferentSites(t *testing.T) {
	o, client := spreadOrchestrator(t, sitedNode("a", "dar-a"), sitedNode("b", "dar-b"))

	dep := appliedDeployment(t, o, client, testSpec())

	c := constraintOn(dep, corev1.LabelTopologyZone)
	if c == nil {
		t.Fatalf("no spread by site: %+v", dep.Spec.Template.Spec.TopologySpreadConstraints)
	}
	if c.WhenUnsatisfiable != corev1.ScheduleAnyway || c.MaxSkew != 1 {
		t.Errorf("site constraint = %+v, want a soft maxSkew 1", c)
	}
	if constraintOn(dep, corev1.LabelHostname) == nil {
		t.Error("spreading by site dropped spreading by machine")
	}
}

// An app with a volume runs one pod, which goes where its volume can be
// mounted. There is nothing to spread, and a constraint added to its template
// would only restart it — with the gap Recreate means — on its next apply.
func TestAnAppWithAVolumeIsNotSpread(t *testing.T) {
	o, client := spreadOrchestrator(t, sitedNode("a", "dar-a"), sitedNode("b", "dar-b"))
	spec := testSpec()
	spec.Replicas = 1
	spec.Volumes = []orchestrator.VolumeSpec{{Name: "data", MountPath: "/data", SizeBytes: 1 << 30}}

	dep := appliedDeployment(t, o, client, spec)

	if got := dep.Spec.Template.Spec.TopologySpreadConstraints; len(got) != 0 {
		t.Fatalf("an app with a volume was spread: %+v", got)
	}
}

// Retiring a node restarts an app by writing into its pod template. Spreading
// lives in the same template and must survive it, and a redeploy after the
// restart must keep both.
func TestSpreadingSurvivesARetirementRestart(t *testing.T) {
	ctx := context.Background()
	o, client := spreadOrchestrator(t, sitedNode("a", "dar-a"), sitedNode("b", "dar-b"))
	spec := testSpec()
	appliedDeployment(t, o, client, spec)

	if err := o.RestartApp(ctx, spec.Namespace, spec.Name, "a@2026-09-27T00:00:00Z"); err != nil {
		t.Fatalf("RestartApp: %v", err)
	}
	dep := appliedDeployment(t, o, client, spec)

	if dep.Spec.Template.Annotations[orchestrator.AnnotationRetiredFrom] == "" {
		t.Error("a redeploy dropped the retirement's restart")
	}
	if constraintOn(dep, corev1.LabelHostname) == nil || constraintOn(dep, corev1.LabelTopologyZone) == nil {
		t.Errorf("spreading lost across a restart: %+v", dep.Spec.Template.Spec.TopologySpreadConstraints)
	}
}

// The node list says where each machine is, and says nothing for one that was
// never given a site.
func TestNodesSayWhereTheyAre(t *testing.T) {
	o, _ := spreadOrchestrator(t, sitedNode("a", "dar-a"), sitedNode("b", ""))

	nodes, err := o.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	got := map[string]string{}
	for _, n := range nodes {
		got[n.Name] = n.Site
	}
	if got["a"] != "dar-a" || got["b"] != "" {
		t.Fatalf("sites = %v, want a at dar-a and b at none", got)
	}
}
