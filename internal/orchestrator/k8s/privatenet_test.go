package k8s

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// projectNetwork is web's namespace in project p1, with db beside it.
func projectNetwork() orchestrator.PrivateNetwork {
	return orchestrator.PrivateNetwork{
		Owner: "team-a", Namespace: "yacht-web", Project: "p1",
		Aliases: []orchestrator.PrivateAlias{{Name: "db", Host: "db.yacht-db.svc.cluster.local"}},
	}
}

func ensureNS(t *testing.T, o *Orchestrator, owner orchestrator.OwnerID, name string) {
	t.Helper()
	if err := o.EnsureNamespace(context.Background(), orchestrator.NamespaceSpec{Owner: owner, Name: name}); err != nil {
		t.Fatalf("EnsureNamespace %s: %v", name, err)
	}
}

// ingressFrom describes a policy's ingress peers, in order: a team's
// namespaces in a project, a team's not yet labelled with one, one namespace
// by name, or the platform's.
func ingressFrom(p *networkingv1.NetworkPolicy) []string {
	var out []string
	for _, peer := range p.Spec.Ingress[0].From {
		sel := peer.NamespaceSelector
		l := sel.MatchLabels
		switch {
		case len(l) == 0:
			out = append(out, "platform")
		case l[orchestrator.LabelProject] != "":
			out = append(out, l[orchestrator.LabelOwner]+" in "+l[orchestrator.LabelProject])
		case l[nameLabel] != "":
			out = append(out, l[orchestrator.LabelOwner]+" at "+l[nameLabel])
		case len(sel.MatchExpressions) == 1 && sel.MatchExpressions[0].Key == orchestrator.LabelProject &&
			sel.MatchExpressions[0].Operator == metav1.LabelSelectorOpDoesNotExist:
			out = append(out, l[orchestrator.LabelOwner]+" unlabelled")
		default:
			out = append(out, l[orchestrator.LabelOwner])
		}
	}
	return out
}

// A sync labels the namespace with its project, gives it its siblings'
// names, and lets in the project — not the rest of the team — and the
// platform.
func TestSyncPrivateNetworkGivesSiblingsNamesAndShutsOutOtherProjects(t *testing.T) {
	o, client := testOrchestrator(t)
	ctx := context.Background()
	ensureNS(t, o, "team-a", "yacht-web")
	client.CoreV1().Services("yacht-web").Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "yacht-web"},
	}, metav1.CreateOptions{})

	p := projectNetwork()
	// web is its own service's name, in production: that alias is skipped,
	// never put in place of the app's Service.
	p.Aliases = append(p.Aliases, orchestrator.PrivateAlias{Name: "web", Host: "web.yacht-other.svc.cluster.local"})
	if err := o.SyncPrivateNetwork(ctx, p); err != nil {
		t.Fatalf("SyncPrivateNetwork: %v", err)
	}

	ns, _ := client.CoreV1().Namespaces().Get(ctx, "yacht-web", metav1.GetOptions{})
	if ns.Labels[orchestrator.LabelProject] != "p1" || ns.Labels[orchestrator.LabelOwner] != "team-a" {
		t.Errorf("namespace labels = %v, want the project beside the owner", ns.Labels)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] == "" {
		t.Errorf("the sync lost the namespace's pod security labels: %v", ns.Labels)
	}

	db, err := client.CoreV1().Services("yacht-web").Get(ctx, "db", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("db's alias: %v", err)
	}
	if db.Spec.Type != corev1.ServiceTypeExternalName || db.Spec.ExternalName != "db.yacht-db.svc.cluster.local" ||
		db.Labels[orchestrator.LabelPrivateAlias] != "true" || db.Labels[orchestrator.LabelOwner] != "team-a" {
		t.Errorf("db's alias = %+v %+v, want an ExternalName to db's full name", db.Labels, db.Spec)
	}
	web, _ := client.CoreV1().Services("yacht-web").Get(ctx, "web", metav1.GetOptions{})
	if web.Spec.Type == corev1.ServiceTypeExternalName {
		t.Error("the app's own Service was replaced by an alias")
	}

	// Not the rest of team-a: only its namespaces in p1, those no pass has
	// labelled yet, and the platform.
	want := []string{"team-a in p1", "team-a unlabelled", "platform"}
	if got := ingressFrom(isolationOf(t, client, "yacht-web")); !slices.Equal(got, want) {
		t.Errorf("ingress from %v, want %v", got, want)
	}
}

