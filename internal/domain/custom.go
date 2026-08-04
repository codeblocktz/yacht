package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrNotVerified means the domain's DNS does not point at the platform yet.
var ErrNotVerified = errors.New("domain: the DNS for this name does not point here yet")

// ErrNoTarget means the install has not been told what custom domains point at.
var ErrNoTarget = errors.New("domain: no CNAME target is configured for this install")

// ErrDomainNotFound means the app has no such custom domain.
var ErrDomainNotFound = errors.New("domain: no such domain")

// Resolver looks up DNS. An interface so verification can be tested without a
// network, and so a caller can substitute a resolver that does not use the
// host's own cache — a freshly created record is exactly the case where a
// cached negative answer is wrong.
type Resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// NetResolver is the standard library's resolver.
type NetResolver struct{ R *net.Resolver }

func (n NetResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	return n.resolver().LookupCNAME(ctx, host)
}

func (n NetResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return n.resolver().LookupHost(ctx, host)
}

func (n NetResolver) resolver() *net.Resolver {
	if n.R != nil {
		return n.R
	}
	return net.DefaultResolver
}

// Custom is a hostname somebody brought, and how far it has got.
type Custom struct {
	ID       uuid.UUID
	Host     string
	Verified bool

	// State is the detail Verified flattens away. Verified is kept because it
	// is what routing is gated on, and it is generated in the database from
	// exactly this value — the two cannot disagree.
	State State

	// Target is what the CNAME has to point at. Carried on the row so a change
	// to the platform target shows up as a domain needing re-verification
	// rather than silently invalidating one that was proven against the old.
	Target string

	// Observed is what the name resolved to when it was last looked at, as a
	// sentence. Empty when the last check found nothing, or found what it
	// wanted — a domain that is working has nothing to explain.
	Observed string

	// LastError is why the last check could not answer. Distinct from Observed:
	// a resolver timing out says nothing about the domain, and merging the two
	// would present an outage as a misconfiguration.
	LastError string

	// LastCheckedAt is zero until something has actually looked.
	LastCheckedAt time.Time
	NextCheckAt   time.Time

	// CreatedAt is when the claim was made. The first step of the progression
	// the page draws, and the only one that is true the moment a domain exists.
	CreatedAt time.Time

	// VerifiedAt is when this was first proven, not when it was last checked.
	VerifiedAt time.Time

	// Attempts is how many consecutive checks have run without it settling.
	// Drives the backoff, and is reset by asking for a check by hand.
	Attempts int
}

// Observation is what one look at DNS actually saw.
//
// pointsAt used to answer this question with a bool, which is why the failure
// message was a black box: the code knew the name resolved to
// ghs.googlehosted.com and threw that away before anybody could be told.
type Observation struct {
	// Resolves reports whether the name answered at all, by any record type.
	Resolves bool

	// CNAME is the canonical name the host answered with. Empty when there is
	// no CNAME, including the common case where a resolver answers a plain A
	// record by echoing the name back.
	CNAME string

	// Addrs is what the host resolves to; TargetAddrs what the target does.
	// Both kept so the diagnosis can show the comparison that was actually
	// made rather than asserting its conclusion.
	Addrs       []string
	TargetAddrs []string

	// PointsHere is the verdict the routing gate is built on.
	PointsHere bool

	// Err is why the lookup could not answer. A non-nil Err means this
	// observation says nothing about the domain.
	Err error
}

// Describe renders what was seen, for somebody who has to fix it.
//
// One line, because it sits under a step in a list. The CNAME is preferred over
// addresses: it is what the person typed into their provider, so it is what they
// can recognise as wrong.
func (o Observation) Describe() string {
	switch {
	case o.Err != nil:
		return ""
	case o.CNAME != "":
		return "points at " + strings.TrimSuffix(o.CNAME, ".")
	case len(o.Addrs) > 0:
		return "resolves to " + strings.Join(o.Addrs, ", ")
	}
	return ""
}

