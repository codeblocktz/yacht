package k8s

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplyAppCreatesIngressForHosts(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}

	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "web.apps.example.com" {
		t.Fatalf("rules = %+v, want one rule for web.apps.example.com", ing.Spec.Rules)
	}

	// The backend is the Service's stable port, not the container port. That
	// indirection is why changing an app's port does not rewrite its routing.
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend.Name != spec.Name || backend.Port.Number != servicePort {
		t.Fatalf("backend = %s:%d, want %s:%d",
			backend.Name, backend.Port.Number, spec.Name, servicePort)
	}

	if ing.Spec.IngressClassName != nil {
		t.Fatalf("ingressClassName = %q, want unset so the cluster default applies",
			*ing.Spec.IngressClassName)
	}
}

func TestApplyAppCreatesNoIngressWithoutHosts(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if err := o.ApplyApp(ctx, testSpec()); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses("yacht-demo").
		Get(ctx, "web", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// TLS carries the hosts and no secretName. The absence is the whole mechanism:
// the certificate comes from the controller's default, and an implementation
// that "helpfully" fills a secret name silently reintroduces the cross-
// namespace problem this design exists to avoid.
func TestIngressTLSCarriesNoSecretName(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	spec.TLS = true

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}

	if len(ing.Spec.TLS) != 1 {
		t.Fatalf("tls = %+v, want one entry", ing.Spec.TLS)
	}
	if ing.Spec.TLS[0].SecretName != "" {
		t.Fatalf("secretName = %q, want empty", ing.Spec.TLS[0].SecretName)
	}
	if len(ing.Spec.TLS[0].Hosts) != 1 || ing.Spec.TLS[0].Hosts[0] != "web.apps.example.com" {
		t.Fatalf("tls hosts = %v, want [web.apps.example.com]", ing.Spec.TLS[0].Hosts)
	}
}

func TestApplyAppWithoutTLSEmitsNoTLSBlock(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	spec.TLS = false

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if len(ing.Spec.TLS) != 0 {
		t.Fatalf("tls = %+v, want none", ing.Spec.TLS)
	}
}

// Removing the last hostname must remove the Ingress. Converging only forward
// leaves an app routable at a name it no longer owns.
func TestApplyAppPrunesIngressWhenHostsGoAway(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	spec.Hosts = nil
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp without hosts: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want the ingress pruned, got %v", err)
	}
}

// Clearing the port already skipped Service creation but left any existing
// Service behind. Same class of bug as the Ingress one, fixed at the same time.
func TestApplyAppPrunesServiceAndIngressWhenPortCleared(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	spec.Port = 0
	spec.Hosts = nil
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp with no port: %v", err)
	}

	if _, err := client.CoreV1().Services(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("want the service pruned, got %v", err)
	}
	if _, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("want the ingress pruned, got %v", err)
	}
}

func TestDeleteAppRemovesIngress(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	if err := o.DeleteApp(ctx, spec.Ref); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestDeleteAppIsIdempotentWithNoIngress(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)

	// Deleting an app that never had an Ingress must not error, or a delete
	// after a partial apply becomes unretryable.
	if err := o.DeleteApp(ctx, testSpec().Ref); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
}
