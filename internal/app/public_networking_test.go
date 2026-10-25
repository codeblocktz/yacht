package app

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/domain"
)

// publicApp presses Generate Domain for an app and lets the reconciler route
// it: what a test about something else needs from an app that is meant to be
// on the internet, now that none is until somebody asks.
func publicApp(t *testing.T, s *Service, ownerID, name string) App {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GenerateDomain(ctx, ownerID, name, 0); err != nil {
		t.Fatalf("GenerateDomain %s: %v", name, err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		t.Fatalf("Get %s: %v", name, err)
	}
	return a
}

// Railway's rule: an app is on the internet because somebody asked, not
// because it exists.
func TestCreateIssuesNoPublicDomain(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.none.test", WildcardTLS: true})
	id := owner(t, s, pool, "net-none")

	created, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Host != "" {
		t.Errorf("Host = %q, want none until Generate Domain", created.Host)
	}
	if hosts := orch.lastAppSpec().Hosts; len(hosts) != 0 {
		t.Errorf("spec.Hosts = %v, want none", hosts)
	}
	n, err := s.Networking(ctx, id, "web")
	if err != nil {
		t.Fatalf("Networking: %v", err)
	}
	if n.Public.Host != "" || n.Public.Unavailable != "" {
		t.Errorf("Public = %+v, want no host and Generate Domain on offer", n.Public)
	}
	if n.Public.Suggested != "web-production" || n.Public.Domain != "up.apps.none.test" {
		t.Errorf("Public suggests %q under %q, want web-production under up.apps.none.test",
			n.Public.Suggested, n.Public.Domain)
	}
}

// Generate Domain gives <app>-<environment>.up.<app domain>, routed and served
// from the wildcard, never from a certificate of its own.
func TestGenerateDomainIsRoutedUnderUp(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{
		AppDomain: "apps.gen.test", WildcardTLS: true, CertIssuer: "yacht-acme",
	})
	id := owner(t, s, pool, "net-generate")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	a := publicApp(t, s, id, "web")
	const want = "web-production.up.apps.gen.test"
	if a.Host != want || a.URLScheme() != "https" {
		t.Fatalf("Host = %q over %s, want %s over https", a.Host, a.URLScheme(), want)
	}
	spec := orch.lastAppSpec()
	if !slices.Equal(spec.Hosts, []string{want}) || !slices.Equal(spec.TLSHosts, []string{want}) {
		t.Errorf("spec routes %v with TLS for %v, want %s for both", spec.Hosts, spec.TLSHosts, want)
	}
	// A certificate per generated name would spend Let's Encrypt's weekly
	// limit for the whole domain at one per app.
	if len(spec.IssuedHosts) != 0 {
		t.Errorf("spec.IssuedHosts = %v, want none: the wildcard covers it", spec.IssuedHosts)
	}

	if _, err := s.GenerateDomain(ctx, id, "web", 0); err == nil {
		t.Error("a second Generate Domain replaced the first, want it refused")
	}
}

// Two teams with an app called web cannot both have web-production; the
// second is told nothing and gets a suffix.
func TestAGeneratedNameSomebodyHoldsGetsASuffix(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{AppDomain: "apps.taken.test"})
	first := owner(t, s, pool, "net-taken-a")
	second := owner(t, s, pool, "net-taken-b")
	for _, id := range []string{first, second} {
		if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
			Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	a, err := s.GenerateDomain(ctx, first, "web", 0)
	if err != nil {
		t.Fatalf("GenerateDomain: %v", err)
	}
	b, err := s.GenerateDomain(ctx, second, "web", 0)
	if err != nil {
		t.Fatalf("GenerateDomain for the second team: %v", err)
	}
	if a != "web-production.up.apps.taken.test" {
		t.Errorf("first = %q", a)
	}
	label, _, _ := strings.Cut(b, ".")
	if !strings.HasPrefix(label, "web-production-") || len(label) != len("web-production-")+4 {
		t.Errorf("second = %q, want web-production-<four characters>", b)
	}
}

