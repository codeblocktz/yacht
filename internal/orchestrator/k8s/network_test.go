package k8s

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func isolationOf(t *testing.T, client *fake.Clientset, namespace string) *networkingv1.NetworkPolicy {
	t.Helper()
	p, err := client.NetworkingV1().NetworkPolicies(namespace).Get(context.Background(), isolationPolicy, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("network policy in %s: %v", namespace, err)
	}
	return p
}

// A team's namespace accepts connections from its own team's namespaces and
// from the platform's, never from another team's or from a build — and, by
// default, says nothing about where its workloads connect out to.
func TestATeamsNamespaceAcceptsOnlyItsTeamAndThePlatform(t *testing.T) {
	o, client := testOrchestrator(t)
	if err := o.EnsureNamespace(context.Background(), orchestrator.NamespaceSpec{Owner: "team-a", Name: "yacht-a1"}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	p := isolationOf(t, client, "yacht-a1")

	if len(p.Spec.PodSelector.MatchLabels)+len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Error("the policy does not cover every pod in the namespace")
	}
	if !slices.Equal(p.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}) {
		t.Errorf("policy types = %v, want ingress only by default", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].From) != 2 {
		t.Fatalf("ingress = %+v, want one rule with two peers", p.Spec.Ingress)
	}
	team, platform := p.Spec.Ingress[0].From[0], p.Spec.Ingress[0].From[1]
	if team.NamespaceSelector == nil || team.NamespaceSelector.MatchLabels[orchestrator.LabelOwner] != "team-a" {
		t.Errorf("the first peer is %+v, want the team's own namespaces", team)
	}
	sel := platform.NamespaceSelector
	if sel == nil || len(sel.MatchExpressions) != 2 ||
		sel.MatchExpressions[0].Key != orchestrator.LabelOwner || sel.MatchExpressions[0].Operator != metav1.LabelSelectorOpDoesNotExist ||
		sel.MatchExpressions[1].Key != nameLabel || sel.MatchExpressions[1].Operator != metav1.LabelSelectorOpNotIn ||
		!slices.Equal(sel.MatchExpressions[1].Values, []string{BuildNamespace}) {
		t.Errorf("the platform peer is %+v, want namespaces with no team, builds excepted", sel)
	}
}

