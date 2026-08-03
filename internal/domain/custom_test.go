package domain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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

// The failure a person can act on names what it found.
//
// This is the whole point of Probe replacing a bool. "That name does not resolve
// here yet" is true of a name that does not exist and of a name pointing at
// somebody else's platform, and only one of those is fixed by waiting.
func TestProbeReportsWhereTheNameActuallyPoints(t *testing.T) {
	res := fakeResolver{
		cname: map[string]string{"shop.wrong.test": "ghs.googlehosted.com."},
		addrs: map[string][]string{
			"shop.wrong.test":  {"203.0.113.9"},
			"edge.domain.test": {"198.51.100.1"},
		},
	}

	obs := Probe(context.Background(), res, "shop.wrong.test", "edge.domain.test")
	if obs.PointsHere {
		t.Fatal("a name pointing elsewhere was accepted")
	}
	if obs.CNAME != "ghs.googlehosted.com." {
		t.Errorf("CNAME = %q, want the record that is actually there", obs.CNAME)
	}
	if got := obs.Describe(); got != "points at ghs.googlehosted.com" {
		t.Errorf("Describe() = %q", got)
	}
}

// A resolver with no CNAME to report answers with the name it was asked about.
// Reading that as a CNAME would tell somebody their A record is a CNAME
// pointing at itself.
func TestProbeDoesNotReportAnEchoedNameAsACNAME(t *testing.T) {
	res := fakeResolver{
		cname: map[string]string{"apex.test": "apex.test."},
		addrs: map[string][]string{
			"apex.test":        {"203.0.113.9"},
			"edge.domain.test": {"198.51.100.1"},
		},
	}

	obs := Probe(context.Background(), res, "apex.test", "edge.domain.test")
	if obs.CNAME != "" {
		t.Errorf("CNAME = %q, want empty for a name with no CNAME", obs.CNAME)
	}
	if got := obs.Describe(); got != "resolves to 203.0.113.9" {
		t.Errorf("Describe() = %q, want the addresses", got)
	}
}

// A target that does not resolve is this install's misconfiguration, not the
// customer's. Recorded as an error so it does not mark every custom domain on
// the install as pointing somewhere wrong.
func TestProbeBlamesTheInstallWhenTheTargetIsBroken(t *testing.T) {
	res := fakeResolver{addrs: map[string][]string{"shop.customer.test": {"203.0.113.9"}}}

	obs := Probe(context.Background(), res, "shop.customer.test", "edge.missing.test")
	if obs.Err == nil {
		t.Fatal("a target that does not resolve was not reported as an error")
	}
	if got := Classify(obs, StateAwaitingDNS); got != StateAwaitingDNS {
		t.Errorf("state = %q, want the previous state kept", got)
	}
}

// Nothing ever un-verified a domain before this. One whose DNS was deleted
// stayed proven forever, and stayed in the Ingress with it.
func TestALiveDomainThatStopsResolvingDrifts(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-drift", "web", "ns-test-custom-drift")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.drift.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	working := fakeResolver{cname: map[string]string{"shop.drift.test": "edge.domain.test."}}
	if err := Verify(ctx, q, working, a.OwnerID, c.ID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := q.MarkDomainRouted(ctx, c.ID); err != nil {
		t.Fatalf("MarkDomainRouted: %v", err)
	}

	hosts, _ := RoutableHosts(ctx, q, a.ID)
	if !hasHost(hosts, "shop.drift.test") {
		t.Fatalf("the domain is not routed before drift — hosts were %v", hosts)
	}

	// The record is deleted at the provider. The platform's own target still
	// resolves — that is what makes this the customer's change rather than an
	// outage on this install, and the two must not classify the same way.
	deleted := fakeResolver{
		addrs: map[string][]string{"edge.domain.test": {"198.51.100.1"}},
	}
	row, err := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if err != nil {
		t.Fatalf("GetCustomDomain: %v", err)
	}
	state, err := Check(ctx, q, deleted, row, time.Now())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != StateDrifted {
		t.Fatalf("state = %q, want drifted", state)
	}

	hosts, _ = RoutableHosts(ctx, q, a.ID)
	if hasHost(hosts, "shop.drift.test") {
		t.Fatalf("a drifted domain is still routed — hosts were %v", hosts)
	}
}

// The failure mode this guards against is the loud one: a resolver outage on
// the install marking every live custom domain as broken at once, and every
// Ingress dropping its hosts a few seconds later.
//
// Nothing resolves here, including the platform's own target, which is what
// separates "our DNS is down" from "the customer changed their record".
func TestAResolverOutageDoesNotDriftEveryDomain(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-outage", "web", "ns-test-custom-outage")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.outage.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	working := fakeResolver{cname: map[string]string{"shop.outage.test": "edge.domain.test."}}
	if err := Verify(ctx, q, working, a.OwnerID, c.ID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := q.MarkDomainRouted(ctx, c.ID); err != nil {
		t.Fatalf("MarkDomainRouted: %v", err)
	}

	row, err := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if err != nil {
		t.Fatalf("GetCustomDomain: %v", err)
	}
	state, err := Check(ctx, q, fakeResolver{}, row, time.Now())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != StateRouted {
		t.Fatalf("state = %q, want the domain left alone during an outage", state)
	}

	hosts, _ := RoutableHosts(ctx, q, a.ID)
	if !hasHost(hosts, "shop.outage.test") {
		t.Fatalf("an outage withdrew a working domain — hosts were %v", hosts)
	}

	// The reason is still recorded, so the page can say why nothing is moving.
	row, _ = q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if row.LastError == "" {
		t.Error("the outage was not recorded anywhere")
	}
}

// verified is generated from state in the database. This asserts the two agree
// across every state, because the Ingress is built from one and the page is
// drawn from the other.
func TestTheRoutingGateAgreesWithTheState(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-gate", "web", "ns-test-custom-gate")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.gate.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	for _, state := range []State{
		StatePending, StateAwaitingDNS, StateMisdirected,
		StateVerified, StateRouted, StateDrifted,
	} {
		if _, err := q.RecordDomainCheck(ctx, dbgen.RecordDomainCheckParams{
			ID:          c.ID,
			State:       string(state),
			CheckedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
			NextCheckAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("record %q: %v", state, err)
		}

		row, err := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
		if err != nil {
			t.Fatalf("read %q: %v", state, err)
		}
		if row.Verified != state.Routable() {
			t.Errorf("state %q: verified = %v, Routable() = %v",
				state, row.Verified, state.Routable())
		}

		hosts, _ := RoutableHosts(ctx, q, a.ID)
		if hasHost(hosts, "shop.gate.test") != state.Routable() {
			t.Errorf("state %q: routed = %v, want %v",
				state, hasHost(hosts, "shop.gate.test"), state.Routable())
		}
	}
}

// The state column refuses a value nothing knows how to render. A typo in a
// migration or a query would otherwise reach the page as a blank status.
func TestAnUnknownStateIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-custom-badstate", "web", "ns-test-custom-badstate")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.badstate.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	if _, err := q.RecordDomainCheck(ctx, dbgen.RecordDomainCheckParams{
		ID:          c.ID,
		State:       "nearly-verified",
		CheckedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		NextCheckAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("an unknown state was accepted")
	}
}

func hasHost(hosts []string, want string) bool {
	for _, h := range hosts {
		if h == want {
			return true
		}
	}
	return false
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
