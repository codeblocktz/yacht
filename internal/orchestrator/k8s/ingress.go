package k8s

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Annotations the ingress controller and the DNS controller read.
//
// Both are other people's software, named by string. They are written only when
// the corresponding setting is on, so an install running neither controller
// gets an Ingress with no annotations at all rather than configuration for
// something that is not there.
const (
	// externalDNSTarget makes ExternalDNS publish a CNAME to this value instead
	// of A records for the nodes. Without it a cluster exposes its own node
	// addresses in public DNS, which is both a disclosure and a promise that
	// those addresses will not change.
	externalDNSTarget = "external-dns.alpha.kubernetes.io/target"

	// traefikEntrypoints limits which of Traefik's entrypoints will route this
	// Ingress. Set to the secure one, a request arriving on plain HTTP is not
	// served rather than served and redirected.
	traefikEntrypoints = "traefik.ingress.kubernetes.io/router.entrypoints"

	// certManagerClusterIssuer has cert-manager's ingress-shim create a
	// Certificate for every TLS entry that names a Secret, and keep it renewed.
	// Entries naming none — the wildcard's — are skipped with a warning event
	// on the Ingress rather than failing the others.
	certManagerClusterIssuer = "cert-manager.io/cluster-issuer"
)

// ingressAnnotations returns what the spec asks the controllers for.
func ingressAnnotations(spec orchestrator.AppSpec) map[string]string {
	ann := map[string]string{}
	if spec.CNAMETarget != "" {
		ann[externalDNSTarget] = spec.CNAMETarget
	}
	if spec.HTTPSOnly {
		ann[traefikEntrypoints] = "websecure"
	}
	if len(spec.IssuedHosts) > 0 {
		ann[certManagerClusterIssuer] = spec.CertIssuer
	}
	return ann
}

// applyIngress routes the spec's hostnames to its Service.
//
// ingressClassName is left unset so the cluster's default IngressClass
// applies. Naming a class here would hard-code which controller is installed,
// which is the coupling this design otherwise avoids.
//
// The wildcard's TLS entry, when present, lists hosts and names no Secret. An Ingress's
// TLS Secret must live in the Ingress's own namespace, and every app has its
// own namespace — so one pre-provisioned wildcard cannot be referenced from
// all of them. The certificate comes from the ingress controller's configured
// default instead, which also keeps the private key out of tenant namespaces.
//
// Issued hosts are the opposite case and do name one: a certificate for a
// single custom domain belongs to that app alone, so its Secret lives beside it.
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

	// Only the hosts the default certificate can actually serve. Listing one it
	// cannot match does not conjure a certificate for it — it produces a
	// handshake the browser refuses, which is worse than plain HTTP because it
	// looks like the platform is broken rather than unconfigured.
	if len(spec.TLSHosts) > 0 {
		ingSpec = ingSpec.WithTLS(networkingv1ac.IngressTLS().
			WithHosts(spec.TLSHosts...))
	}

	// Each issued host is its own entry and its own Secret, so one domain
	// failing to issue does not hold back the rest. Nothing is served from the
	// Secret until it exists; until then the controller answers with its
	// default, which is no worse than having no entry at all.
	for _, host := range spec.IssuedHosts {
		ingSpec = ingSpec.WithTLS(networkingv1ac.IngressTLS().
			WithHosts(host).
			WithSecretName(orchestrator.CertSecretName(host)))
	}

	ing := networkingv1ac.Ingress(spec.Name, spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithSpec(ingSpec)

	if ann := ingressAnnotations(spec); len(ann) > 0 {
		ing = ing.WithAnnotations(ann)
	}

	if _, err := o.client.NetworkingV1().Ingresses(spec.Namespace).
		Apply(ctx, ing, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply ingress %s: %w", spec.Ref, err)
	}
	return nil
}

// Certificate reports whether the certificate issued for host is in place.
//
// Read from the Secret rather than cert-manager's Certificate resource, so
// this needs no client for somebody else's API: the Secret is only written once
// issuance has succeeded, and the certificate in it says when it expires.
func (o *Orchestrator) Certificate(ctx context.Context, ref orchestrator.Ref, host string) (orchestrator.Certificate, error) {
	sec, err := o.client.CoreV1().Secrets(ref.Namespace).
		Get(ctx, orchestrator.CertSecretName(host), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return orchestrator.Certificate{}, nil
	}
	if err != nil {
		return orchestrator.Certificate{}, fmt.Errorf("k8s: read certificate for %s: %w", host, err)
	}

	// The Secret holds the leaf followed by the chain it was issued with.
	var chain []*x509.Certificate
	rest := sec.Data[corev1.TLSCertKey]
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return orchestrator.Certificate{}, fmt.Errorf("k8s: parse certificate for %s: %w", host, err)
		}
		chain = append(chain, c)
	}
	if len(chain) == 0 {
		return orchestrator.Certificate{}, nil
	}
	leaf := chain[0]
	// A certificate for some other name in the Secret this host is served
	// from is not this host's certificate, whatever put it there.
	if leaf.VerifyHostname(host) != nil {
		return orchestrator.Certificate{}, nil
	}

	// Verified the way a browser would, against the system's roots, at a
	// moment it is valid — expiry is reported separately, and folding it in
	// here would describe an expired certificate as an untrusted one.
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, verr := leaf.Verify(x509.VerifyOptions{
		DNSName:       host,
		Intermediates: intermediates,
		CurrentTime:   leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2),
	})
	return orchestrator.Certificate{Issued: true, NotAfter: leaf.NotAfter, Trusted: verr == nil}, nil
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