// The edit form: a new name under up, validated, unique; a target port.
func TestSetPublicDomainChangesTheNameAndPort(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.edit.test"})
	id := owner(t, s, pool, "net-edit")
	for _, name := range []string{"web", "api"} {
		if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
			Name: name, Image: "nginx:alpine", Replicas: 1, Port: 8080,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		publicApp(t, s, id, name)
	}

	for _, bad := range []string{strings.Repeat("a", 64), "Not_A_Label", "-web", "a.b"} {
		if err := s.SetPublicDomain(ctx, id, "web", bad, 0); err == nil {
			t.Errorf("SetPublicDomain accepted %q", bad)
		}
	}
	if err := s.SetPublicDomain(ctx, id, "web", "api-production", 0); !errors.Is(err, domain.ErrHostTaken) {
		t.Errorf("taking another app's name = %v, want ErrHostTaken", err)
	}

	if err := s.SetPublicDomain(ctx, id, "web", strings.Repeat("s", 63), 5001); err != nil {
		t.Fatalf("SetPublicDomain: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("roll out the port: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	want := strings.Repeat("s", 63) + ".up.apps.edit.test"
	got := mustGet(t, s, id, "web")
	if got.Host != want || got.Port != 5001 {
		t.Errorf("web is at %s:%d, want %s:5001", got.Host, got.Port, want)
	}
	if spec := orch.lastAppSpec(); spec.Name == "web" && spec.Port != 5001 {
		t.Errorf("applied port %d, want 5001", spec.Port)
	}
}

// An app given <name>.<app domain> before names were generated keeps it —
// through a port change too — and only moves under up when its name changes.
func TestALegacyHostnameStaysUntilItIsRenamed(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{AppDomain: "apps.legacy.test", WildcardTLS: true})
	id := owner(t, s, pool, "net-legacy")
	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "dominion", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := domain.EnsureManaged(ctx, s.q, s.managedInput(a)); err != nil {
		t.Fatalf("issue the old hostname: %v", err)
	}

	if err := s.Redeploy(ctx, id, "dominion"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "dominion"); err != nil {
		t.Fatalf("worker redeploy: %v", err)
	}
	if got := mustGet(t, s, id, "dominion"); got.Host != "dominion.apps.legacy.test" || !got.TLS {
		t.Fatalf("after a redeploy Host = %q (TLS %t), want the old name kept", got.Host, got.TLS)
	}

	if err := s.SetPublicDomain(ctx, id, "dominion", "dominion", 3000); err != nil {
		t.Fatalf("SetPublicDomain with the name unchanged: %v", err)
	}
	if got := mustGet(t, s, id, "dominion"); got.Host != "dominion.apps.legacy.test" || got.Port != 3000 {
		t.Fatalf("a port change left it at %s:%d, want dominion.apps.legacy.test:3000", got.Host, got.Port)
	}

	if err := s.SetPublicDomain(ctx, id, "dominion", "dominion-production", 0); err != nil {
		t.Fatalf("SetPublicDomain: %v", err)
	}
	if got := mustGet(t, s, id, "dominion"); got.Host != "dominion-production.up.apps.legacy.test" {
		t.Fatalf("renamed, Host = %q, want it under up", got.Host)
	}
}

// Changing the app domain moves both kinds under it, each keeping its kind.
func TestTheAppDomainChangingMovesEachKindOfHostname(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{AppDomain: "apps.move-a.test"})
	id := owner(t, s, pool, "net-move")
	old, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "old", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := domain.EnsureManaged(ctx, s.q, s.managedInput(old)); err != nil {
		t.Fatalf("issue the old hostname: %v", err)
	}
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "new", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	publicApp(t, s, id, "new")

	s.opts.AppDomain = "apps.move-b.test"
	for _, name := range []string{"old", "new"} {
		if err := s.Redeploy(ctx, id, name); err != nil {
			t.Fatalf("Redeploy: %v", err)
		}
		if err := executeNamedOperation(s, ctx, id, name); err != nil {
			t.Fatalf("worker redeploy: %v", err)
		}
	}
	if got := mustGet(t, s, id, "old").Host; got != "old.apps.move-b.test" {
		t.Errorf("old = %q, want old.apps.move-b.test", got)
	}
	if got := mustGet(t, s, id, "new").Host; got != "new-production.up.apps.move-b.test" {
		t.Errorf("new = %q, want new-production.up.apps.move-b.test", got)
	}
}

func TestRemovePublicDomainReleasesTheName(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.remove.test"})
	id := owner(t, s, pool, "net-remove")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	publicApp(t, s, id, "web")

	if err := s.RemovePublicDomain(ctx, id, "web"); err != nil {
		t.Fatalf("RemovePublicDomain: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if got := mustGet(t, s, id, "web").Host; got != "" {
		t.Errorf("Host = %q, want none", got)
	}
	if hosts := orch.lastAppSpec().Hosts; len(hosts) != 0 {
		t.Errorf("spec.Hosts = %v, want none", hosts)
	}
	if _, err := s.GenerateDomain(ctx, id, "web", 0); err != nil {
		t.Errorf("generating again: %v", err)
	}
}

// An internal app speaks something an HTTP hostname cannot carry, and an
// install with no app domain has nothing to put one under.
func TestGenerateDomainIsRefusedWhereItCannotWork(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{AppDomain: "apps.refuse.test"})
	id := owner(t, s, pool, "net-refuse")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "worker", Image: "busybox:latest", Replicas: 1,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.GenerateDomain(ctx, id, "worker", 0); err == nil {
		t.Error("a portless app was given a domain with no port named")
	}
	if _, err := s.Update(ctx, id, "worker", UpdateInput{
		Image: "busybox:latest", Port: 5432, Internal: true,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := s.GenerateDomain(ctx, id, "worker", 0); err == nil {
		t.Error("an internal app was given a domain")
	}
	if n, _ := s.Networking(ctx, id, "worker"); n.Public.Unavailable == "" {
		t.Error("the page is not told why an internal app cannot have a domain")
	}

	s.opts.AppDomain = ""
	if n, _ := s.Networking(ctx, id, "worker"); !strings.Contains(n.Public.Unavailable, "app domain") {
		t.Errorf("Unavailable = %q, want it to name the missing app domain", n.Public.Unavailable)
	}
}

// Private networking is the Service's name in cluster DNS, on the port it
// listens on — the same answer PRIVATE_HOST and PRIVATE_PORT give.
func TestNetworkingShowsThePrivateAddress(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	id := owner(t, s, pool, "net-private")
	db, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "db"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	n, err := s.Networking(ctx, id, "db")
	if err != nil {
		t.Fatalf("Networking: %v", err)
	}
	if n.Private.Host != "db."+db.Namespace+".svc.cluster.local" || n.Private.Port != db.Port {
		t.Errorf("Private = %+v, want db.%s.svc.cluster.local:%d", n.Private, db.Namespace, db.Port)
	}
}

// A template's website is public from the start, as Railway's are; its
// database is not.
func TestATemplatesWebsiteIsGivenADomain(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t), AppDomain: "apps.template.test"})
	id := owner(t, s, pool, "net-template")
	if _, err := s.DeployTemplate(ctx, id, "postgres-app", "shop"); err != nil {
		t.Fatalf("DeployTemplate: %v", err)
	}
	if got := mustGet(t, s, id, "shop-web").Host; got != "shop-web-production.up.apps.template.test" {
		t.Errorf("the website is at %q, want shop-web-production.up.apps.template.test", got)
	}
	if got := mustGet(t, s, id, "shop-db").Host; got != "" {
		t.Errorf("the database is at %q, want nowhere public", got)
	}
}

