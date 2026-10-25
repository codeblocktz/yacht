package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrHostReserved means the operator has put this hostname out of reach.
//
// Distinct from ErrHostTaken: nobody holds it, and waiting will not free it.
// Telling somebody a name is taken when it is reserved sends them to look for
// the app that has it.
var ErrHostReserved = errors.New("domain: hostname is reserved")

// ErrHostTaken means another app already holds the hostname.
//
// Hostnames are globally unique because DNS has one owner per name. The engine
// is single-owner so it cannot collide with itself, but a multi-tenant wrapper
// can — two tenants each naming an app "web" resolve to the same host. This
// exists so that reads as a taken name rather than as a constraint violation.
var ErrHostTaken = errors.New("domain: hostname already taken")

// ManagedInput describes the platform hostname to issue for an app.
type ManagedInput struct {
	OwnerID   string
	AppID     uuid.UUID
	AppName   string
	AppDomain string
	TLS       bool

	// Reserved are the operator's additional reserved suffixes. An app name
	// that would claim one of them is refused rather than issued.
	Reserved []string
}

// EnsureManaged issues the app's platform hostname, or moves it if the app
// domain has changed since it was issued.
//
// Takes a *dbgen.Queries rather than holding its own, so a caller inside a
// transaction passes q.WithTx(tx) and the hostname is written in the same
// transaction as the app itself. An app cannot then exist without its URL.
//
// With no app domain configured it returns ("", nil): the feature is off, and
// that is a state to pass through quietly rather than an error to handle at
// every call site.
func EnsureManaged(ctx context.Context, q *dbgen.Queries, in ManagedInput) (string, error) {
	host, err := Issue(in.AppName, in.AppDomain)
	if errors.Is(err, ErrNoAppDomain) {
		// The feature has been switched off. Retire any hostname previously
		// issued rather than leaving it behind: a stale row keeps the app
		// routed at a name the operator has retired, and holds that globally
		// unique name against every other app. Backing the feature out has to
		// actually back it out.
		return "", RetireManaged(ctx, q, in.AppID)
	}
	if err != nil {
		return "", err
	}

	// Checked against the operator's list only, with the app domain left out
	// on purpose. Reserved treats everything under the app domain as reserved
	// — correct when asking whether a tenant may bring a name, and useless
	// here, where every issued host is under it by construction. What an
	// operator reserves are particular names within it, like admin.apps.example.com.
	if Reserved(host, "", in.Reserved) {
		return "", fmt.Errorf("%w: %s", ErrHostReserved, host)
	}

	return upsertManaged(ctx, q, in.OwnerID, in.AppID, host, in.TLS, false)
}

func upsertManaged(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID,
	host string, tls, generated bool,
) (string, error) {
	if _, err := q.UpsertManagedDomain(ctx, dbgen.UpsertManagedDomainParams{
		OwnerID:   ownerID,
		AppID:     appID,
		Host:      host,
		Tls:       tls,
		Generated: generated,
	}); err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: %s", ErrHostTaken, host)
		}
		return "", fmt.Errorf("domain: issue %s: %w", host, err)
	}
	return host, nil
}

// Managed is an app's platform hostname as stored.
type Managed struct {
	Host string

	// Generated is a hostname under up.<app domain>, made by Generate Domain
	// or chosen since. False is the <name>.<app domain> every app was given
	// when it was created, before an app had to ask for one.
	Generated bool

	// Label is the part before the domain: the app's name for one issued at
	// create, and whatever was generated or chosen otherwise.
	Label string
}

// GetManaged returns an app's platform hostname, and false when it has none.
func GetManaged(ctx context.Context, q *dbgen.Queries, appID uuid.UUID) (Managed, bool, error) {
	row, err := q.GetManagedDomain(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Managed{}, false, nil
	}
	if err != nil {
		return Managed{}, false, fmt.Errorf("domain: read hostname: %w", err)
	}
	label, _, _ := strings.Cut(row.Host, ".")
	return Managed{Host: row.Host, Generated: row.Generated, Label: label}, true, nil
}

