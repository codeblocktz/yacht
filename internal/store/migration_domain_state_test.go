package store

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 00021 is the riskiest change in this schema's history: it drops
// `verified` and re-adds it as a column generated from `state`.
//
// A live run proved nothing much — the install it ran against had two rows,
// both managed and both routed. What matters is the combinations that install
// did not have, and whether every domain that was routing before is routing
// after. A domain that silently stops being served is the failure mode, and it
// is silent.
//
// This migrates a database of its own to 00020, fills it with every
// combination, and then runs the migration under test.
func TestMigration21PreservesEveryRoutingDomain(t *testing.T) {
	ctx := context.Background()
	dsn := migrationSandbox(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}

	// Up to the version before the one under test.
	if err := goose.UpToContext(ctx, db, migrationsDir, 20); err != nil {
		t.Fatalf("migrate to 20: %v", err)
	}

	seedPreMigrationDomains(t, ctx, db)

	// The migration under test.
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	// What each row should have become. Routed is the only state a domain that
	// was already serving may land in; anything else takes it out of the
	// Ingress, which is the silent breakage this exists to catch.
	for _, want := range []struct {
		host     string
		state    string
		verified bool
	}{
		{"managed-routed.apps.test", "routed", true},
		{"custom-proven.customer.test", "routed", true},
		// Never proven, so it was not being routed and must not start.
		{"custom-unproven.customer.test", "pending", false},
	} {
		var state string
		var verified bool
		err := db.QueryRowContext(ctx,
			`SELECT state, verified FROM domains WHERE host = $1`, want.host,
		).Scan(&state, &verified)
		if err != nil {
			t.Fatalf("read %s: %v", want.host, err)
		}
		if state != want.state {
			t.Errorf("%s: state = %q, want %q", want.host, state, want.state)
		}
		if verified != want.verified {
			t.Errorf("%s: verified = %v, want %v", want.host, verified, want.verified)
		}
	}

	// The point of the generated column: the routing gate and the state on
	// screen are one fact. Asserted across every state rather than the three
	// seeded, because a mismatch here serves traffic for a domain nobody
	// proved.
	for _, state := range []string{
		"pending", "awaiting_dns", "misdirected", "verified", "routed", "drifted",
	} {
		if _, err := db.ExecContext(ctx,
			`UPDATE domains SET state = $1 WHERE host = 'custom-unproven.customer.test'`,
			state,
		); err != nil {
			t.Fatalf("set state %q: %v", state, err)
		}
		var verified bool
		if err := db.QueryRowContext(ctx,
			`SELECT verified FROM domains WHERE host = 'custom-unproven.customer.test'`,
		).Scan(&verified); err != nil {
			t.Fatalf("read verified for %q: %v", state, err)
		}
		routable := state == "verified" || state == "routed"
		if verified != routable {
			t.Errorf("state %q: verified = %v, want %v", state, verified, routable)
		}
	}
}

// The escape hatch has to work before it is needed.
//
// Rolling back must restore `verified` as a plain boolean carrying what state
// had settled on — a rollback that un-routes every live domain is worse than
// the problem it is reaching for.
func TestMigration21RollsBackWithoutUnroutingAnything(t *testing.T) {
	ctx := context.Background()
	dsn := migrationSandbox(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, migrationsDir, 20); err != nil {
		t.Fatalf("migrate to 20: %v", err)
	}
	seedPreMigrationDomains(t, ctx, db)
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	if err := goose.DownToContext(ctx, db, migrationsDir, 20); err != nil {
		t.Fatalf("roll back to 20: %v", err)
	}

	for host, want := range map[string]bool{
		"managed-routed.apps.test":      true,
		"custom-proven.customer.test":   true,
		"custom-unproven.customer.test": false,
	} {
		var verified bool
		if err := db.QueryRowContext(ctx,
			`SELECT verified FROM domains WHERE host = $1`, host,
		).Scan(&verified); err != nil {
			t.Fatalf("read %s after rollback: %v", host, err)
		}
		if verified != want {
			t.Errorf("%s: verified = %v after rollback, want %v", host, verified, want)
		}
	}

	// And it is a column that can be written again, not a generated one left
	// behind — the whole point of going back.
	if _, err := db.ExecContext(ctx,
		`UPDATE domains SET verified = false WHERE host = 'custom-proven.customer.test'`,
	); err != nil {
		t.Errorf("verified is not writable after rollback: %v", err)
	}
}

// seedPreMigrationDomains fills a 00020-era schema with the combinations that
// matter: a platform hostname, a proven custom domain, and an unproven one.
func seedPreMigrationDomains(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()

	const owner = "mig-owner"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO teams (id, display_name) VALUES ($1, 'Migration') ON CONFLICT DO NOTHING`,
		owner,
	); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	var appID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO apps (owner_id, name, namespace, image, replicas, port)
		 VALUES ($1, 'web', 'ns-mig-web', 'nginx:alpine', 1, 8080)
		 RETURNING id`, owner,
	).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	for _, d := range []struct {
		host     string
		managed  bool
		verified bool
	}{
		{"managed-routed.apps.test", true, true},
		{"custom-proven.customer.test", false, true},
		{"custom-unproven.customer.test", false, false},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO domains (owner_id, app_id, host, tls, verified, managed, verify_target)
			 VALUES ($1, $2, $3, true, $4, $5, 'edge.example.com')`,
			owner, appID, d.host, d.verified, d.managed,
		); err != nil {
			t.Fatalf("seed domain %s: %v", d.host, err)
		}
	}
}

// migrationSandbox makes a database of its own for one test.
//
// Its own, because these run migrations backwards. Doing that to the shared
// test database would leave every other package's tests looking at a schema
// from a different era, and the failures would land anywhere but here.
func migrationSandbox(t *testing.T) string {
	t.Helper()

	base := os.Getenv("YACHT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run migration tests")
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := "yacht_mig_" + strings.ToLower(strings.NewReplacer(
		"/", "_", "-", "_", ".", "_",
	).Replace(t.Name()))
	if len(name) > 60 {
		name = name[:60]
	}

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()

	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name); err != nil {
		t.Fatalf("drop sandbox: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name)
	})

	u.Path = "/" + name
	return u.String()
}