// A TCP proxy's port is chosen once, from the install's range, and stays
// the app's; it reaches the cluster on the next apply and leaves on the one
// after it is turned off.
func TestTCPProxyPortsAreClaimedOnceAndApplied(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{
		TCPProxyHost: "proxy.apps.test", TCPProxyPortFirst: 30010, TCPProxyPortLast: 30011,
	})
	id := owner(t, s, pool, "net-proxy")
	// Leftovers from an earlier run would hold the two ports.
	if _, err := pool.Exec(ctx, `DELETE FROM tcp_proxies WHERE port BETWEEN 30010 AND 30011`); err != nil {
		t.Fatalf("clear proxies: %v", err)
	}
	for _, name := range []string{"one", "two", "three"} {
		if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
			Name: name, Image: "postgres:17", Replicas: 1, Port: 5432,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	one, err := s.EnableTCPProxy(ctx, id, "one")
	if err != nil {
		t.Fatalf("EnableTCPProxy: %v", err)
	}
	if one.Port < 30010 || one.Port > 30011 || one.Address() != "proxy.apps.test:"+itoa(one.Port) || one.Target != 5432 {
		t.Fatalf("proxy = %+v, want a port in 30010-30011 reaching 5432", one)
	}
	again, err := s.EnableTCPProxy(ctx, id, "one")
	if err != nil || again.Port != one.Port {
		t.Fatalf("enabling again gave %d, %v; want the same port %d", again.Port, err, one.Port)
	}
	two, err := s.EnableTCPProxy(ctx, id, "two")
	if err != nil || two.Port == one.Port {
		t.Fatalf("second proxy = %+v, %v; want the other port", two, err)
	}
	if _, err := s.EnableTCPProxy(ctx, id, "three"); !errors.Is(err, ErrNoTCPProxyPort) {
		t.Fatalf("a third proxy in a range of two = %v, want ErrNoTCPProxyPort", err)
	}

	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if spec := orch.lastAppSpecFor(t, "two"); spec.TCPProxyPort != two.Port {
		t.Errorf("applied proxy port %d, want %d", spec.TCPProxyPort, two.Port)
	}
	n, err := s.Networking(ctx, id, "one")
	if err != nil || n.TCPProxy.Port != one.Port || !n.TCPProxy.Available {
		t.Errorf("Networking.TCPProxy = %+v, %v", n.TCPProxy, err)
	}

	if err := s.DisableTCPProxy(ctx, id, "two"); err != nil {
		t.Fatalf("DisableTCPProxy: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if spec := orch.lastAppSpecFor(t, "two"); spec.TCPProxyPort != 0 {
		t.Errorf("after turning it off the proxy port is %d, want none", spec.TCPProxyPort)
	}
	if _, err := s.EnableTCPProxy(ctx, id, "three"); err != nil {
		t.Errorf("the freed port was not offered again: %v", err)
	}
}

func TestTCPProxiesAreOffWithoutAHost(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "net-proxy-off")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "db", Image: "postgres:17", Replicas: 1, Port: 5432,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.EnableTCPProxy(ctx, id, "db"); err == nil {
		t.Error("a proxy was enabled on an install with nowhere to reach it")
	}
	if n, _ := s.Networking(ctx, id, "db"); n.TCPProxy.Available {
		t.Error("Networking offers TCP proxies the install does not have")
	}
}

func itoa(p int32) string { return strconv.Itoa(int(p)) }