// Strict isolation also limits the way out: DNS, the team's own apps, and
// public addresses, with private ranges refused but for those allowed.
func TestStrictIsolationLimitsWhereWorkloadsConnectTo(t *testing.T) {
	o, client := testOrchestrator(t)
	lan := netip.MustParsePrefix("10.20.0.0/24")
	o.WithIsolation(orchestrator.Isolation{Mode: orchestrator.IsolationStrict, EgressAllow: []netip.Prefix{lan}})
	if err := o.EnsureNamespace(context.Background(), orchestrator.NamespaceSpec{Owner: "team-a", Name: "yacht-a1"}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	p := isolationOf(t, client, "yacht-a1")

	if !slices.Contains(p.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Fatalf("policy types = %v, want egress too", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Egress) != 3 {
		t.Fatalf("egress = %+v, want DNS, the team and public addresses", p.Spec.Egress)
	}
	dns, team, public := p.Spec.Egress[0], p.Spec.Egress[1], p.Spec.Egress[2]
	if len(dns.Ports) != 2 || dns.Ports[0].Port.IntValue() != 53 || *dns.Ports[0].Protocol != corev1.ProtocolUDP ||
		dns.To[0].NamespaceSelector.MatchLabels[nameLabel] != "kube-system" {
		t.Errorf("the DNS rule is %+v", dns)
	}
	if team.To[0].NamespaceSelector.MatchLabels[orchestrator.LabelOwner] != "team-a" {
		t.Errorf("the team rule is %+v", team)
	}
	if len(public.To) != 2 {
		t.Fatalf("the public rule is %+v, want everything public and the allowed range", public)
	}
	all := public.To[0].IPBlock
	if all == nil || all.CIDR != "0.0.0.0/0" {
		t.Fatalf("the public rule's block is %+v", all)
	}
	for _, private := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"} {
		if !slices.Contains(all.Except, private) {
			t.Errorf("%s is reachable under strict isolation", private)
		}
	}
	if public.To[1].IPBlock == nil || public.To[1].IPBlock.CIDR != lan.String() {
		t.Errorf("the allowed range is %+v, want %s", public.To[1], lan)
	}
}

// Nothing connects to a build; under strict isolation a build reaches DNS
// and public addresses only.
func TestBuildsAcceptNothingAndStrictlyReachOnlyThePublic(t *testing.T) {
	for _, mode := range []orchestrator.IsolationMode{orchestrator.IsolationTeam, orchestrator.IsolationStrict} {
		t.Run(string(mode), func(t *testing.T) {
			o, client := testOrchestrator(t)
			o.WithIsolation(orchestrator.Isolation{Mode: mode})
			if err := o.ensureBuildNamespace(context.Background()); err != nil {
				t.Fatalf("ensureBuildNamespace: %v", err)
			}
			p := isolationOf(t, client, BuildNamespace)
			if len(p.Spec.Ingress) != 0 || !slices.Contains(p.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
				t.Errorf("builds accept %+v, want nothing", p.Spec.Ingress)
			}
			strict := mode == orchestrator.IsolationStrict
			if slices.Contains(p.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) != strict {
				t.Errorf("egress limited = %t under %s", !strict, mode)
			}
			if strict && len(p.Spec.Egress) != 2 {
				t.Errorf("a strict build's egress is %+v, want DNS and public addresses", p.Spec.Egress)
			}
		})
	}
}

// Turning isolation off takes away what was written, rather than leaving
// policies nobody is maintaining any more.
func TestIsolationOffRemovesThePolicy(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := orchestrator.NamespaceSpec{Owner: "team-a", Name: "yacht-a1"}
	if err := o.EnsureNamespace(ctx, spec); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	o.WithIsolation(orchestrator.Isolation{Mode: orchestrator.IsolationOff})
	if err := o.EnsureNamespace(ctx, spec); err != nil {
		t.Fatalf("EnsureNamespace with isolation off: %v", err)
	}
	if _, err := client.NetworkingV1().NetworkPolicies("yacht-a1").Get(ctx, isolationPolicy, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the policy is still there with isolation off: %v", err)
	}
}

// Namespaces made before isolation existed are isolated at startup: each
// team's, and the builds'. Namespaces the engine did not make are left alone.
func TestEnsureIsolationCoversNamespacesMadeBefore(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	managed := map[string]string{orchestrator.LabelManagedBy: orchestrator.ManagedByValue}
	for _, ns := range []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "yacht-old", Labels: map[string]string{
			orchestrator.LabelManagedBy: orchestrator.ManagedByValue, orchestrator.LabelOwner: "team-b"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: BuildNamespace, Labels: managed}},
		{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	} {
		if _, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", ns.Name, err)
		}
	}
	if err := o.EnsureIsolation(ctx); err != nil {
		t.Fatalf("EnsureIsolation: %v", err)
	}
	if p := isolationOf(t, client, "yacht-old"); p.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels[orchestrator.LabelOwner] != "team-b" {
		t.Errorf("the old team namespace's policy is %+v", p.Spec.Ingress)
	}
	isolationOf(t, client, BuildNamespace)
	if _, err := client.NetworkingV1().NetworkPolicies("kube-system").Get(ctx, isolationPolicy, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a namespace the engine did not make was given a policy: %v", err)
	}
}

