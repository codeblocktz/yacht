package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// isolationPolicy is the NetworkPolicy the engine writes in each namespace it
// manages. One per namespace, so changing the mode rewrites it in place.
const isolationPolicy = "yacht-isolation"

// nameLabel is the label Kubernetes gives every namespace with its own name.
const nameLabel = "kubernetes.io/metadata.name"

// privateRanges are refused as destinations under strict isolation: the
// nodes' own network, the cluster's pods and services, the link-local range
// cloud metadata services answer on, and carrier-grade NAT. Everything a
// workload has any business reaching there is its own team — allowed by
// namespace — the ingress controller, which the public reaches too, or named
// in EgressAllow.
var privateRanges = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16",
}

// WithIsolation sets the network isolation the orchestrator enforces.
func (o *Orchestrator) WithIsolation(i orchestrator.Isolation) *Orchestrator {
	o.isolationMu.Lock()
	o.isolation = i.OrDefault()
	o.isolationMu.Unlock()
	return o
}

// Isolation is the network isolation in force.
func (o *Orchestrator) Isolation() orchestrator.Isolation {
	o.isolationMu.RLock()
	defer o.isolationMu.RUnlock()
	return o.isolation
}

// SetIsolation changes the isolation while the engine runs, and brings every
// namespace into line with it before returning.
func (o *Orchestrator) SetIsolation(ctx context.Context, i orchestrator.Isolation) error {
	o.WithIsolation(i)
	return o.EnsureIsolation(ctx)
}

// teamIsolation is the policy for a team's namespace.
//
// Into it: the team's own namespaces, and the platform's — every namespace
// the engine did not make for a team, which is where the ingress controller
// and the engine's waker run — except the one builds run in, where a
// customer's Dockerfile executes. Isolating at the receiving end is what
// keeps teams apart: whatever another team's workload may send, nothing here
// accepts it.
//
// Kubernetes' own health checks come from the node, not from a pod, and
// NetworkPolicy does not hold them back.
func teamIsolation(namespace, owner string, iso orchestrator.Isolation) *networkingv1ac.NetworkPolicyApplyConfiguration {
	sameTeam := networkingv1ac.NetworkPolicyPeer().WithNamespaceSelector(
		metav1ac.LabelSelector().WithMatchLabels(map[string]string{orchestrator.LabelOwner: owner}))
	platform := networkingv1ac.NetworkPolicyPeer().WithNamespaceSelector(
		metav1ac.LabelSelector().WithMatchExpressions(
			metav1ac.LabelSelectorRequirement().
				WithKey(orchestrator.LabelOwner).WithOperator(metav1.LabelSelectorOpDoesNotExist),
			metav1ac.LabelSelectorRequirement().
				WithKey(nameLabel).WithOperator(metav1.LabelSelectorOpNotIn).WithValues(BuildNamespace),
		))

	spec := networkingv1ac.NetworkPolicySpec().
		WithPodSelector(metav1ac.LabelSelector()).
		WithPolicyTypes(networkingv1.PolicyTypeIngress).
		WithIngress(networkingv1ac.NetworkPolicyIngressRule().WithFrom(sameTeam, platform))

	if iso.Mode == orchestrator.IsolationStrict {
		spec = spec.WithPolicyTypes(networkingv1.PolicyTypeEgress).
			WithEgress(dnsEgress(),
				networkingv1ac.NetworkPolicyEgressRule().WithTo(networkingv1ac.NetworkPolicyPeer().
					WithNamespaceSelector(metav1ac.LabelSelector().
						WithMatchLabels(map[string]string{orchestrator.LabelOwner: owner}))),
				publicEgress(iso.EgressAllow), ingressEgress(), loadBalancerEgress())
	}
	return policy(namespace, spec)
}

// buildIsolation is the policy for the namespace builds run in. Nothing
// connects to a build, so nothing is let in. Under strict isolation a build
// reaches DNS and public addresses — its repository, base images, package
// registries, and a registry this cluster serves itself through its ingress —
// and the private ranges named in EgressAllow, where an image registry on the
// LAN would be.
func buildIsolation(iso orchestrator.Isolation) *networkingv1ac.NetworkPolicyApplyConfiguration {
	spec := networkingv1ac.NetworkPolicySpec().
		WithPodSelector(metav1ac.LabelSelector()).
		WithPolicyTypes(networkingv1.PolicyTypeIngress)
	if iso.Mode == orchestrator.IsolationStrict {
		spec = spec.WithPolicyTypes(networkingv1.PolicyTypeEgress).
			WithEgress(dnsEgress(), publicEgress(iso.EgressAllow), ingressEgress(), loadBalancerEgress())
	}
	return policy(BuildNamespace, spec)
}

func policy(namespace string, spec *networkingv1ac.NetworkPolicySpecApplyConfiguration) *networkingv1ac.NetworkPolicyApplyConfiguration {
	return networkingv1ac.NetworkPolicy(isolationPolicy, namespace).
		WithLabels(map[string]string{orchestrator.LabelManagedBy: orchestrator.ManagedByValue}).
		WithSpec(spec)
}

// dnsEgress lets a workload ask the cluster's DNS, in kube-system, for names.
func dnsEgress() *networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	port := intstr.FromInt32(53)
	return networkingv1ac.NetworkPolicyEgressRule().
		WithTo(networkingv1ac.NetworkPolicyPeer().WithNamespaceSelector(
			metav1ac.LabelSelector().WithMatchLabels(map[string]string{nameLabel: "kube-system"}))).
		WithPorts(
			networkingv1ac.NetworkPolicyPort().WithProtocol(udp).WithPort(port),
			networkingv1ac.NetworkPolicyPort().WithProtocol(tcp).WithPort(port))
}

