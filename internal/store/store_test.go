package store

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
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
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id = ANY($1)`, ids); err != nil {
			t.Errorf("purge owners: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	for _, id := range ids {
		if _, err := pool.Exec(ctx, `INSERT INTO teams (id) VALUES ($1)`, id); err != nil {
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

// The rename must carry every foreign key with it. If it does not, apps
// silently lose their parent and the owner-scoping invariant is gone.
func TestOwnersRenamedToTeamsKeepsForeignKeys(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.constraint_column_usage ccu
		  ON tc.constraint_name = ccu.constraint_name
		WHERE tc.constraint_type = 'FOREIGN KEY' AND ccu.table_name = 'teams'
	`).Scan(&n); err != nil {
		t.Fatalf("query foreign keys: %v", err)
	}
	// apps, deployments, domains, memberships, invitations all reference it.
	if n < 3 {
		t.Fatalf("foreign keys referencing teams = %d, want at least 3 (apps, deployments, domains)", n)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.owners') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check owners: %v", err)
	}
	if exists {
		t.Fatal("owners still exists — the rename did not happen")
	}
}

func TestAccountTablesExist(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	for _, table := range []string{"users", "memberships", "sessions", "invitations"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s does not exist", table)
		}
	}
}

// A person may hold one role in a team, not two.
func TestMembershipIsUniquePerUserAndTeam(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	const teamID = "test-membership-team"
	seedOwners(t, pool, teamID)

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"membership@example.test").Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (user_id, owner_id, role) VALUES ($1, $2, 'owner')`,
		userID, teamID); err != nil {
		t.Fatalf("first membership: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (user_id, owner_id, role) VALUES ($1, $2, 'member')`,
		userID, teamID); err == nil {
		t.Fatal("a second membership for the same user and team was accepted")
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

	// Tables that legitimately carry no owner_id, each for a stated reason.
	// Extending this map is how the invariant gets quietly lost, so every
	// entry says why it is here.
	exempt := map[string]string{
		"teams":            "the principal table itself — its own id is the owner",
		"goose_db_version": "migration bookkeeping",
		"users":            "a person exists before belonging to any team",
		"sessions":         "a session belongs to a person, not to a team",
		"magic_links":      "a sign-in link proves an address, before any team is chosen",
	}

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
		if _, ok := exempt[table]; ok {
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

// TestManagedDomainIsUniquePerApp guards the partial index. An app has exactly
// one platform-issued hostname, but any number of custom domains — so the
// constraint has to be partial, and a plain unique index on app_id would
// silently forbid the custom domains this table exists for.
func TestManagedDomainIsUniquePerApp(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	seedOwners(t, pool, "test-managed-domain")

	q := dbgen.New(pool)

	appRow, err := q.CreateApp(ctx, dbgen.CreateAppParams{
		OwnerID:   "test-managed-domain",
		Name:      "web",
		Namespace: "ns-test-managed-domain",
		Image:     "nginx:alpine",
		Replicas:  1,
		Port:      8080,
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	if _, err := q.UpsertManagedDomain(ctx, dbgen.UpsertManagedDomainParams{
		OwnerID: appRow.OwnerID, AppID: appRow.ID, Host: "web.apps.example.com", Tls: true,
	}); err != nil {
		t.Fatalf("first managed domain: %v", err)
	}

	// The upsert must move the host rather than insert a second managed row.
	if _, err := q.UpsertManagedDomain(ctx, dbgen.UpsertManagedDomainParams{
		OwnerID: appRow.OwnerID, AppID: appRow.ID, Host: "web.apps.acme.com", Tls: true,
	}); err != nil {
		t.Fatalf("reissue managed domain: %v", err)
	}

	rows, err := q.ListDomainsByApp(ctx, appRow.ID)
	if err != nil {
		t.Fatalf("list domains: %v", err)
	}

	managed := 0
	for _, r := range rows {
		if r.Managed {
			managed++
			if r.Host != "web.apps.acme.com" {
				t.Fatalf("managed host = %q, want it reissued to web.apps.acme.com", r.Host)
			}
			if !r.Verified {
				t.Fatal("a platform-issued host needs no proof of ownership; want verified")
			}
		}
	}
	if managed != 1 {
		t.Fatalf("managed rows = %d, want exactly 1", managed)
	}
}

// TestPartialIndexLeavesCustomDomainsUnconstrained is the other half of
// domains_app_managed_key. The managed side is covered above; without this,
// nothing exercises the WHERE managed predicate, and a plain unique index on
// app_id would pass every existing test while silently forbidding the custom
// domains this table exists for.
func TestPartialIndexLeavesCustomDomainsUnconstrained(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	q := dbgen.New(pool)

	const ownerID = "test-partial-index"
	seedOwners(t, pool, ownerID)

	appRow, err := q.CreateApp(ctx, dbgen.CreateAppParams{
		OwnerID: ownerID, Name: "web", Namespace: "ns-test-partial-index",
		Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	if _, err := q.UpsertManagedDomain(ctx, dbgen.UpsertManagedDomainParams{
		OwnerID: ownerID, AppID: appRow.ID, Host: "web.apps.example.com", Tls: true,
	}); err != nil {
		t.Fatalf("managed domain: %v", err)
	}

	// Inserted directly: there is deliberately no production query for custom
	// domains yet, and adding one purely to satisfy a test would ship an
	// unused code path.
	for _, host := range []string{"www.customer.test", "shop.customer.test"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO domains (owner_id, app_id, host, tls, verified, managed)
			 VALUES ($1, $2, $3, true, false, false)`,
			ownerID, appRow.ID, host,
		); err != nil {
			t.Fatalf("insert custom domain %s: %v", host, err)
		}
	}

	rows, err := q.ListDomainsByApp(ctx, appRow.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (one managed, two custom)", len(rows))
	}

	managed := 0
	for _, r := range rows {
		if r.Managed {
			managed++
		}
	}
	if managed != 1 {
		t.Fatalf("managed rows = %d, want exactly 1", managed)
	}
}

// A volume belongs to one app under one name, and one mount path. Two claims
// at the same path is a workload where one silently wins.
func TestVolumeNameAndMountAreUniquePerApp(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	q := dbgen.New(pool)

	const ownerID = "test-volumes"
	seedOwners(t, pool, ownerID)

	a, err := q.CreateApp(ctx, dbgen.CreateAppParams{
		OwnerID: ownerID, Name: "web", Namespace: "ns-test-volumes",
		Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	insert := func(name, mount string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO volumes (owner_id, app_id, name, mount_path, size_bytes)
			 VALUES ($1, $2, $3, $4, $5)`,
			ownerID, a.ID, name, mount, 1<<30)
		return err
	}

	if err := insert("data", "/var/lib/data"); err != nil {
		t.Fatalf("first volume: %v", err)
	}
	if err := insert("data", "/somewhere/else"); err == nil {
		t.Error("two volumes with the same name on one app were accepted")
	}
	if err := insert("other", "/var/lib/data"); err == nil {
		t.Error("two volumes at the same mount path on one app were accepted")
	}
	if err := insert("other", "/var/lib/other"); err != nil {
		t.Errorf("a second, distinct volume was refused: %v", err)
	}
}

// The two value columns exist so one variable can be sealed while its
// neighbour stays readable. The constraint is what stops a writer leaving a
// secret in the readable column, which no amount of care in Go would prevent
// forever.
func TestSecretVariablesCannotBeStoredReadable(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	q := dbgen.New(pool)

	const ownerID = "test-variables"
	seedOwners(t, pool, ownerID)

	a, err := q.CreateApp(ctx, dbgen.CreateAppParams{
		OwnerID: ownerID, Name: "web", Namespace: "ns-test-variables",
		Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	insert := func(key, value string, sealed []byte, secret bool) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO variables (owner_id, app_id, key, value, sealed, secret)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			ownerID, a.ID, key, value, sealed, secret)
		return err
	}

	if err := insert("PLAIN", "readable", nil, false); err != nil {
		t.Fatalf("plain variable: %v", err)
	}
	if err := insert("SEALED", "", []byte{0x01, 0x02}, true); err != nil {
		t.Fatalf("sealed variable: %v", err)
	}

	if err := insert("BAD1", "plaintext-password", nil, true); err == nil {
		t.Error("a secret was accepted with its value in the readable column")
	}
	if err := insert("BAD2", "", []byte{0x01}, false); err == nil {
		t.Error("a non-secret was accepted carrying sealed bytes")
	}
	if err := insert("BAD3", "both", []byte{0x01}, true); err == nil {
		t.Error("a secret was accepted with both columns populated")
	}
	// A shell will not export a name with a dash, so a pod spec that sets one
	// produces a variable the program never sees.
	if err := insert("has-dash", "x", nil, false); err == nil {
		t.Error("a key no shell can export was accepted")
	}
	if err := insert("PLAIN", "again", nil, false); err == nil {
		t.Error("two variables with the same key on one app were accepted")
	}
}