// Changing the isolation while running rewrites every namespace's policy at
// once — tighter, off, and back — and what is deployed afterwards gets the
// new one.
func TestSetIsolationRewritesEveryNamespace(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: "team-a", Name: "yacht-a1"}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	if got := isolationOf(t, client, "yacht-a1").Spec.PolicyTypes; len(got) != 1 {
		t.Fatalf("policy types under team isolation = %v", got)
	}

	if err := o.SetIsolation(ctx, orchestrator.Isolation{Mode: orchestrator.IsolationStrict}); err != nil {
		t.Fatalf("SetIsolation strict: %v", err)
	}
	if got := o.Isolation().Mode; got != orchestrator.IsolationStrict {
		t.Errorf("Isolation().Mode = %q after setting strict", got)
	}
	if got := isolationOf(t, client, "yacht-a1").Spec.PolicyTypes; !slices.Contains(got, networkingv1.PolicyTypeEgress) {
		t.Errorf("policy types after strict = %v, want egress limited too", got)
	}

	if err := o.SetIsolation(ctx, orchestrator.Isolation{Mode: orchestrator.IsolationOff}); err != nil {
		t.Fatalf("SetIsolation off: %v", err)
	}
	if _, err := client.NetworkingV1().NetworkPolicies("yacht-a1").Get(ctx, isolationPolicy, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the policy is still there with isolation turned off: %v", err)
	}

	// The zero value is team isolation, as at startup.
	if err := o.SetIsolation(ctx, orchestrator.Isolation{}); err != nil {
		t.Fatalf("SetIsolation team: %v", err)
	}
	isolationOf(t, client, "yacht-a1")
	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: "team-b", Name: "yacht-b1"}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	if got := isolationOf(t, client, "yacht-b1").Spec.PolicyTypes; len(got) != 1 {
		t.Errorf("a namespace made after the change has policy types %v, want team isolation's", got)
	}
}

// Deploys racing a change of mode never leave a namespace on the old one.
func TestSetIsolationWinsOverDeploysInFlight(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	names := []string{"yacht-r1", "yacht-r2", "yacht-r3", "yacht-r4"}
	for _, n := range names {
		if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: "team-r", Name: n}); err != nil {
			t.Fatalf("EnsureNamespace: %v", err)
		}
	}
	done := make(chan error, len(names)*10)
	for i := 0; i < 10; i++ {
		for _, n := range names {
			go func() { done <- o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: "team-r", Name: n}) }()
		}
	}
	if err := o.SetIsolation(ctx, orchestrator.Isolation{Mode: orchestrator.IsolationStrict}); err != nil {
		t.Fatalf("SetIsolation: %v", err)
	}
	for range len(names) * 10 {
		if err := <-done; err != nil {
			t.Fatalf("EnsureNamespace: %v", err)
		}
	}
	for _, n := range names {
		if got := isolationOf(t, client, n).Spec.PolicyTypes; !slices.Contains(got, networkingv1.PolicyTypeEgress) {
			t.Errorf("%s was left with policy types %v after the change to strict", n, got)
		}
	}
}

func TestParseIsolation(t *testing.T) {
	for _, tc := range []struct {
		mode, allow string
		want        orchestrator.IsolationMode
		allowed     int
		bad         bool
	}{
		{"", "", orchestrator.IsolationTeam, 0, false},
		{"Strict", "10.0.0.0/24, 192.168.1.5/32", orchestrator.IsolationStrict, 2, false},
		{"off", "", orchestrator.IsolationOff, 0, false},
		{"paranoid", "", "", 0, true},
		{"strict", "10.0.0.0", "", 0, true},
	} {
		got, err := orchestrator.ParseIsolation(tc.mode, tc.allow)
		if (err != nil) != tc.bad {
			t.Errorf("ParseIsolation(%q, %q) error = %v, want error %t", tc.mode, tc.allow, err, tc.bad)
			continue
		}
		if !tc.bad && (got.Mode != tc.want || len(got.EgressAllow) != tc.allowed) {
			t.Errorf("ParseIsolation(%q, %q) = %+v", tc.mode, tc.allow, got)
		}
	}
}
