package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A TCP proxy is a NodePort Service of its own beside the app's ClusterIP one.
//
// A NodePort rather than a Traefik TCP route: Traefik routes TCP by SNI, which
// a database client does not send unless it speaks TLS from the first byte,
// and otherwise by entrypoint, one fixed port each, written into Traefik's own
// configuration and taking a restart of the controller every app goes through
// to add. A NodePort is answered by kube-proxy on every machine, needs nothing
// installed, and is one object per app that comes and goes with the proxy.
//
// Its own Service rather than turning the app's into a NodePort, so the address
// other apps use inside the cluster is never touched by the proxy being turned
// on or off.

// tcpProxyName is the proxy's Service, and the policy that lets the outside
// world through to it.
func tcpProxyName(app string) string { return app + "-tcp-proxy" }

// applyTCPProxy makes the proxy's Service, or removes it when the spec has no
// proxy port.
func (o *Orchestrator) applyTCPProxy(ctx context.Context, spec orchestrator.AppSpec) error {
	if spec.TCPProxyPort == 0 || spec.Port == 0 {
		return o.deleteTCPProxy(ctx, spec.Ref)
	}
	svc := corev1ac.Service(tcpProxyName(spec.Name), spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeNodePort).
			// Cluster, not Local: a connection to any machine reaches the app
			// wherever its pod runs, which is what lets the proxy's address be
			// one name for every machine rather than the one the pod is on.
			WithExternalTrafficPolicy(corev1.ServiceExternalTrafficPolicyCluster).
			WithSelector(orchestrator.SelectorLabels(spec.Name)).
			WithPorts(corev1ac.ServicePort().
				WithName("tcp").
				WithProtocol(corev1.ProtocolTCP).
				WithPort(spec.Port).
				WithTargetPort(intstr.FromInt32(spec.Port)).
				WithNodePort(spec.TCPProxyPort)))
	if _, err := o.client.CoreV1().Services(spec.Namespace).
		Apply(ctx, svc, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply tcp proxy %s: %w", spec.Ref, err)
	}
	// Under the same lock as the namespace's own policy, so a change of
	// isolation mode running beside this cannot leave the two disagreeing.
	o.isolating.RLock()
	defer o.isolating.RUnlock()
	return o.applyTCPProxyPolicy(ctx, o.Isolation(), spec)
}

// applyTCPProxyPolicy lets connections from anywhere reach the app's port.
//
// Needed because the namespace's isolation policy admits only the team's own
// namespaces and the platform's, and a connection through a NodePort arrives
// from a machine's address, which is neither: without this the proxy would
// answer and then hand every connection to a pod that drops it. One more policy
// rather than a change to the isolation one, because policies add up — this
// opens the one port, on the one app, for as long as its proxy exists.
//
// Written only while isolation is on. A policy selecting the pods is what
// isolates them, so with isolation off this would shut everything else out
// rather than let one more thing in.
func (o *Orchestrator) applyTCPProxyPolicy(
	ctx context.Context, iso orchestrator.Isolation, spec orchestrator.AppSpec,
) error {
	if iso.Mode == orchestrator.IsolationOff {
		return o.deleteTCPProxyPolicy(ctx, spec.Ref)
	}
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(spec.Port)
	p := networkingv1ac.NetworkPolicy(tcpProxyName(spec.Name), spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithSpec(networkingv1ac.NetworkPolicySpec().
			WithPodSelector(metav1ac.LabelSelector().
				WithMatchLabels(orchestrator.SelectorLabels(spec.Name))).
			WithPolicyTypes(networkingv1.PolicyTypeIngress).
			WithIngress(networkingv1ac.NetworkPolicyIngressRule().
				WithFrom(
					networkingv1ac.NetworkPolicyPeer().WithIPBlock(networkingv1ac.IPBlock().WithCIDR("0.0.0.0/0")),
					networkingv1ac.NetworkPolicyPeer().WithIPBlock(networkingv1ac.IPBlock().WithCIDR("::/0"))).
				WithPorts(networkingv1ac.NetworkPolicyPort().WithProtocol(tcp).WithPort(port))))
	if _, err := o.client.NetworkingV1().NetworkPolicies(spec.Namespace).
		Apply(ctx, p, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply tcp proxy policy %s: %w", spec.Ref, err)
	}
	return nil
}

// deleteTCPProxy removes an app's proxy and the policy that let it through,
// tolerating either's absence.
func (o *Orchestrator) deleteTCPProxy(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.CoreV1().Services(ref.Namespace).
		Delete(ctx, tcpProxyName(ref.Name), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete tcp proxy %s: %w", ref, err)
	}
	return o.deleteTCPProxyPolicy(ctx, ref)
}

func (o *Orchestrator) deleteTCPProxyPolicy(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.NetworkingV1().NetworkPolicies(ref.Namespace).
		Delete(ctx, tcpProxyName(ref.Name), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete tcp proxy policy %s: %w", ref, err)
	}
	return nil
}

// syncTCPProxyPolicies brings the proxies' policies in one namespace into line
// with the isolation in force, from the proxies' Services: EnsureIsolation's
// counterpart for them. Turning isolation off removes the namespace's own
// policy, and a proxy's left behind would then be the only policy selecting
// the app's pods — shutting out everything but the proxied port. Turning it on
// again would otherwise leave every proxy dropping connections until its app
// next deployed.
func (o *Orchestrator) syncTCPProxyPolicies(ctx context.Context, namespace string, iso orchestrator.Isolation) error {
	list, err := o.client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: orchestrator.LabelManagedBy + "=" + orchestrator.ManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("k8s: list tcp proxies in %q: %w", namespace, err)
	}
	for _, svc := range list.Items {
		name := svc.Labels[orchestrator.LabelApp]
		if svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Name != tcpProxyName(name) || len(svc.Spec.Ports) == 0 {
			continue
		}
		spec := orchestrator.AppSpec{
			Ref: orchestrator.Ref{
				Owner:     orchestrator.OwnerID(svc.Labels[orchestrator.LabelOwner]),
				Namespace: namespace, Name: name,
			},
			Port: svc.Spec.Ports[0].Port,
		}
		if err := o.applyTCPProxyPolicy(ctx, iso, spec); err != nil {
			return err
		}
	}
	return nil
}
