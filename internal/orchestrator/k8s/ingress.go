package k8s

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// applyIngress routes the spec's hostnames to its Service.
//
// ingressClassName is left unset so the cluster's default IngressClass
// applies. Naming a class here would hard-code which controller is installed,
// which is the coupling this design otherwise avoids.
//
// The TLS block, when present, lists hosts and names no Secret. An Ingress's
// TLS Secret must live in the Ingress's own namespace, and every app has its
// own namespace — so one pre-provisioned wildcard cannot be referenced from
// all of them. The certificate comes from the ingress controller's configured
// default instead, which also keeps the private key out of tenant namespaces.
func (o *Orchestrator) applyIngress(ctx context.Context, spec orchestrator.AppSpec) error {
	pathType := networkingv1.PathTypePrefix

	rules := make([]*networkingv1ac.IngressRuleApplyConfiguration, 0, len(spec.Hosts))
	for _, host := range spec.Hosts {
		rules = append(rules, networkingv1ac.IngressRule().
			WithHost(host).
			WithHTTP(networkingv1ac.HTTPIngressRuleValue().
				WithPaths(networkingv1ac.HTTPIngressPath().
					WithPath("/").
					WithPathType(pathType).
					WithBackend(networkingv1ac.IngressBackend().
						WithService(networkingv1ac.IngressServiceBackend().
							WithName(spec.Name).
							WithPort(networkingv1ac.ServiceBackendPort().
								WithNumber(servicePort)))))))
	}

	ingSpec := networkingv1ac.IngressSpec().WithRules(rules...)

	if spec.TLS {
		ingSpec = ingSpec.WithTLS(networkingv1ac.IngressTLS().
			WithHosts(spec.Hosts...))
	}

	ing := networkingv1ac.Ingress(spec.Name, spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithSpec(ingSpec)

	if _, err := o.client.NetworkingV1().Ingresses(spec.Namespace).
		Apply(ctx, ing, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply ingress %s: %w", spec.Ref, err)
	}
	return nil
}

// deleteIngress removes an app's Ingress, tolerating its absence.
func (o *Orchestrator) deleteIngress(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.NetworkingV1().Ingresses(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete ingress %s: %w", ref, err)
	}
	return nil
}

// deleteService removes an app's Service, tolerating its absence.
//
// Needed because clearing an app's port stops the Service being applied but
// does not remove one already there. Converging only forward leaves the old
// object serving traffic nobody asked for.
func (o *Orchestrator) deleteService(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.CoreV1().Services(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete service %s: %w", ref, err)
	}
	return nil
}