// An app in another project whose variables name this one is still let in,
// by its namespace; the rest of the team is not.
func TestSyncPrivateNetworkLetsInPeersFromOtherProjects(t *testing.T) {
	o, client := testOrchestrator(t)
	ctx := context.Background()
	ensureNS(t, o, "team-a", "yacht-web")
	p := projectNetwork()
	p.Peers = []string{"yacht-api", "yacht-api"}
	if err := o.SyncPrivateNetwork(ctx, p); err != nil {
		t.Fatalf("SyncPrivateNetwork: %v", err)
	}
	withPeer := []string{"team-a in p1", "team-a unlabelled", "team-a at yacht-api", "platform"}
	if got := ingressFrom(isolationOf(t, client, "yacht-web")); !slices.Equal(got, withPeer) {
		t.Errorf("ingress from %v, want %v", got, withPeer)
	}

	// A deploy rebuilds the policy from the namespace, so the peers stay.
	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: "team-a", Name: "yacht-web", Project: "p1"}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	if got := ingressFrom(isolationOf(t, client, "yacht-web")); !slices.Equal(got, withPeer) {
		t.Errorf("after a deploy, ingress from %v, want %v", got, withPeer)
	}
	// And so does a change of mode.
	if err := o.SetIsolation(ctx, orchestrator.Isolation{Mode: orchestrator.IsolationStrict}); err != nil {
		t.Fatalf("SetIsolation: %v", err)
	}
	if got := ingressFrom(isolationOf(t, client, "yacht-web")); !slices.Equal(got, withPeer) {
		t.Errorf("after a change of mode, ingress from %v, want %v", got, withPeer)
	}

	// When nothing refers to it any more, the peer goes.
	p.Peers = nil
	if err := o.SyncPrivateNetwork(ctx, p); err != nil {
		t.Fatalf("SyncPrivateNetwork: %v", err)
	}
	want := []string{"team-a in p1", "team-a unlabelled", "platform"}
	if got := ingressFrom(isolationOf(t, client, "yacht-web")); !slices.Equal(got, want) {
		t.Errorf("ingress from %v, want %v", got, want)
	}
	ns, _ := client.CoreV1().Namespaces().Get(ctx, "yacht-web", metav1.GetOptions{})
	if _, ok := ns.Annotations[orchestrator.AnnotationPrivatePeers]; ok {
		t.Errorf("annotations = %v, want the peers removed", ns.Annotations)
	}
}

// A namespace a deploy has labelled with its project is isolated to it from
// the first apply, before any sync: its siblings, labelled the same way,
// reach it, and the rest of the team does not.
func TestEnsureNamespaceWithAProjectIsolatesToIt(t *testing.T) {
	o, client := testOrchestrator(t)
	if err := o.EnsureNamespace(context.Background(),
		orchestrator.NamespaceSpec{Owner: "team-a", Name: "yacht-web", Project: "p1"}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	want := []string{"team-a in p1", "team-a unlabelled", "platform"}
	if got := ingressFrom(isolationOf(t, client, "yacht-web")); !slices.Equal(got, want) {
		t.Errorf("ingress from %v, want %v", got, want)
	}
}

// A sibling that has gone takes its name with it.
func TestSyncPrivateNetworkRemovesAGoneSiblingsName(t *testing.T) {
	o, client := testOrchestrator(t)
	ctx := context.Background()
	ensureNS(t, o, "team-a", "yacht-web")
	if err := o.SyncPrivateNetwork(ctx, projectNetwork()); err != nil {
		t.Fatalf("SyncPrivateNetwork: %v", err)
	}
	p := projectNetwork()
	p.Aliases = nil
	if err := o.SyncPrivateNetwork(ctx, p); err != nil {
		t.Fatalf("SyncPrivateNetwork: %v", err)
	}
	if _, err := client.CoreV1().Services("yacht-web").Get(ctx, "db", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("db's alias after db went: err = %v, want it gone", err)
	}
}

// A sync never makes a namespace — one racing an app's deletion would come
// back empty — and never touches another team's.
func TestSyncPrivateNetworkLeavesMissingAndOtherTeamsNamespacesAlone(t *testing.T) {
	o, client := testOrchestrator(t)
	ctx := context.Background()
	if err := o.SyncPrivateNetwork(ctx, projectNetwork()); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Fatalf("SyncPrivateNetwork of a missing namespace = %v, want ErrNotFound", err)
	}
	if _, err := client.CoreV1().Namespaces().Get(ctx, "yacht-web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the sync made the namespace: err = %v", err)
	}

	ensureNS(t, o, "team-b", "yacht-web")
	if err := o.SyncPrivateNetwork(ctx, projectNetwork()); err == nil {
		t.Error("team-a's sync of team-b's namespace succeeded")
	}
	if svcs, _ := client.CoreV1().Services("yacht-web").List(ctx, metav1.ListOptions{}); len(svcs.Items) != 0 {
		t.Errorf("team-b's namespace has %d services from team-a's sync", len(svcs.Items))
	}
}

// Isolation off still gives siblings their names: they are DNS, not policy.
func TestPrivateNamesDoNotNeedIsolation(t *testing.T) {
	o, client := testOrchestrator(t)
	ctx := context.Background()
	o.WithIsolation(orchestrator.Isolation{Mode: orchestrator.IsolationOff})
	ensureNS(t, o, "team-a", "yacht-web")
	if err := o.SyncPrivateNetwork(ctx, projectNetwork()); err != nil {
		t.Fatalf("SyncPrivateNetwork: %v", err)
	}
	if _, err := client.CoreV1().Services("yacht-web").Get(ctx, "db", metav1.GetOptions{}); err != nil {
		t.Errorf("db's alias: %v", err)
	}
	if _, err := client.NetworkingV1().NetworkPolicies("yacht-web").Get(ctx, isolationPolicy, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a policy was written with isolation off: err = %v", err)
	}
}

// The CoreDNS rule rewrites only `<name>.internal` asked from an engine
// namespace, and to the asker's own namespace.
func TestPrivateDNSRewriteIsScopedToTheEnginesNamespaces(t *testing.T) {
	for _, want := range []string{
		`\.internal\.(yacht-[a-z0-9-]+)\.svc\.cluster\.local\.$`,
		`{1}.{3}.svc.cluster.local.`,
		"answer auto",
	} {
		if !strings.Contains(orchestrator.PrivateDNSRewrite, want) {
			t.Errorf("PrivateDNSRewrite lacks %q", want)
		}
	}
}
