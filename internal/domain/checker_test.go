package domain

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// fakeRouter records what the checker asked to be applied, and can refuse.
type fakeRouter struct {
	mu      sync.Mutex
	applied []uuid.UUID
	owners  []string
	fail    bool
}

func (f *fakeRouter) ApplyRouting(_ context.Context, ownerID string, appID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return context.DeadlineExceeded
	}
	f.applied = append(f.applied, appID)
	f.owners = append(f.owners, ownerID)
	return nil
}

func (f *fakeRouter) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}

// checkerFor builds a checker whose clock the test controls.
func checkerFor(t *testing.T, res Resolver, router Router, at time.Time) *Checker {
	t.Helper()
	c := NewChecker(testPool(t), res, router, nil)
	c.now = func() time.Time { return at }
	return c
}

// The behaviour the whole phase exists for: a claim proves itself, with nobody
// pressing anything.
func TestAClaimProvesItselfWithoutBeingAsked(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-check-auto", "web", "ns-test-check-auto")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.auto.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	router := &fakeRouter{}
	res := fakeResolver{cname: map[string]string{"shop.auto.test": "edge.domain.test."}}
	checker := checkerFor(t, res, router, time.Now())

	if err := checker.Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	row, err := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if err != nil {
		t.Fatalf("GetCustomDomain: %v", err)
	}
	if State(row.State) != StateVerified {
		t.Fatalf("state = %q, want verified until workload convergence", row.State)
	}
	if router.calls() != 1 {
		t.Fatalf("routing applied %d times, want once", router.calls())
	}
	if router.owners[0] != a.OwnerID {
		t.Errorf("applied for owner %q, want %q", router.owners[0], a.OwnerID)
	}
	if !hasHost(mustHosts(t, ctx, q, a.ID), "shop.auto.test") {
		t.Error("a proven domain is not routed")
	}
}

// A claim with no record yet settles into awaiting_dns and says so, rather than
// sitting in pending forever waiting for a click.
func TestAClaimWithNoRecordSettlesIntoWaiting(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-check-wait", "web", "ns-test-check-wait")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.wait.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	router := &fakeRouter{}
	res := fakeResolver{addrs: map[string][]string{"edge.domain.test": {"198.51.100.1"}}}
	if err := checkerFor(t, res, router, time.Now()).Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	row, _ := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if State(row.State) != StateAwaitingDNS {
		t.Fatalf("state = %q, want awaiting_dns", row.State)
	}
	if row.LastCheckedAt.Time.IsZero() {
		t.Error("nothing recorded when the check ran")
	}
	if router.calls() != 0 {
		t.Errorf("routing was applied for a domain that does not resolve")
	}
}

// A domain pointing elsewhere records where, which is the sentence that ends
// the support conversation.
func TestAMisdirectedDomainRecordsWhereItPoints(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-check-mis", "web", "ns-test-check-mis")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.mis.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	res := fakeResolver{
		cname: map[string]string{"shop.mis.test": "ghs.googlehosted.com."},
		addrs: map[string][]string{
			"shop.mis.test":    {"203.0.113.9"},
			"edge.domain.test": {"198.51.100.1"},
		},
	}
	if err := checkerFor(t, res, &fakeRouter{}, time.Now()).Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	row, _ := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if State(row.State) != StateMisdirected {
		t.Fatalf("state = %q, want misdirected", row.State)
	}
	if row.Observed != "points at ghs.googlehosted.com" {
		t.Errorf("observed = %q, want the record that is actually there", row.Observed)
	}
}

// The gap this closes: a domain used to be marked proven and then applied, so an
// apply that failed left a verified row absent from the Ingress with nothing
// that would ever retry it.
func TestADomainIsNotCalledRoutedUntilTheApplyTakes(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-check-noapply", "web", "ns-test-check-noapply")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.noapply.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	res := fakeResolver{cname: map[string]string{"shop.noapply.test": "edge.domain.test."}}
	broken := &fakeRouter{fail: true}
	if err := checkerFor(t, res, broken, time.Now()).Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	row, _ := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if State(row.State) != StateVerified {
		t.Fatalf("state = %q, want verified — routed would claim an Ingress that was never written", row.State)
	}

	// The next pass can request convergence again, but only observed workload
	// metadata may promote the domain to routed.
	working := &fakeRouter{}
	later := checkerFor(t, res, working, time.Now().Add(time.Hour))
	if err := later.Pass(ctx); err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	row, _ = q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if State(row.State) != StateVerified {
		t.Fatalf("state after retry = %q, want verified", row.State)
	}
	if _, err := q.MarkVerifiedDomainsRoutedForApp(ctx,
		dbgen.MarkVerifiedDomainsRoutedForAppParams{OwnerID: a.OwnerID, AppID: a.ID}); err != nil {
		t.Fatalf("mark observed routing: %v", err)
	}
	row, _ = q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if State(row.State) != StateRouted {
		t.Fatalf("state after observed convergence = %q, want routed", row.State)
	}
}

