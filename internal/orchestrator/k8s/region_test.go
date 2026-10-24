package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// regionNode is a machine in a region, at a site.
func regionNode(name, region, site string) *corev1.Node {
	n := sitedNode(name, site)
	if region != "" {
		n.Labels[corev1.LabelTopologyRegion] = region
	}
	return n
}

// requiredRegions is the regions a Deployment's pods are required to run in,
// nil when nothing requires one.
func requiredRegions(t *testing.T, dep *appsv1.Deployment) []string {
	t.Helper()
	aff := dep.Spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil ||
		aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
		t.Fatalf("node affinity = %+v, want one term with one expression", terms)
	}
	e := terms[0].MatchExpressions[0]
	if e.Key != orchestrator.RegionLabel || e.Operator != corev1.NodeSelectorOpIn {
		t.Fatalf("node affinity expression = %+v, want %s In", e, orchestrator.RegionLabel)
	}
	return e.Values
}

// The label is Kubernetes' own region label, which kubelets may set on
// themselves and the scheduler and storage drivers already read.
func TestTheRegionLabelIsKubernetesOwn(t *testing.T) {
	if orchestrator.RegionLabel != corev1.LabelTopologyRegion {
		t.Fatalf("RegionLabel = %q, want %q", orchestrator.RegionLabel, corev1.LabelTopologyRegion)
	}
}

// A region is a requirement: the pods may run on the region's machines and
// nowhere else, and the pool's selector and toleration hold beside it.
func TestAnAppIsRequiredToRunInItsRegion(t *testing.T) {
	o, client := spreadOrchestrator(t,
		regionNode("fsn-1", "fsn", "fsn-a"), regionNode("dar-1", "dar", "dar-a"))
	spec := testSpec()
	spec.Region = "fsn"
	spec.Pool = "pro"

	dep := appliedDeployment(t, o, client, spec)
	if got := requiredRegions(t, dep); len(got) != 1 || got[0] != "fsn" {
		t.Fatalf("required regions = %v, want [fsn]", got)
	}
	pod := dep.Spec.Template.Spec
	if pod.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
		t.Errorf("a region was written as a preference too: %+v", pod.Affinity.NodeAffinity)
	}
	if pod.NodeSelector[orchestrator.PoolLabel] != "pro" || len(pod.Tolerations) != 1 {
		t.Fatalf("the pool was lost beside the region: selector %v, tolerations %+v",
			pod.NodeSelector, pod.Tolerations)
	}
	// Spreading carries on, across the region's own sites: the scheduler
	// counts only the nodes the affinity admits.
	if constraintOn(dep, corev1.LabelTopologyZone) == nil || constraintOn(dep, corev1.LabelHostname) == nil {
		t.Errorf("spreading was lost in a region: %+v", pod.TopologySpreadConstraints)
	}
}

// Every way an app runs is the same Deployment: one with a volume, as every
// database is, and one asleep at zero replicas behind the waker, which wakes
// in the region it slept in.
func TestEveryWayAnAppRunsKeepsItsRegion(t *testing.T) {
	for name, mutate := range map[string]func(*orchestrator.AppSpec){
		"with a volume": func(s *orchestrator.AppSpec) {
			s.Replicas = 1
			s.Volumes = []orchestrator.VolumeSpec{{
				Name: "data", MountPath: "/var/lib/postgresql/data", SizeBytes: 1 << 30, Class: "replicated-dar",
			}}
		},
		"asleep": func(s *orchestrator.AppSpec) {
			s.Hosts = []string{"web.apps.example.test"}
			s.Replicas = 0
			s.Waker = &orchestrator.WakerEndpoint{IP: "10.0.0.5", Port: 8090}
		},
		"internal": func(s *orchestrator.AppSpec) { s.Internal = true },
	} {
		t.Run(name, func(t *testing.T) {
			o, client := spreadOrchestrator(t, regionNode("dar-1", "dar", ""))
			spec := testSpec()
			spec.Region = "dar"
			mutate(&spec)
			dep := appliedDeployment(t, o, client, spec)
			if got := requiredRegions(t, dep); len(got) != 1 || got[0] != "dar" {
				t.Fatalf("required regions = %v, want [dar]", got)
			}
		})
	}
}

