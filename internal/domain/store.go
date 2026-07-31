package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

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

	if _, err := q.UpsertManagedDomain(ctx, dbgen.UpsertManagedDomainParams{
		OwnerID: in.OwnerID,
		AppID:   in.AppID,
		Host:    host,
		Tls:     in.TLS,
	}); err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: %s", ErrHostTaken, host)
		}
		return "", fmt.Errorf("domain: issue %s: %w", host, err)
	}
	return host, nil
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