// Probe looks up a host and reports everything it saw.
//
// The lookups are the same two pointsAt has always made, in the same order and
// with the same meaning. What is different is that the detail survives the call.
func Probe(ctx context.Context, res Resolver, host, target string) Observation {
	var obs Observation

	if cname, err := res.LookupCNAME(ctx, host); err == nil {
		// A resolver with no CNAME to report answers with the name it was
		// asked about. Treating that as a CNAME would tell somebody their
		// A record is a CNAME pointing at itself.
		if normalize(cname) != normalize(host) {
			obs.CNAME = cname
			obs.Resolves = true
		}
		if normalize(cname) == normalize(target) {
			obs.PointsHere = true
			return obs
		}
	}

	// An apex domain cannot carry a CNAME, so the fallback is that both names
	// answer with the same addresses. A flattened ALIAS record looks exactly
	// like this and is the correct way to point an apex at a platform.
	want, wantErr := res.LookupHost(ctx, target)
	obs.TargetAddrs = want

	got, gotErr := res.LookupHost(ctx, host)
	if gotErr == nil && len(got) > 0 {
		obs.Addrs = got
		obs.Resolves = true
	}

	// The target failing to resolve is this install's problem, not the
	// customer's. Reported as an error rather than as the domain being wrong,
	// so a broken CNAME target does not mark every domain on the install
	// misdirected.
	if wantErr != nil || len(want) == 0 {
		obs.Err = fmt.Errorf("domain: the configured target %q does not resolve", target)
		return obs
	}
	if gotErr != nil || len(got) == 0 {
		return obs
	}

	set := make(map[string]bool, len(want))
	for _, a := range want {
		set[a] = true
	}
	for _, a := range got {
		if set[a] {
			obs.PointsHere = true
			return obs
		}
	}
	return obs
}

// AddCustom claims a hostname for an app, unverified.
//
// Nothing routes to it yet. The claim is recorded so the person can be shown
// which record to create, and proving it is a separate act — see Verify.
func AddCustom(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID,
	host, target, appDomain string, reserved []string,
) (Custom, error) {
	host = normalize(host)
	// A scheme is what people paste, so it is stripped rather than refused.
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")

	switch {
	case host == "":
		return Custom{}, errors.New("a domain is required, for example shop.example.com")
	case len(host) > maxHostname:
		return Custom{}, fmt.Errorf("domain: hostname exceeds %d characters", maxHostname)
	case !domainRE.MatchString(host):
		return Custom{}, fmt.Errorf("domain: %q is not a valid hostname", host)
	}

	// The platform's own domain is not somebody's to bring: a name under it is
	// issued by the platform, and claiming one here would route around the
	// uniqueness the platform relies on.
	if Reserved(host, appDomain, reserved) {
		return Custom{}, fmt.Errorf("%w: %s", ErrHostReserved, host)
	}
	if target == "" {
		return Custom{}, ErrNoTarget
	}

	row, err := q.CreateCustomDomain(ctx, dbgen.CreateCustomDomainParams{
		OwnerID: ownerID, AppID: appID, Host: host, VerifyTarget: target,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Said without saying who has it. Which team holds a name is not
			// this team's business, and the answer is the same either way.
			return Custom{}, fmt.Errorf("%w: %s", ErrHostTaken, host)
		}
		return Custom{}, fmt.Errorf("domain: claim %s: %w", host, err)
	}
	return toCustom(row), nil
}

// Verify proves a claim by resolving it, now.
//
// The check is that the name resolves to the target, by CNAME or by resolving
// to the same addresses. Requiring a literal CNAME record would fail for an
// apex domain, which cannot have one — and telling somebody their correctly
// configured apex is wrong is worse than accepting the address match.
//
// This and the background checker are the same code: both call Check, which is
// the only function that decides what state a domain is in. Two paths writing
// state independently is how they end up disagreeing.
func Verify(
	ctx context.Context, q *dbgen.Queries, res Resolver, ownerID string, id uuid.UUID,
) error {
	row, err := q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: ownerID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDomainNotFound
		}
		return fmt.Errorf("domain: read claim: %w", err)
	}

	state, err := Check(ctx, q, res, row, time.Now())
	if err != nil {
		return err
	}
	if !state.Routable() {
		return ErrNotVerified
	}
	return nil
}

