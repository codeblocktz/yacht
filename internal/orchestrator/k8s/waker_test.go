package k8s

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	restclient "k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func sleepingSpec() orchestrator.AppSpec {
	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	spec.Replicas = 0
	spec.Waker = &orchestrator.WakerEndpoint{IP: "10.0.0.5", Port: 8090}
	return spec
}

func TestASleepingAppIsRoutedToTheWaker(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := sleepingSpec()
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend.Name != "web-waker" || backend.Port.Number != servicePort {
		t.Fatalf("a sleeping app routes to %s:%d; want web-waker:%d", backend.Name, backend.Port.Number, servicePort)
	}

	svc, err := client.CoreV1().Services(spec.Namespace).Get(ctx, "web-waker", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get waker service: %v", err)
	}
	if len(svc.Spec.Selector) != 0 || svc.Spec.Ports[0].TargetPort.IntVal != 8090 {
		t.Fatalf("waker service = %+v; want no selector, targeting 8090", svc.Spec)
	}
	slice, err := client.DiscoveryV1().EndpointSlices(spec.Namespace).Get(ctx, "web-waker", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get waker endpoint: %v", err)
	}
	if slice.Labels[discoveryv1.LabelServiceName] != "web-waker" ||
		slice.AddressType != discoveryv1.AddressTypeIPv4 ||
		slice.Endpoints[0].Addresses[0] != "10.0.0.5" || *slice.Ports[0].Port != 8090 ||
		*slice.Ports[0].Name != svc.Spec.Ports[0].Name {
		t.Fatalf("waker endpoint = %+v; want 10.0.0.5:8090 for web-waker, port names matching", slice)
	}
	if slice.Labels[discoveryv1.LabelManagedBy] == "endpointslice-controller.k8s.io" {
		t.Fatalf("the waker's endpoint is labelled as the controller's, which would delete it")
	}

	dep, err := client.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if *dep.Spec.Replicas != 0 {
		t.Fatalf("a sleeping app runs %d replicas", *dep.Spec.Replicas)
	}
	// Its own Service stays: other apps reach it by name, and it is where the
	// route comes back to.
	if _, err := client.CoreV1().Services(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("a sleeping app lost its own service: %v", err)
	}
}

func TestTheRouteMovesBeforeThePodsGoAway(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp awake: %v", err)
	}
	client.ClearActions()

	if err := o.ApplyApp(ctx, sleepingSpec()); err != nil {
		t.Fatalf("ApplyApp asleep: %v", err)
	}
	ingressAt, deploymentAt := -1, -1
	for i, a := range client.Actions() {
		if a.GetVerb() != "patch" {
			continue
		}
		switch a.GetResource().Resource {
		case "ingresses":
			if ingressAt < 0 {
				ingressAt = i
			}
		case "deployments":
			if deploymentAt < 0 {
				deploymentAt = i
			}
		}
	}
	if ingressAt < 0 || deploymentAt < 0 || ingressAt > deploymentAt {
		t.Fatalf("ingress applied at %d, deployment at %d; the route must move first", ingressAt, deploymentAt)
	}
}

func TestWakingRemovesTheWakersRoute(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := sleepingSpec()
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp asleep: %v", err)
	}
	spec.Waker, spec.Replicas = nil, 2
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp awake: %v", err)
	}
	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if got := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name; got != "web" {
		t.Fatalf("an awake app routes to %s, want its own service", got)
	}
	if _, err := client.CoreV1().Services(spec.Namespace).Get(ctx, "web-waker", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("waker service still there: %v", err)
	}
	if _, err := client.DiscoveryV1().EndpointSlices(spec.Namespace).Get(ctx, "web-waker", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("waker endpoint still there: %v", err)
	}
}

func TestAWakerRouteNeedsAHostAndAnAddress(t *testing.T) {
	spec := sleepingSpec()
	spec.Hosts = nil
	if err := spec.Validate(); err == nil {
		t.Fatalf("a waker route with no hostname validated")
	}
	for _, ip := range []string{"waker.local", "127.0.0.1", "0.0.0.0", ""} {
		spec := sleepingSpec()
		spec.Waker.IP = ip
		if err := spec.Validate(); err == nil {
			t.Errorf("waker address %q validated; the cluster cannot reach it", ip)
		}
	}
	spec = sleepingSpec()
	spec.Waker.IP = "fd00::5"
	if err := spec.Validate(); err != nil {
		t.Errorf("an IPv6 waker: %v", err)
	}
}