// A volume in a region is claimed from the class the region's volumes use,
// which is the one keeping replicated storage's copies there too.
func TestAVolumeIsClaimedFromItsRegionsClass(t *testing.T) {
	o, client := spreadOrchestrator(t, regionNode("dar-1", "dar", ""))
	spec := testSpec()
	spec.Region, spec.Replicas = "dar", 1
	spec.Volumes = []orchestrator.VolumeSpec{{
		Name: "data", MountPath: "/data", SizeBytes: 1 << 30, Class: "replicated-dar",
	}}
	appliedDeployment(t, o, client, spec)
	pvc, err := client.CoreV1().PersistentVolumeClaims(spec.Namespace).
		Get(context.Background(), claimName(spec.Name, "data"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "replicated-dar" {
		t.Fatalf("claim class = %v, want replicated-dar", pvc.Spec.StorageClassName)
	}
}

// An install with no regions writes no affinity, and its pods' template —
// and the revision fingerprint that decides whether they restart — is what
// it was before regions existed.
func TestNoRegionWritesNoAffinity(t *testing.T) {
	o, client := spreadOrchestrator(t, sitedNode("only", ""))
	spec := testSpec()
	dep := appliedDeployment(t, o, client, spec)
	if dep.Spec.Template.Spec.Affinity != nil {
		t.Fatalf("an app with no region has affinity %+v", dep.Spec.Template.Spec.Affinity)
	}

	before := specHash(spec)
	spec.Region = "fsn"
	if specHash(spec) == before {
		t.Fatal("a region does not change the fingerprint, so placing an app would not roll it")
	}
	spec.Region = ""
	if specHash(spec) != before {
		t.Fatal("an empty region changed the fingerprint")
	}
}

// Moving an app replaces the requirement, and taking the region away takes
// the affinity off: it is this field manager's, so an apply that omits it
// removes it.
func TestARegionChangeReplacesTheRequirement(t *testing.T) {
	o, client := spreadOrchestrator(t, regionNode("fsn-1", "fsn", ""), regionNode("dar-1", "dar", ""))
	spec := testSpec()
	spec.Region = "fsn"
	appliedDeployment(t, o, client, spec)

	spec.Region = "dar"
	if got := requiredRegions(t, appliedDeployment(t, o, client, spec)); len(got) != 1 || got[0] != "dar" {
		t.Fatalf("after a move the pods are required in %v, want [dar]", got)
	}
	spec.Region = ""
	if dep := appliedDeployment(t, o, client, spec); dep.Spec.Template.Spec.Affinity != nil &&
		dep.Spec.Template.Spec.Affinity.NodeAffinity != nil {
		t.Fatalf("the region was removed and the affinity stayed: %+v", dep.Spec.Template.Spec.Affinity)
	}
}

// A region that could end a shell command, or is not a label value, never
// reaches the cluster.
func TestAMalformedRegionIsRefused(t *testing.T) {
	o, _ := spreadOrchestrator(t)
	for _, bad := range []string{"FSN", "fsn;id", "-fsn", "fsn dar"} {
		spec := testSpec()
		spec.Region = bad
		if err := o.ApplyApp(context.Background(), spec); err == nil {
			t.Errorf("ApplyApp accepted region %q", bad)
		}
	}
}

// The node list says which region each machine is in.
func TestNodesSayWhichRegionTheyAreIn(t *testing.T) {
	o, _ := spreadOrchestrator(t, regionNode("a", "fsn", "fsn-a"), regionNode("b", "", ""))
	nodes, err := o.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	got := map[string]orchestrator.NodeInfo{}
	for _, n := range nodes {
		got[n.Name] = n
	}
	if got["a"].Region != "fsn" || got["a"].Site != "fsn-a" || got["b"].Region != "" {
		t.Fatalf("nodes = %+v, want a in fsn at fsn-a and b in none", got)
	}
}