// Check looks one domain up and records what it found.
//
// The single place a domain's state is decided and written. Returns the state it
// settled on so a caller can act on a transition — applying the Ingress when a
// domain becomes routable, or withdrawing it when one drifts.
//
// now is passed rather than read so the backoff can be tested against a clock
// the test controls.
func Check(
	ctx context.Context, q *dbgen.Queries, res Resolver, row dbgen.Domain, now time.Time,
) (State, error) {
	previous := State(row.State)

	// No target configured is not a failed check, it is an install that cannot
	// verify anything. Recorded as the error it is, leaving the state alone.
	if row.VerifyTarget == "" {
		return previous, record(ctx, q, row, previous, Observation{
			Err: ErrNoTarget,
		}, now)
	}

	obs := Probe(ctx, res, row.Host, row.VerifyTarget)
	state := Classify(obs, previous)
	return state, record(ctx, q, row, state, obs, now)
}

// record writes the outcome of one check.
func record(
	ctx context.Context, q *dbgen.Queries, row dbgen.Domain,
	state State, obs Observation, now time.Time,
) error {
	// Attempts count consecutive checks that did not change anything. A state
	// that moved is progress, and progress should be looked at again promptly
	// rather than at whatever interval the previous state had backed off to.
	attempts := row.CheckAttempts + 1
	if state != State(row.State) {
		attempts = 1
	}

	var lastErr string
	if obs.Err != nil {
		lastErr = obs.Err.Error()
	}

	_, err := q.RecordDomainCheck(ctx, dbgen.RecordDomainCheckParams{
		ID:            row.ID,
		State:         string(state),
		Observed:      obs.Describe(),
		LastError:     lastErr,
		CheckedAt:     pgtype.Timestamptz{Time: now, Valid: true},
		NextCheckAt:   now.Add(NextCheck(state, int(attempts))),
		CheckAttempts: attempts,
	})
	if err != nil {
		return fmt.Errorf("domain: record check for %s: %w", row.Host, err)
	}
	return nil
}

// ListCustom returns an app's custom domains.
func ListCustom(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID,
) ([]Custom, error) {
	rows, err := q.ListCustomDomains(ctx, dbgen.ListCustomDomainsParams{
		OwnerID: ownerID, AppID: appID,
	})
	if err != nil {
		return nil, fmt.Errorf("domain: list custom: %w", err)
	}
	out := make([]Custom, 0, len(rows))
	for _, r := range rows {
		out = append(out, toCustom(r))
	}
	return out, nil
}

// RemoveCustom releases a claim.
func RemoveCustom(
	ctx context.Context, q *dbgen.Queries, ownerID string, id uuid.UUID,
) error {
	n, err := q.DeleteCustomDomain(ctx, dbgen.DeleteCustomDomainParams{OwnerID: ownerID, ID: id})
	if err != nil {
		return fmt.Errorf("domain: remove custom: %w", err)
	}
	if n == 0 {
		return ErrDomainNotFound
	}
	return nil
}

// RoutableHosts returns the hostnames an app's Ingress should carry.
//
// A managed host is routable because the platform issued it. A custom one only
// once it is proven — the gate is in the query rather than in a caller, so
// nothing can route an unverified claim by forgetting to check.
func RoutableHosts(
	ctx context.Context, q *dbgen.Queries, appID uuid.UUID,
) ([]string, error) {
	hosts, err := q.RoutableHostsForApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("domain: routable hosts: %w", err)
	}
	return hosts, nil
}

// ToCustom converts a stored row into the shape the rest of the system reads.
//
// Exported because the install-wide list joins domains to apps and so cannot go
// through ListCustom, which is scoped to one app. Everything about how a row
// becomes a Custom still lives here rather than being reimplemented at the join.
func ToCustom(row dbgen.Domain) Custom { return toCustom(row) }

func toCustom(row dbgen.Domain) Custom {
	return Custom{
		ID:            row.ID,
		Host:          row.Host,
		Verified:      row.Verified,
		State:         State(row.State),
		Target:        row.VerifyTarget,
		Observed:      row.Observed,
		LastError:     row.LastError,
		LastCheckedAt: row.LastCheckedAt.Time,
		NextCheckAt:   row.NextCheckAt,
		CreatedAt:     row.CreatedAt,
		VerifiedAt:    row.VerifiedAt.Time,
		Attempts:      int(row.CheckAttempts),
	}
}
