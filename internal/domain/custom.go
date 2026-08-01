package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// Custom is a hostname somebody brought, and whether it is proven.
type Custom struct {
	ID       uuid.UUID
	Host     string
	Verified bool

	// Target is what the CNAME has to point at. Carried on the row so a change
	// to the platform target shows up as a domain needing re-verification
	// rather than silently invalidating one that was proven against the old.
	Target string
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

// Verify proves a claim by resolving it.
//
// The check is that the name resolves to the target, by CNAME or by resolving
// to the same addresses. Requiring a literal CNAME record would fail for an
// apex domain, which cannot have one — and telling somebody their correctly
// configured apex is wrong is worse than accepting the address match.
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
	if row.VerifyTarget == "" {
		return ErrNoTarget
	}

	if !pointsAt(ctx, res, row.Host, row.VerifyTarget) {
		return ErrNotVerified
	}

	n, err := q.VerifyCustomDomain(ctx, dbgen.VerifyCustomDomainParams{OwnerID: ownerID, ID: id})
	if err != nil {
		return fmt.Errorf("domain: mark verified: %w", err)
	}
	if n == 0 {
		return ErrDomainNotFound
	}
	return nil
}

// pointsAt reports whether host resolves to target.
func pointsAt(ctx context.Context, res Resolver, host, target string) bool {
	if cname, err := res.LookupCNAME(ctx, host); err == nil {
		if normalize(cname) == normalize(target) {
			return true
		}
	}

	// An apex domain cannot carry a CNAME, so the fallback is that both names
	// answer with the same addresses. A flattened ALIAS record looks exactly
	// like this and is the correct way to point an apex at a platform.
	want, err := res.LookupHost(ctx, target)
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := res.LookupHost(ctx, host)
	if err != nil || len(got) == 0 {
		return false
	}

	set := make(map[string]bool, len(want))
	for _, a := range want {
		set[a] = true
	}
	for _, a := range got {
		if set[a] {
			return true
		}
	}
	return false
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

func toCustom(row dbgen.Domain) Custom {
	return Custom{
		ID: row.ID, Host: row.Host, Verified: row.Verified, Target: row.VerifyTarget,
	}
}