func TestServiceAddressIsTheAppsClusterIP(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	if _, err := client.CoreV1().Services("yacht-demo").Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "yacht-demo"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.43.12.7", Ports: []corev1.ServicePort{{Port: 80}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	got, err := o.ServiceAddress(ctx, testSpec().Ref)
	if err != nil || got != "10.43.12.7:80" {
		t.Fatalf("ServiceAddress = %q, %v; want 10.43.12.7:80", got, err)
	}
	ref := testSpec().Ref
	ref.Name = "gone"
	if _, err := o.ServiceAddress(ctx, ref); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Fatalf("ServiceAddress of a missing service = %v, want ErrNotFound", err)
	}
}

// metricsBody is a response the fake clientset's proxy hands back.
type metricsBody string

func (m metricsBody) DoRaw(context.Context) ([]byte, error) { return []byte(m), nil }
func (m metricsBody) Stream(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(m))), nil
}

const traefikMetrics = `# HELP traefik_service_requests_total How many HTTP requests processed on a service.
# TYPE traefik_service_requests_total counter
traefik_service_requests_total{code="200",method="GET",protocol="http",service="yacht-demo-web-80@kubernetes"} 40
traefik_service_requests_total{code="404",method="GET",protocol="http",service="yacht-demo-web-80@kubernetes"} 2
traefik_service_requests_total{code="200",method="GET",protocol="http",service="yacht-demo-web-waker-80@kubernetes"} 9
traefik_service_requests_total{code="200",method="GET",protocol="http",service="yacht-other-api-80@kubernetes"} 1e+06
traefik_entrypoint_requests_total{code="200",entrypoint="web",method="GET",protocol="http"} 1000
`

func traefikPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kube-system",
			Labels: map[string]string{"app.kubernetes.io/name": "traefik"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "traefik", Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9100}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestRequestCountsAreTraefiksPerAppCountersSummed(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	for _, name := range []string{"traefik-a", "traefik-b"} {
		if _, err := client.CoreV1().Pods("kube-system").Create(ctx, traefikPod(name), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod: %v", err)
		}
	}
	var ports []string
	client.PrependProxyReactor("pods", func(a k8stesting.Action) (bool, restclient.ResponseWrapper, error) {
		p := a.(k8stesting.ProxyGetAction)
		ports = append(ports, p.GetPort())
		return true, metricsBody(traefikMetrics), nil
	})

	web := testSpec().Ref
	quiet := orchestrator.Ref{Owner: "owner-1", Namespace: "yacht-quiet", Name: "site"}
	got, err := o.RequestCounts(ctx, []orchestrator.Ref{web, quiet})
	if err != nil {
		t.Fatalf("RequestCounts: %v", err)
	}
	// 42 from each of two controller pods; the waker's own traffic and other
	// apps' are not this app's.
	if got[web] != 84 || got[quiet] != 0 || len(got) != 2 {
		t.Fatalf("counts = %v; want web 84 and site 0", got)
	}
	if len(ports) != 2 || ports[0] != "9100" {
		t.Fatalf("metrics read from ports %v; want 9100 on each pod", ports)
	}
}

func TestRequestCountsSayWhenTheyCannotBeRead(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	if _, err := o.RequestCounts(ctx, []orchestrator.Ref{testSpec().Ref}); !errors.Is(err, orchestrator.ErrNotSupported) {
		t.Fatalf("with no Traefik: %v, want ErrNotSupported", err)
	}
	if _, err := client.CoreV1().Pods("kube-system").Create(ctx, traefikPod("traefik-a"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	client.PrependProxyReactor("pods", func(k8stesting.Action) (bool, restclient.ResponseWrapper, error) {
		return true, metricsBody("# nothing here\ngo_goroutines 12\n"), nil
	})
	if _, err := o.RequestCounts(ctx, []orchestrator.Ref{testSpec().Ref}); !errors.Is(err, orchestrator.ErrNotSupported) {
		t.Fatalf("with metrics off: %v, want ErrNotSupported rather than a count of zero", err)
	}
}

func TestAPodNoNodeHasRoomForSaysSo(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "yacht-demo"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/1 nodes are available: 1 Insufficient memory.",
		}}},
	}
	if _, err := client.CoreV1().Pods("yacht-demo").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pods, err := o.Pods(ctx, orchestrator.PodListOptions{Namespace: "yacht-demo"})
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if pods[0].Reason != orchestrator.ReasonUnschedulable || !strings.Contains(pods[0].Message, "Insufficient memory") {
		t.Fatalf("pod = %+v; want unschedulable, with the scheduler's reason", pods[0])
	}
}
