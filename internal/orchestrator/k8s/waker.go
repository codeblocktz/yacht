package k8s

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	discoveryv1ac "k8s.io/client-go/applyconfigurations/discovery/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The waker needs both: a count to know when an app is idle, and an address
// to hand a held request to once it is not.
var (
	_ orchestrator.RequestCounter   = (*Orchestrator)(nil)
	_ orchestrator.ServiceAddresser = (*Orchestrator)(nil)
)

// The waker's route: a Service with no selector in the app's own namespace,
// whose one endpoint is the engine's waker, and which the app's Ingress points
// at while the app sleeps.
//
// A Service rather than the Ingress naming the waker some other way, because
// an Ingress backend is a Service in the Ingress's own namespace and nothing
// else. The endpoint is written by hand as an EndpointSlice, which is what the
// ingress controllers this runs behind read; a Service of type ExternalName
// would have been one object instead of two, and Traefik refuses to route to
// those unless it was installed to allow it.

// wakerServiceName is the waker's Service in an app's namespace. Every app
// has a namespace of its own, so the suffix cannot meet another app's name.
func wakerServiceName(name string) string { return name + "-waker" }

// endpointSliceManagedBy tells the EndpointSlice controller the slice is not
// its own, so it neither adopts nor deletes it.
const endpointSliceManagedBy = "yacht.codeblock.tz/waker"

// applyWakerRoute writes the waker's Service and its endpoint.
func (o *Orchestrator) applyWakerRoute(ctx context.Context, spec orchestrator.AppSpec) error {
	name := wakerServiceName(spec.Name)
	labels := orchestrator.ObjectLabels(spec.Ref)

	svc := corev1ac.Service(name, spec.Namespace).
		WithLabels(labels).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			WithPorts(corev1ac.ServicePort().
				WithName("http").
				WithPort(servicePort).
				WithProtocol(corev1.ProtocolTCP).
				WithTargetPort(intstr.FromInt32(spec.Waker.Port))))
	if _, err := o.client.CoreV1().Services(spec.Namespace).
		Apply(ctx, svc, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply waker service %s: %w", spec.Ref, err)
	}

	addrType := discoveryv1.AddressTypeIPv4
	if ip, err := netip.ParseAddr(spec.Waker.IP); err == nil && ip.Is6() && !ip.Is4In6() {
		addrType = discoveryv1.AddressTypeIPv6
	}
	sliceLabels := map[string]string{
		discoveryv1.LabelServiceName: name,
		discoveryv1.LabelManagedBy:   endpointSliceManagedBy,
	}
	for k, v := range labels {
		sliceLabels[k] = v
	}
	slice := discoveryv1ac.EndpointSlice(name, spec.Namespace).
		WithLabels(sliceLabels).
		WithAddressType(addrType).
		WithEndpoints(discoveryv1ac.Endpoint().
			WithAddresses(spec.Waker.IP).
			WithConditions(discoveryv1ac.EndpointConditions().WithReady(true))).
		WithPorts(discoveryv1ac.EndpointPort().
			WithName("http").
			WithPort(spec.Waker.Port).
			WithProtocol(corev1.ProtocolTCP))
	if _, err := o.client.DiscoveryV1().EndpointSlices(spec.Namespace).
		Apply(ctx, slice, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply waker endpoint %s: %w", spec.Ref, err)
	}
	return nil
}

// deleteWakerRoute removes the waker's Service and endpoint once the app is
// routed to itself again, tolerating their absence — which is every apply of
// an app that has never slept.
func (o *Orchestrator) deleteWakerRoute(ctx context.Context, ref orchestrator.Ref) error {
	name := wakerServiceName(ref.Name)
	err := o.client.DiscoveryV1().EndpointSlices(ref.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete waker endpoint %s: %w", ref, err)
	}
	err = o.client.CoreV1().Services(ref.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete waker service %s: %w", ref, err)
	}
	return nil
}

// ingressBackend is the Service an app's hostnames route to: its own, or the
// waker's while it sleeps.
func ingressBackend(spec orchestrator.AppSpec) string {
	if spec.Waker != nil {
		return wakerServiceName(spec.Name)
	}
	return spec.Name
}

// ServiceAddress is the app's own Service, as an address the waker can hand
// a held request to.
func (o *Orchestrator) ServiceAddress(ctx context.Context, ref orchestrator.Ref) (string, error) {
	svc, err := o.client.CoreV1().Services(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", orchestrator.ErrNotFound
		}
		return "", fmt.Errorf("k8s: read service %s: %w", ref, err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone || len(svc.Spec.Ports) == 0 {
		return "", fmt.Errorf("k8s: service %s has no address", ref)
	}
	return net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(svc.Spec.Ports[0].Port))), nil
}