// A live domain whose record is deleted must leave the Ingress. Nothing did
// this before, because nothing ever re-checked.
func TestTheCheckerWithdrawsADomainThatDrifts(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-check-drift", "web", "ns-test-check-drift")
	q := dbgen.New(pool)

	c, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.checkdrift.test", "edge.domain.test", "apps.domain.test", nil)
	if err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	working := fakeResolver{cname: map[string]string{"shop.checkdrift.test": "edge.domain.test."}}
	router := &fakeRouter{}
	if err := checkerFor(t, working, router, time.Now()).Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, err := q.MarkVerifiedDomainsRoutedForApp(ctx,
		dbgen.MarkVerifiedDomainsRoutedForAppParams{OwnerID: a.OwnerID, AppID: a.ID}); err != nil {
		t.Fatalf("mark observed routing: %v", err)
	}
	if !hasHost(mustHosts(t, ctx, q, a.ID), "shop.checkdrift.test") {
		t.Fatal("the domain never became routed")
	}

	// The customer deletes the record. The platform's own target still
	// resolves, which is what makes this their change rather than our outage.
	deleted := fakeResolver{addrs: map[string][]string{"edge.domain.test": {"198.51.100.1"}}}
	// Well past the settled re-check interval, so the row is due again.
	future := time.Now().Add(settledRecheck + time.Hour)
	if err := checkerFor(t, deleted, router, future).Pass(ctx); err != nil {
		t.Fatalf("drift Pass: %v", err)
	}

	row, _ := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: a.OwnerID, ID: c.ID})
	if State(row.State) != StateDrifted {
		t.Fatalf("state = %q, want drifted", row.State)
	}
	if hasHost(mustHosts(t, ctx, q, a.ID), "shop.checkdrift.test") {
		t.Error("a drifted domain is still routed")
	}
	if router.calls() != 2 {
		t.Errorf("routing applied %d times, want twice — once to add, once to withdraw", router.calls())
	}
}

// A live domain is not looked at on every pass. Without the backoff every
// domain on the install would be a DNS lookup every five seconds forever.
func TestASettledDomainIsNotRecheckedImmediately(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-check-settled", "web", "ns-test-check-settled")
	q := dbgen.New(pool)

	if _, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.settled.test", "edge.domain.test", "apps.domain.test", nil); err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	res := fakeResolver{cname: map[string]string{"shop.settled.test": "edge.domain.test."}}
	router := &fakeRouter{}
	at := time.Now()

	if err := checkerFor(t, res, router, at).Pass(ctx); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	// A second pass moments later must find nothing due.
	if err := checkerFor(t, res, router, at.Add(CheckInterval)).Pass(ctx); err != nil {
		t.Fatalf("second Pass: %v", err)
	}

	if router.calls() != 1 {
		t.Errorf("routing applied %d times, want once — the second pass should have found nothing due", router.calls())
	}
}

// Claiming leases the row, so a second checker running concurrently does not
// redo the same work. This is what makes the loop safe on several replicas.
func TestClaimingLeasesARowAwayFromOtherCheckers(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-check-lease", "web", "ns-test-check-lease")
	q := dbgen.New(pool)

	if _, err := AddCustom(ctx, q, a.OwnerID, a.ID,
		"shop.lease.test", "edge.domain.test", "apps.domain.test", nil); err != nil {
		t.Fatalf("AddCustom: %v", err)
	}

	at := time.Now()
	first, err := q.ClaimDomainsDueForCheck(ctx, dbgen.ClaimDomainsDueForCheckParams{
		DueBefore: at, LeaseUntil: at.Add(checkLease), Lim: checkBatch,
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first claim took %d rows, want 1", len(first))
	}

	second, err := q.ClaimDomainsDueForCheck(ctx, dbgen.ClaimDomainsDueForCheckParams{
		DueBefore: at, LeaseUntil: at.Add(checkLease), Lim: checkBatch,
	})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second claim took %d rows, want none — the lease did not hold", len(second))
	}
}

// A managed hostname is issued by the platform and needs no proof. Checking one
// would be this install asking DNS about its own wildcard, forever.
func TestTheCheckerIgnoresPlatformHostnames(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-check-managed", "web", "ns-test-check-managed")
	q := dbgen.New(pool)

	if _, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.domain.test", TLS: true,
	}); err != nil {
		t.Fatalf("EnsureManaged: %v", err)
	}

	at := time.Now()
	claimed, err := q.ClaimDomainsDueForCheck(ctx, dbgen.ClaimDomainsDueForCheckParams{
		DueBefore: at, LeaseUntil: at.Add(checkLease), Lim: checkBatch,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, row := range claimed {
		if row.Managed {
			t.Fatalf("the checker claimed a platform hostname: %s", row.Host)
		}
	}
}

// Nothing runs at all without a resolver. An install that cannot look anything
// up would otherwise record the same failure every five seconds forever.
func TestTheCheckerDoesNothingWithoutAResolver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Run returns immediately rather than blocking on the ticker.
	NewChecker(testPool(t), nil, &fakeRouter{}, nil).Run(ctx)
}

func mustHosts(t *testing.T, ctx context.Context, q *dbgen.Queries, appID uuid.UUID) []string {
	t.Helper()
	hosts, err := RoutableHosts(ctx, q, appID)
	if err != nil {
		t.Fatalf("RoutableHosts: %v", err)
	}
	return hosts
}