// publicEgress is every public IPv4 address, and the private ranges allowed.
func publicEgress(allow []netip.Prefix) *networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration {
	rule := networkingv1ac.NetworkPolicyEgressRule().WithTo(
		networkingv1ac.NetworkPolicyPeer().WithIPBlock(
			networkingv1ac.IPBlock().WithCIDR("0.0.0.0/0").WithExcept(privateRanges...)))
	for _, p := range allow {
		rule = rule.WithTo(networkingv1ac.NetworkPolicyPeer().WithIPBlock(
			networkingv1ac.IPBlock().WithCIDR(p.String())))
	}
	return rule
}

// ingressNamespaces are where the ingress controller runs: K3s's own Traefik
// in kube-system, or one installed in its own namespace. The same places the
// access log is read from (httplog.go).
var ingressNamespaces = []string{"kube-system", "traefik"}

// ingressEgress lets a workload reach the ingress controller's HTTP and HTTPS
// ports, which anyone on the internet reaches already: a request for a public
// address this cluster serves, made from inside it, arrives there by a private
// address. Kubernetes rewrites the destination before the policy is asked —
// a Service's address, or a node's own public address, becomes the pod behind
// it — so publicEgress alone refuses an app calling another app's public URL,
// or a build pushing to a registry the cluster serves, whenever the way in is
// on the same cluster. Only the entrypoints, never its dashboard or metrics.
func ingressEgress() *networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration {
	tcp := corev1.ProtocolTCP
	return networkingv1ac.NetworkPolicyEgressRule().
		WithTo(networkingv1ac.NetworkPolicyPeer().
			WithNamespaceSelector(metav1ac.LabelSelector().WithMatchExpressions(
				metav1ac.LabelSelectorRequirement().
					WithKey(nameLabel).WithOperator(metav1.LabelSelectorOpIn).WithValues(ingressNamespaces...))).
			WithPodSelector(metav1ac.LabelSelector().
				WithMatchLabels(map[string]string{"app.kubernetes.io/name": "traefik"}))).
		WithPorts(
			networkingv1ac.NetworkPolicyPort().WithProtocol(tcp).WithPort(intstr.FromString("web")),
			networkingv1ac.NetworkPolicyPort().WithProtocol(tcp).WithPort(intstr.FromString("websecure")))
}

// loadBalancerEgress is the other way K3s rewrites a request for a node's own
// public address: to the load balancer pod that binds 80 and 443 on that node
// (ServiceLB's svclb-traefik), which hands it on to the ingress controller.
// Its pods run in kube-system, the only namespace K3s puts them in.
func loadBalancerEgress() *networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration {
	tcp := corev1.ProtocolTCP
	return networkingv1ac.NetworkPolicyEgressRule().
		WithTo(networkingv1ac.NetworkPolicyPeer().
			WithNamespaceSelector(metav1ac.LabelSelector().
				WithMatchLabels(map[string]string{nameLabel: "kube-system"})).
			WithPodSelector(metav1ac.LabelSelector().
				WithMatchLabels(map[string]string{"svccontroller.k3s.cattle.io/svcname": "traefik"}))).
		WithPorts(
			networkingv1ac.NetworkPolicyPort().WithProtocol(tcp).WithPort(intstr.FromInt32(80)),
			networkingv1ac.NetworkPolicyPort().WithProtocol(tcp).WithPort(intstr.FromInt32(443)))
}

// isolate writes one namespace's policy, as build makes it of the isolation
// in force.
func (o *Orchestrator) isolate(ctx context.Context, build func(orchestrator.Isolation) *networkingv1ac.NetworkPolicyApplyConfiguration) error {
	o.isolating.RLock()
	defer o.isolating.RUnlock()
	iso := o.Isolation()
	return o.applyIsolation(ctx, iso, build(iso))
}

// applyIsolation writes, or under IsolationOff removes, a namespace's policy.
func (o *Orchestrator) applyIsolation(ctx context.Context, iso orchestrator.Isolation, p *networkingv1ac.NetworkPolicyApplyConfiguration) error {
	namespace, name := *p.Namespace, *p.Name
	if iso.Mode == orchestrator.IsolationOff {
		err := o.client.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: remove network policy in %q: %w", namespace, err)
		}
		return nil
	}
	if _, err := o.client.NetworkingV1().NetworkPolicies(namespace).Apply(ctx, p, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply network policy in %q: %w", namespace, err)
	}
	return nil
}

// EnsureIsolation brings every namespace the engine manages into line with
// its isolation: those made before isolation existed, or before its mode
// changed, would otherwise wait for their next deploy.
func (o *Orchestrator) EnsureIsolation(ctx context.Context) error {
	o.isolating.Lock()
	defer o.isolating.Unlock()
	iso := o.Isolation()
	list, err := o.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: orchestrator.LabelManagedBy + "=" + orchestrator.ManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("k8s: list namespaces to isolate: %w", err)
	}
	n := 0
	for _, ns := range list.Items {
		var p *networkingv1ac.NetworkPolicyApplyConfiguration
		switch owner := ns.Labels[orchestrator.LabelOwner]; {
		case ns.Name == BuildNamespace:
			p = buildIsolation(iso)
		case owner != "":
			p = teamIsolation(ns.Name, owner, iso)
		default:
			continue
		}
		if err := o.applyIsolation(ctx, iso, p); err != nil {
			return err
		}
		if ns.Name != BuildNamespace {
			if err := o.syncTCPProxyPolicies(ctx, ns.Name, iso); err != nil {
				return err
			}
		}
		n++
	}
	o.log.Info("network isolation in place",
		slog.String("mode", string(iso.Mode)), slog.Int("namespaces", n))
	return nil
}

var _ orchestrator.NetworkIsolator = (*Orchestrator)(nil)
