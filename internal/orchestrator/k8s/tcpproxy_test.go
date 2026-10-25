package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A proxy is a NodePort on the port it was given, reaching the app's own
// port, with a policy that lets the outside world through to that port alone.
func TestApplyAppMakesTheTCPProxy(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	spec.Port, spec.Internal, spec.TCPProxyPort = 5432, true, 30042

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	svc, err := client.CoreV1().Services(spec.Namespace).Get(ctx, "web-tcp-proxy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort || len(svc.Spec.Ports) != 1 {
		t.Fatalf("proxy = %+v, want one NodePort", svc.Spec)
	}
	p := svc.Spec.Ports[0]
	if p.NodePort != 30042 || p.Port != 5432 || p.TargetPort.IntVal != 5432 {
		t.Errorf("proxy port = %d -> %d -> %d, want 30042 -> 5432 -> 5432", p.NodePort, p.Port, p.TargetPort.IntVal)
	}
	if svc.Spec.Selector[orchestrator.LabelApp] != "web" {
		t.Errorf("proxy selects %v, want the app's pods", svc.Spec.Selector)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Errorf("externalTrafficPolicy = %q, want Cluster so every machine answers", svc.Spec.ExternalTrafficPolicy)
	}

	// The app's own Service is untouched: still the address other apps use.
	own, err := client.CoreV1().Services(spec.Namespace).Get(ctx, "web", metav1.GetOptions{})
	if err != nil || own.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("the app's Service = %+v, %v; want it left a ClusterIP", own, err)
	}

	pol, err := client.NetworkingV1().NetworkPolicies(spec.Namespace).Get(ctx, "web-tcp-proxy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy policy: %v", err)
	}
	if len(pol.Spec.Ingress) != 1 || len(pol.Spec.Ingress[0].Ports) != 1 ||
		pol.Spec.Ingress[0].Ports[0].Port.IntVal != 5432 ||
		pol.Spec.Ingress[0].From[0].IPBlock == nil || pol.Spec.Ingress[0].From[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("proxy policy = %+v, want anywhere in to 5432 only", pol.Spec)
	}

	// Turned off, both go.
	spec.TCPProxyPort = 0
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp without a proxy: %v", err)
	}
	if _, err := client.CoreV1().Services(spec.Namespace).Get(ctx, "web-tcp-proxy", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the proxy outlived being turned off: %v", err)
	}
	if _, err := client.NetworkingV1().NetworkPolicies(spec.Namespace).Get(ctx, "web-tcp-proxy", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the proxy's policy outlived it: %v", err)
	}
}

func TestDeleteAppRemovesTheTCPProxy(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	spec.TCPProxyPort = 30043
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	if err := o.DeleteApp(ctx, spec.Ref); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if _, err := client.CoreV1().Services(spec.Namespace).Get(ctx, "web-tcp-proxy", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the proxy outlived its app: %v", err)
	}
}

// With isolation off nothing selects the pods but the proxy's policy, which
// would then shut out everything but the proxied port; it goes with the
// namespace's own, and comes back with it.
func TestTheTCPProxyPolicyFollowsIsolation(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	spec.TCPProxyPort = 30044
	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: spec.Owner, Name: spec.Namespace}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	if err := o.SetIsolation(ctx, orchestrator.Isolation{Mode: orchestrator.IsolationOff}); err != nil {
		t.Fatalf("SetIsolation off: %v", err)
	}
	if _, err := client.NetworkingV1().NetworkPolicies(spec.Namespace).Get(ctx, "web-tcp-proxy", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the proxy's policy stayed with isolation off: %v", err)
	}

	if err := o.SetIsolation(ctx, orchestrator.Isolation{}); err != nil {
		t.Fatalf("SetIsolation team: %v", err)
	}
	pol, err := client.NetworkingV1().NetworkPolicies(spec.Namespace).Get(ctx, "web-tcp-proxy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the proxy's policy did not come back with isolation: %v", err)
	}
	if got := pol.Spec.Ingress[0].Ports[0].Port.IntVal; got != spec.Port {
		t.Errorf("restored policy opens %d, want %d", got, spec.Port)
	}
}

func TestATCPProxyNeedsAPort(t *testing.T) {
	spec := testSpec()
	spec.Port, spec.TCPProxyPort = 0, 30045
	if err := spec.Validate(); err == nil {
		t.Error("a proxy to a workload with no port validated")
	}
	spec.Port, spec.TCPProxyPort = 8080, 70000
	if err := spec.Validate(); err == nil {
		t.Error("a proxy port past 65535 validated")
	}
}
