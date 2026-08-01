package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// fakeResolver answers from a map, so verification is tested without a network
// and without waiting on a real lookup.
type fakeResolver struct {
	cname map[string]string
	addrs map[string][]string
}

func (f fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if c, ok := f.cname[host]; ok {
		return c, nil
	}
	return "", errors.New("no cname")
}

func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if a, ok := f.addrs[host]; ok {
		return a, nil
	}
	return nil, errors.New("no host")
}

// A claim routes nothing until it is proven.
//
// This is the whole reason verification exists: the host column is globally
// unique, so without it the first team to type a name holds it against every
// other team on the install, and would be serving traffic for a domain it does
// not control.
func TestAnUnverifiedDomainIsNotRouted(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-unver", "web", "ns-test-custom-unver")
	q := dbgen.New(pool)

	if _, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.domain.test", TLS: true,
	}); err != nil {
		t.Fatalf("EnsureManaged: %v", err)
	}
	if _, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.customer.test", "edge.domain.test", "apps.domain.test", nil); err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	hosts, err := RoutableHosts(ctx, q, a.ID)
	if err != nil {
		t.Fatalf("RoutableHosts: %v", err)
	}
	for _, h := range hosts {
		if h == "shop.customer.test" {
			t.Fatal("an unverified claim is being routed")
		}
	}
	if len(hosts) != 1 || hosts[0] != "web.apps.domain.test" {
		t.Fatalf("hosts = %v, want only the platform host", hosts)
	}
}

// And once proven it is routed.
func TestAVerifiedDomainIsRouted(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-ver", "web", "ns-test-custom-ver")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.verified.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	res := fakeResolver{cname: map[string]string{"shop.verified.test": "edge.domain.test."}}
	if err := Verify(ctx, q, res, a.OwnerID, c.ID); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	hosts, err := RoutableHosts(ctx, q, a.ID)
	if err != nil {
		t.Fatalf("RoutableHosts: %v", err)
	}
	var found bool
	for _, h := range hosts {
		if h == "shop.verified.test" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a verified domain is not routed — hosts were %v", hosts)
	}
}

// A name pointed somewhere else is refused. Otherwise verification is a button
// that says yes.
func TestADomainPointingElsewhereIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-wrong", "web", "ns-test-custom-wrong")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.wrong.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	res := fakeResolver{
		cname: map[string]string{"shop.wrong.test": "somewhere.else.test."},
		addrs: map[string][]string{
			"shop.wrong.test":  {"203.0.113.9"},
			"edge.domain.test": {"198.51.100.1"},
		},
	}
	if err := Verify(ctx, q, res, a.OwnerID, c.ID); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("Verify = %v, want ErrNotVerified", err)
	}
}

// An apex cannot carry a CNAME, so matching addresses has to count. Refusing
// a correctly flattened apex would be telling somebody their working setup is
// broken.
func TestAnApexVerifiesByAddress(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-apex", "web", "ns-test-custom-apex")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"apex.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	res := fakeResolver{
		addrs: map[string][]string{
			"apex.test":        {"198.51.100.1"},
			"edge.domain.test": {"198.51.100.1"},
		},
	}
	if err := Verify(ctx, q, res, a.OwnerID, c.ID); err != nil {
		t.Fatalf("a flattened apex was refused: %v", err)
	}
}

// Two apps cannot hold one hostname, and the refusal does not say who has it —
// which team holds a name is not the asking team's business.
func TestAHostnameCannotBeClaimedTwice(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	first := seedApp(t, pool, "test-custom-a", "web", "ns-test-custom-a")
	second := seedApp(t, pool, "test-custom-b", "web", "ns-test-custom-b")
	q := dbgen.New(pool)

	if _, err := AddCustom(ctx, q, first.OwnerID, first.ID,
		"contested.test", "edge.domain.test", "apps.domain.test", nil); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := AddCustom(ctx, q, second.OwnerID, second.ID,
		"contested.test", "edge.domain.test", "apps.domain.test", nil)
	if !errors.Is(err, ErrHostTaken) {
		t.Fatalf("second claim = %v, want ErrHostTaken", err)
	}
	if err != nil && (contains(err.Error(), "test-custom-a") || contains(err.Error(), first.OwnerID)) {
		t.Errorf("the refusal names who holds the domain: %v", err)
	}
}

// The platform's own domain is not somebody's to bring.
func TestThePlatformDomainCannotBeClaimed(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-plat", "web", "ns-test-custom-plat")
	q := dbgen.New(pool)

	_, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"free.apps.domain.test", "edge.domain.test", "apps.domain.test", nil)
	if !errors.Is(err, ErrHostReserved) {
		t.Fatalf("claiming a name under the platform domain = %v, want ErrHostReserved", err)
	}
}

// People paste a URL, not a hostname.
func TestASchemeIsStrippedRatherThanRefused(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-scheme", "web", "ns-test-custom-scheme")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"https://Pasted.Example.Test/", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}
	if c.Host != "pasted.example.test" {
		t.Fatalf("host = %q, want the scheme and trailing slash removed and lowercased", c.Host)
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
