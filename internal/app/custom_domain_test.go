package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/domain"
)

// No app may bring a name the install itself answers on. Each of these
// resolves to the platform because it is the platform, so a claim would
// verify on its first check and be given an Ingress and a certificate in the
// claiming team's namespace. The operator's reserved list is empty by
// default, so the install has to know its own names without being told.
func TestTheInstallsOwnNamesAreNotCustomDomains(t *testing.T) {
	ctx := context.Background()
	s, _, id := regionService(t, Options{
		AppDomain:     "apps.test",
		PlatformZones: []string{"https://yacht.example.test", "", "https://s3.example.test"},
		PlatformNames: []string{"tcp.example.test", "http://10.0.0.9:9428"},
	})
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO platform_dns (id, cname_target) VALUES (1, 'edge.example.test')
		ON CONFLICT (id) DO UPDATE SET cname_target = excluded.cname_target`); err != nil {
		t.Fatalf("set the install's target: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO platform_registry (id, host, username, password_sealed)
		VALUES (1, 'registry.example.test:5000', 'bot', '\x00')
		ON CONFLICT (id) DO UPDATE SET host = excluded.host`); err != nil {
		t.Fatalf("set the registry: %v", err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"platform_dns", "platform_registry"} {
			if _, err := s.pool.Exec(context.Background(), `DELETE FROM `+table); err != nil {
				t.Errorf("clear %s: %v", table, err)
			}
		}
	})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080, Region: "dar",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, host := range []string{
		"yacht.example.test",       // the dashboard
		"login.yacht.example.test", // a sibling of it, which can set its cookies
		"s3.example.test",          // the buckets' endpoint
		"bucket.s3.example.test",   // a bucket on it
		"tcp.example.test",         // the TCP proxy
		"edge.example.test",        // the CNAME target
		"edge-fsn.example.test",    // a region's edge
		"registry.example.test",    // the registry, without its port
		"web-production.up.apps.test",
	} {
		if err := s.AddDomain(ctx, id, "web", host); !errors.Is(err, domain.ErrHostReserved) {
			t.Errorf("AddDomain(%q) = %v, want ErrHostReserved", host, err)
		}
	}

	// A name, not a zone: nothing under the registry's name is the install's.
	if err := s.AddDomain(ctx, id, "web", "shop.registry.example.test"); err != nil {
		t.Errorf("a name under the registry's was refused: %v", err)
	}
	if err := s.AddDomain(ctx, id, "web", "shop.customer.test"); err != nil {
		t.Errorf("an ordinary custom domain was refused: %v", err)
	}
}

// pointsHere resolves every name to the install's target, as the install's
// own names do.
type pointsHere struct{ target string }

func (p pointsHere) LookupCNAME(context.Context, string) (string, error) { return p.target + ".", nil }
func (p pointsHere) LookupHost(context.Context, string) ([]string, error) {
	return []string{"203.0.113.1"}, nil
}

// A routed claim on one of the install's own names, made before it was
// reserved, is withdrawn by a manual Verify as by the checker: Verify says it
// is not verified, and the app's config version has moved, so the reconciler
// takes the name off the Ingress, and whoever watches routes is told.
func TestVerifyingAReservedClaimWithdrawsIt(t *testing.T) {
	ctx := context.Background()
	var (
		mu      sync.Mutex
		changes []RouteChange
	)
	s, _, id := regionService(t, Options{
		AppDomain: "apps.test", PlatformNames: []string{"tcp.verify-withdraw.test"},
		Resolver: pointsHere{target: "edge.verify-withdraw.test"},
		Routes: RouteObserverFunc(func(_ context.Context, c RouteChange) {
			mu.Lock()
			changes = append(changes, c)
			mu.Unlock()
		}),
	})
	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080, Region: "dar",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var domainID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO domains (owner_id, app_id, host, state, managed, verify_target)
		VALUES ($1, $2, 'tcp.verify-withdraw.test', 'routed', false, 'edge.verify-withdraw.test')
		RETURNING id`, id, a.ID).Scan(&domainID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	before, _ := s.Get(ctx, id, "web")

	err = s.VerifyDomain(ctx, id, "web", domainID)
	if !errors.Is(err, domain.ErrNotVerified) {
		t.Fatalf("VerifyDomain = %v, want ErrNotVerified", err)
	}
	after, _ := s.Get(ctx, id, "web")
	if after.ConfigVersion <= before.ConfigVersion {
		t.Errorf("config version %d → %d; want it moved so the reconciler withdraws the name",
			before.ConfigVersion, after.ConfigVersion)
	}
	if hosts, _ := domain.RoutableHosts(ctx, s.q, a.ID); slices.Contains(hosts, "tcp.verify-withdraw.test") {
		t.Errorf("the reserved name is still routable: %v", hosts)
	}
	s.routesWG.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(changes) == 0 {
		t.Error("nobody watching routes was told of the withdrawal")
	}
}

// What cannot be read is refused rather than guessed at: a list missing the
// registry would let somebody claim it.
func TestCustomDomainReservationsFailClosed(t *testing.T) {
	s, _, _ := regionService(t, Options{AppDomain: "apps.test"})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CustomDomainReservations(cancelled); err == nil {
		t.Error("CustomDomainReservations with no database to ask answered anyway")
	}

	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO platform_dns (id, cname_target) VALUES (1, 'not a host')
		ON CONFLICT (id) DO UPDATE SET cname_target = excluded.cname_target`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), `DELETE FROM platform_dns`) })
	if _, err := s.CustomDomainReservations(ctx); err == nil {
		t.Error("a stored address that is not a host was dropped from the reservations, not refused")
	}
}

// A generated label may not land on one of the install's own names.
func TestAGeneratedNameCannotBeTheInstallsOwn(t *testing.T) {
	ctx := context.Background()
	s, _, id := regionService(t, Options{
		AppDomain: "apps.test", PlatformNames: []string{"tcp.up.apps.test"},
	})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080, Region: "dar",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	publicApp(t, s, id, "web")
	if err := s.SetPublicDomain(ctx, id, "web", "tcp", 8080); !errors.Is(err, domain.ErrHostReserved) {
		t.Fatalf("SetPublicDomain(tcp) = %v, want ErrHostReserved", err)
	}
	if err := s.SetPublicDomain(ctx, id, "web", "shop", 8080); err != nil {
		t.Fatalf("SetPublicDomain(shop): %v", err)
	}
}