// ReconcileManaged keeps the platform hostname an app already holds in line
// with the app domain, and issues none to an app that holds none.
//
// The difference from EnsureManaged is the whole of Generate Domain. An app is
// no longer given a hostname for existing; it has one because somebody asked,
// and an apply that issued one regardless would undo every app that never did.
// What an app does hold moves with the app domain, under the same label and of
// the same kind — <label>.<domain> stays one, <label>.up.<domain> the other —
// so changing the domain still moves every URL on its next apply.
//
// With no app domain configured the hostname is retired, as EnsureManaged
// retires it: the feature is off, and a row left behind would keep the app
// routed at a name the operator has given up.
func ReconcileManaged(ctx context.Context, q *dbgen.Queries, in ManagedInput) (string, error) {
	if normalize(in.AppDomain) == "" {
		return "", RetireManaged(ctx, q, in.AppID)
	}
	held, ok, err := GetManaged(ctx, q, in.AppID)
	if err != nil || !ok {
		return "", err
	}
	base := normalize(in.AppDomain)
	if held.Generated {
		base = GeneratedDomain(base)
	}
	host := held.Label + "." + base
	if host == held.Host {
		return host, nil
	}
	if len(host) > maxHostname {
		return "", fmt.Errorf("domain: hostname %q exceeds %d characters", host, maxHostname)
	}
	if Reserved(host, "", in.Reserved) {
		return "", fmt.Errorf("%w: %s", ErrHostReserved, host)
	}
	return upsertManaged(ctx, q, in.OwnerID, in.AppID, host, in.TLS, held.Generated)
}

// GenerateInput describes a generated hostname: <label>.up.<app domain>.
type GenerateInput struct {
	OwnerID   string
	AppID     uuid.UUID
	Label     string
	AppDomain string
	TLS       bool
	Reserved  []string
}

// IssueGenerated gives an app the generated hostname named by in.Label,
// replacing whatever platform hostname it held, or moving it if it held one.
//
// One platform hostname per app, as before — the partial unique index is still
// what holds that — so choosing a new name releases the old one in the same
// statement, and nothing is left holding a name the app no longer answers on.
//
// ErrHostTaken when another app holds the name; ErrNoAppDomain when there is no
// domain to put it under.
func IssueGenerated(ctx context.Context, q *dbgen.Queries, in GenerateInput) (string, error) {
	base := GeneratedDomain(in.AppDomain)
	if base == "" {
		return "", ErrNoAppDomain
	}
	if err := ValidateLabel(in.Label); err != nil {
		return "", err
	}
	host := normalize(in.Label) + "." + base
	if len(host) > maxHostname {
		return "", fmt.Errorf("domain: hostname %q exceeds %d characters", host, maxHostname)
	}
	if Reserved(host, "", in.Reserved) {
		return "", fmt.Errorf("%w: %s", ErrHostReserved, host)
	}
	return upsertManaged(ctx, q, in.OwnerID, in.AppID, host, in.TLS, true)
}

// RetireManaged removes an app's platform-issued hostname, releasing the name
// for reuse. It is a no-op when the app has none.
func RetireManaged(ctx context.Context, q *dbgen.Queries, appID uuid.UUID) error {
	if err := q.DeleteManagedDomain(ctx, appID); err != nil {
		return fmt.Errorf("domain: retire hostname: %w", err)
	}
	return nil
}

// HostsForApp returns every hostname routed to an app, managed first.
func HostsForApp(ctx context.Context, q *dbgen.Queries, appID uuid.UUID) ([]string, error) {
	rows, err := q.ListDomainsByApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("domain: list for app: %w", err)
	}
	hosts := make([]string, 0, len(rows))
	for _, r := range rows {
		hosts = append(hosts, r.Host)
	}
	return hosts, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
