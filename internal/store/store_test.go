package store

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDSN returns the connection string for the test database, or skips.
//
// Skipping rather than failing keeps `go test ./...` useful on a laptop with
// no Postgres, while CI sets the variable and runs the real thing.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run store tests")
	}
	return dsn
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testPool migrates and returns a pool, closing it when the test ends.
//
// The pool is closed via t.Cleanup rather than defer, and registered before
// any data cleanup a test adds. Cleanup functions run last-registered-first,
// so data deletion runs while the pool is still open. Doing this with `defer
// pool.Close()` instead silently runs every later cleanup against a closed
// pool — which leaves rows behind and makes the next run fail on a unique
// constraint.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	dsn := testDSN(t)

	if err := Migrate(ctx, dsn, quietLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedOwners removes any leftovers, inserts the given owners, and schedules
// their removal. Deleting first makes the test independent of whatever a
// previous run left behind.
func seedOwners(t *testing.T, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	ctx := context.Background()

	purge := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM owners WHERE id = ANY($1)`, ids); err != nil {
			t.Errorf("purge owners: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	for _, id := range ids {
		if _, err := pool.Exec(ctx, `INSERT INTO owners (id) VALUES ($1)`, id); err != nil {
			t.Fatalf("seed owner %s: %v", id, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)

	// Running twice must converge. Startup calls Migrate unconditionally, so
	// a second run happens on every restart.
	for i := range 2 {
		if err := Migrate(ctx, dsn, quietLogger()); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
}

// TestEveryTableIsOwnerScoped enforces the rule the open-core split depends
// on: ownership is a column on the table being queried, not a join away.
//
// This is checked mechanically rather than by review because it only has to be
// forgotten once. A table added without owner_id is one whose queries cannot
// be scoped cheaply, and the expensive scoping is the kind that gets skipped.
func TestEveryTableIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	// owners is the principal table itself — its own id is the owner.
	// goose_db_version is migration bookkeeping.
	exempt := map[string]bool{"owners": true, "goose_db_version": true}

	rows, err := pool.Query(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if len(tables) == 0 {
		t.Fatal("no tables found — did the migration run?")
	}

	for _, table := range tables {
		if exempt[table] {
			continue
		}
		var count int
		err := pool.QueryRow(ctx, `
			SELECT count(*)
			FROM information_schema.columns
			WHERE table_schema = 'public'
			  AND table_name = $1
			  AND column_name = 'owner_id'
		`, table).Scan(&count)
		if err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %q has no owner_id column — "+
				"every resource table must be owner-scoped", table)
		}
	}
}

// App names must be unique per owner, not globally. A global unique index here
// is what makes a single-owner schema impossible to extend: two owners can
// legitimately both have an app called "api".
func TestAppNameUniquenessIsScopedByOwner(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	seedOwners(t, pool, "test-a", "test-b")

	insert := func(owner, name, namespace string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO apps (owner_id, name, namespace, image)
			VALUES ($1, $2, $3, 'nginx:alpine')
		`, owner, name, namespace)
		return err
	}

	// Two owners, same app name, different namespaces: must be allowed.
	if err := insert("test-a", "api", "ns-test-a"); err != nil {
		t.Fatalf("first owner insert: %v", err)
	}
	if err := insert("test-b", "api", "ns-test-b"); err != nil {
		t.Errorf("two owners must be able to share an app name: %v", err)
	}

	// Same owner, duplicate name: must be rejected.
	if err := insert("test-a", "api", "ns-test-a-2"); err == nil {
		t.Error("duplicate app name within one owner must be rejected")
	}

	// A namespace may belong to only one app, whoever owns it. Without this
	// two owners can be handed the same namespace and delete each other's
	// workloads.
	if err := insert("test-b", "other", "ns-test-a"); err == nil {
		t.Error("a namespace must not be reusable across apps")
	}
}

// The schema rejects names that would be illegal as Kubernetes objects, so a
// bad name cannot reach a cluster even if it bypasses the orchestrator's own
// validation.
func TestSchemaRejectsInvalidKubernetesNames(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	seedOwners(t, pool, "test-names")

	cases := []struct {
		name    string
		appName string
	}{
		{"uppercase", "Api"},
		{"leading dash", "-api"},
		{"underscore", "my_api"},
		{"slash", "api/admin"},
		{"path traversal", "../kube-system"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `
				INSERT INTO apps (owner_id, name, namespace, image)
				VALUES ('test-names', $1, 'ns-valid-name', 'nginx:alpine')
			`, tc.appName)
			if err == nil {
				t.Errorf("schema accepted invalid Kubernetes name %q", tc.appName)
			}
		})
	}
}
