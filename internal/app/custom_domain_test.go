package app

import (
	"context"
	"errors"
	"testing"

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
