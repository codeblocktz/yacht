package k8s

import (
	"context"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Strict is what a new install runs on, so it is checked here by what a
// normal install needs to keep working rather than by the shape of the rules:
// each connection below is decided the way the cluster's policy controller
// decides it, from the policies the engine actually wrote, at both ends.
//
// What this cannot see is the cluster rewriting a destination before the
// policy is asked — a Service's address, or a node's own public one, becoming
// the pod behind it. Destinations are therefore written as the controller
// sees them: a pod, for anything reached by a Service or through the ingress;
// an address, for anything off the cluster's pod network.
func TestStrictIsolationLetsANewInstallWork(t *testing.T) {
	registry := netip.MustParsePrefix("10.20.0.5/32")
	c := newCluster(t, orchestrator.Isolation{Mode: orchestrator.IsolationStrict, EgressAllow: []netip.Prefix{registry}})

	app := c.pod("yacht-a-web", "10.42.0.10", map[string]string{"app": "web"}, nil)
	db := c.pod("yacht-a-db", "10.42.0.11", map[string]string{"app": "db"}, nil)
	otherProject := c.pod("yacht-a-api", "10.42.0.12", map[string]string{"app": "api"}, nil)
	otherTeam := c.pod("yacht-b-db", "10.42.0.20", map[string]string{"app": "db"}, nil)
	build := c.pod(BuildNamespace, "10.42.0.30", map[string]string{"job-name": "build-1"}, nil)
	dns := c.pod("kube-system", "10.42.0.2", map[string]string{"k8s-app": "kube-dns"}, nil)
	traefik := c.pod("kube-system", "10.42.0.3", map[string]string{"app.kubernetes.io/name": "traefik"},
		map[string]int32{"web": 8000, "websecure": 8443, "traefik": 9000, "metrics": 9100})
	svclb := c.pod("kube-system", "10.42.0.4", map[string]string{"svccontroller.k3s.cattle.io/svcname": "traefik"}, nil)
	metricsServer := c.pod("kube-system", "10.42.0.5", map[string]string{"k8s-app": "metrics-server"}, nil)
	platform := c.pod("yacht-system", "10.42.0.6", map[string]string{"app": "platform"}, nil)

	for _, tc := range []struct {
		what  string
		from  endpoint
		to    endpoint
		port  int32
		proto corev1.Protocol
		want  bool
	}{
		// DNS, as every name an app or a build looks up is.
		{"an app asks the cluster's DNS over UDP", app, dns, 53, corev1.ProtocolUDP, true},
		{"an app asks the cluster's DNS over TCP", app, dns, 53, corev1.ProtocolTCP, true},
		{"a build asks the cluster's DNS", build, dns, 53, corev1.ProtocolUDP, true},
		{"an app reaches more of kube-system than DNS", app, metricsServer, 10250, corev1.ProtocolTCP, false},

		// The team's own apps: its database in the project, and in another
		// of its projects only where that project lets it in, as under team.
		{"an app reaches its project's database", app, db, 5432, corev1.ProtocolTCP, true},
		{"an app reaches another project's app it does not refer to", app, otherProject, 8080, corev1.ProtocolTCP, false},
		{"an app reaches another team's database", app, otherTeam, 5432, corev1.ProtocolTCP, false},

		// The way in: the ingress controller and the platform reach apps, and
		// an app or a build reaches the cluster's own public addresses through
		// the ingress controller's entrypoints alone.
		{"the ingress controller reaches an app", traefik, app, 8080, corev1.ProtocolTCP, true},
		{"the platform reaches an app", platform, app, 8080, corev1.ProtocolTCP, true},
		{"a build reaches an app", build, app, 8080, corev1.ProtocolTCP, false},
		{"another team reaches an app", otherTeam, app, 8080, corev1.ProtocolTCP, false},
		{"an app calls another app's public URL over HTTPS", app, traefik, 8443, corev1.ProtocolTCP, true},
		{"an app calls another app's public URL over HTTP", app, traefik, 8000, corev1.ProtocolTCP, true},
		{"a build pushes to a registry the cluster serves", build, traefik, 8443, corev1.ProtocolTCP, true},
		{"a build reaches the cluster's address through K3s's load balancer", build, svclb, 443, corev1.ProtocolTCP, true},
		{"an app reaches the ingress controller's dashboard", app, traefik, 9000, corev1.ProtocolTCP, false},

		// The outside world: Git hosts, package mirrors, registries, APIs.
		{"a build clones from GitHub", build, ip("140.82.112.3"), 443, corev1.ProtocolTCP, true},
		{"a build clones over SSH", build, ip("140.82.112.3"), 22, corev1.ProtocolTCP, true},
		{"a build pulls from a package mirror", build, ip("104.16.0.35"), 443, corev1.ProtocolTCP, true},
		{"a build pushes to a public registry", build, ip("54.236.113.205"), 443, corev1.ProtocolTCP, true},
		{"a build pushes to the registry on the LAN it is allowed", build, ip("10.20.0.5"), 5000, corev1.ProtocolTCP, true},
		{"an app calls a public API", app, ip("142.250.180.14"), 443, corev1.ProtocolTCP, true},
		{"an app reaches a bucket on a node's public address", app, ip("203.0.113.50"), 3900, corev1.ProtocolTCP, true},

		// What strict is for: nothing private beyond those.
		{"an app reaches cloud metadata", app, ip("169.254.169.254"), 80, corev1.ProtocolTCP, false},
		{"a build reaches cloud metadata", build, ip("169.254.169.254"), 80, corev1.ProtocolTCP, false},
		{"an app reaches a Postgres on a node's private address", app, ip("10.0.0.5"), 5432, corev1.ProtocolTCP, false},
		{"a build reaches a Garage admin API on a private address", build, ip("10.0.0.5"), 3903, corev1.ProtocolTCP, false},
		{"an app reaches a kubelet", app, ip("10.0.0.5"), 10250, corev1.ProtocolTCP, false},
		{"an app reaches the LAN", app, ip("192.168.1.10"), 22, corev1.ProtocolTCP, false},
		{"an app reaches carrier-grade NAT", app, ip("100.64.0.1"), 443, corev1.ProtocolTCP, false},
		{"an app reaches a neighbour of the allowed registry", app, ip("10.20.0.6"), 5000, corev1.ProtocolTCP, false},
		{"a build reaches a team's database", build, db, 5432, corev1.ProtocolTCP, false},
		{"a build reaches the platform", build, platform, 8080, corev1.ProtocolTCP, false},
	} {
		if got := c.allows(tc.from, tc.to, tc.port, tc.proto); got != tc.want {
			t.Errorf("%s: allowed = %t, want %t", tc.what, got, tc.want)
		}
	}
}

// Team isolation, an unset mode, is what an install made before strict was
// the default keeps. It shuts out other teams and builds, and leaves open the
// way out that the upgrade note warns of.
func TestTeamIsolationLeavesTheWayOutOpen(t *testing.T) {
	c := newCluster(t, orchestrator.Isolation{})

	app := c.pod("yacht-a-web", "10.42.0.10", map[string]string{"app": "web"}, nil)
	otherTeam := c.pod("yacht-b-db", "10.42.0.20", map[string]string{"app": "db"}, nil)
	build := c.pod(BuildNamespace, "10.42.0.30", map[string]string{"job-name": "build-1"}, nil)

	for _, tc := range []struct {
		what string
		from endpoint
		to   endpoint
		port int32
		want bool
	}{
		{"an app reaches cloud metadata", app, ip("169.254.169.254"), 80, true},
		{"a build reaches cloud metadata", build, ip("169.254.169.254"), 80, true},
		{"an app reaches a Postgres on a node's private address", app, ip("10.0.0.5"), 5432, true},
		{"an app reaches another team's database", app, otherTeam, 5432, false},
		{"a build reaches an app", build, app, 8080, false},
	} {
		if got := c.allows(tc.from, tc.to, tc.port, corev1.ProtocolTCP); got != tc.want {
			t.Errorf("%s: allowed = %t, want %t", tc.what, got, tc.want)
		}
	}
}

// cluster is a fake cluster whose team namespaces and build namespace were
// made by the engine under one isolation, with the platform's beside them.
type cluster struct {
	t      *testing.T
	client *fake.Clientset
}

// endpoint is one end of a connection: a pod, by its namespace and labels,
// or an address off the pod network.
type endpoint struct {
	namespace string
	nsLabels  map[string]string
	labels    map[string]string
	ports     map[string]int32 // named container ports
	addr      netip.Addr
}

func ip(s string) endpoint { return endpoint{addr: netip.MustParseAddr(s)} }

func newCluster(t *testing.T, iso orchestrator.Isolation) *cluster {
	t.Helper()
	ctx := context.Background()
	o, client := testOrchestrator(t)
	o.WithIsolation(iso)
	for _, ns := range []orchestrator.NamespaceSpec{
		{Owner: "team-a", Name: "yacht-a-web", Project: "p1"},
		{Owner: "team-a", Name: "yacht-a-db", Project: "p1"},
		{Owner: "team-a", Name: "yacht-a-api", Project: "p2"},
		{Owner: "team-b", Name: "yacht-b-db", Project: "q1"},
	} {
		if err := o.EnsureNamespace(ctx, ns); err != nil {
			t.Fatalf("EnsureNamespace %s: %v", ns.Name, err)
		}
	}
	if err := o.ensureBuildNamespace(ctx); err != nil {
		t.Fatalf("ensureBuildNamespace: %v", err)
	}
	for _, name := range []string{"kube-system", "yacht-system"} {
		if _, err := client.CoreV1().Namespaces().Create(ctx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	return &cluster{t: t, client: client}
}

// pod is an endpoint in a namespace, labelled as the API server labels it.
func (c *cluster) pod(namespace, addr string, podLabels map[string]string, ports map[string]int32) endpoint {
	c.t.Helper()
	ns, err := c.client.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
	if err != nil {
		c.t.Fatalf("namespace %s: %v", namespace, err)
	}
	nsLabels := map[string]string{nameLabel: namespace}
	for k, v := range ns.Labels {
		nsLabels[k] = v
	}
	return endpoint{namespace: namespace, nsLabels: nsLabels, labels: podLabels, ports: ports,
		addr: netip.MustParseAddr(addr)}
}

func (c *cluster) policies(namespace string) []networkingv1.NetworkPolicy {
	c.t.Helper()
	if namespace == "" {
		return nil
	}
	list, err := c.client.NetworkingV1().NetworkPolicies(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		c.t.Fatalf("policies in %s: %v", namespace, err)
	}
	return list.Items
}

// allows is whether a connection is let out of its source and into its
// destination: a pod selected by no policy of a direction is open in it, and
// one selected by any is open only to what one of their rules allows.
func (c *cluster) allows(from, to endpoint, port int32, proto corev1.Protocol) bool {
	return c.side(from, to, port, proto, networkingv1.PolicyTypeEgress) &&
		c.side(to, from, port, proto, networkingv1.PolicyTypeIngress)
}

// side decides one direction at self: egress towards peer, or ingress from
// it. The port is always the destination's.
func (c *cluster) side(self, peer endpoint, port int32, proto corev1.Protocol, dir networkingv1.PolicyType) bool {
	if self.namespace == "" {
		return true // an address off the pod network has no policy
	}
	dst := peer
	if dir == networkingv1.PolicyTypeIngress {
		dst = self
	}
	selected := false
	for _, p := range c.policies(self.namespace) {
		if !hasType(p, dir) || !matches(&p.Spec.PodSelector, self.labels) {
			continue
		}
		selected = true
		if dir == networkingv1.PolicyTypeEgress {
			for _, r := range p.Spec.Egress {
				if peersMatch(r.To, p.Namespace, peer) && portsMatch(r.Ports, dst, port, proto) {
					return true
				}
			}
		} else {
			for _, r := range p.Spec.Ingress {
				if peersMatch(r.From, p.Namespace, peer) && portsMatch(r.Ports, dst, port, proto) {
					return true
				}
			}
		}
	}
	return !selected
}

func hasType(p networkingv1.NetworkPolicy, dir networkingv1.PolicyType) bool {
	for _, t := range p.Spec.PolicyTypes {
		if t == dir {
			return true
		}
	}
	return false
}

func matches(sel *metav1.LabelSelector, set map[string]string) bool {
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false
	}
	return s.Matches(labels.Set(set))
}

// peersMatch is whether any peer of a rule, written in namespace, is e. No
// peers at all is everything.
func peersMatch(peers []networkingv1.NetworkPolicyPeer, namespace string, e endpoint) bool {
	if len(peers) == 0 {
		return true
	}
	for _, peer := range peers {
		if peer.IPBlock != nil {
			if inBlock(peer.IPBlock, e.addr) {
				return true
			}
			continue
		}
		if e.namespace == "" {
			continue // selectors pick pods, never addresses
		}
		if peer.NamespaceSelector == nil {
			if e.namespace != namespace {
				continue
			}
		} else if !matches(peer.NamespaceSelector, e.nsLabels) {
			continue
		}
		if peer.PodSelector == nil || matches(peer.PodSelector, e.labels) {
			return true
		}
	}
	return false
}

func inBlock(b *networkingv1.IPBlock, addr netip.Addr) bool {
	cidr, err := netip.ParsePrefix(b.CIDR)
	if err != nil || !cidr.Contains(addr) {
		return false
	}
	for _, ex := range b.Except {
		if p, err := netip.ParsePrefix(ex); err == nil && p.Contains(addr) {
			return false
		}
	}
	return true
}

// portsMatch is whether any of a rule's ports is the destination's port. A
// named port is looked up among the destination pod's own.
func portsMatch(ports []networkingv1.NetworkPolicyPort, dst endpoint, port int32, proto corev1.Protocol) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		want := corev1.ProtocolTCP
		if p.Protocol != nil {
			want = *p.Protocol
		}
		if want != proto {
			continue
		}
		if p.Port == nil {
			return true
		}
		if p.Port.Type == intstr.String {
			if n, ok := dst.ports[p.Port.StrVal]; ok && n == port {
				return true
			}
			continue
		}
		if p.Port.IntVal == port {
			return true
		}
	}
	return false
}
