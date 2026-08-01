package domain

// Fixtures use apps.domain.test rather than a shared example domain because
// `go test ./...` runs this package alongside internal/app against one
// database, and domains.host is globally unique by design. Two packages both
// issuing web.apps.example.com collide, and the loser fails with a hostname
// that looks stale rather than contended.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/store"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// testPool migrates and returns a pool, skipping when no database is
// configured so `go test ./...` stays useful on a laptop without Postgres.
//
// The pool's Cleanup is registered before any a test adds, because cleanups
// run last-registered-first and the row deletion in seedApp has to happen
// while the pool is still open.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run store tests")
	}
	ctx := context.Background()

	if err := store.Migrate(ctx, dsn, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedApp removes any leftover owner, inserts one with a single app, and
// schedules the owner's removal. Deleting first makes the test independent of
// whatever a previous run left behind; the cascade takes the apps and domains
// with it.
func seedApp(t *testing.T, pool *pgxpool.Pool, ownerID, name, namespace string) dbgen.App {
	t.Helper()
	ctx := context.Background()

	purge := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id = $1`, ownerID); err != nil {
			t.Errorf("purge owner %s: %v", ownerID, err)
		}
	}
	purge()
	t.Cleanup(purge)

	q := dbgen.New(pool)
	if _, err := q.CreateTeamRow(ctx, dbgen.CreateTeamRowParams{ID: ownerID}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	row, err := q.CreateApp(ctx, dbgen.CreateAppParams{
		OwnerID: ownerID, Name: name, Namespace: namespace,
		Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	return row
}

func TestEnsureManagedIssuesAndReissues(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-ensure", "web", "ns-test-ensure")
	q := dbgen.New(pool)

	host, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.domain.test", TLS: true,
	})
	if err != nil {
		t.Fatalf("EnsureManaged: %v", err)
	}
	if host != "web.apps.domain.test" {
		t.Fatalf("host = %q, want web.apps.domain.test", host)
	}

	// The app domain changed. The next call must move the hostname.
	host, err = EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.domain-moved.test", TLS: true,
	})
	if err != nil {
		t.Fatalf("EnsureManaged reissue: %v", err)
	}
	if host != "web.apps.domain-moved.test" {
		t.Fatalf("reissued host = %q, want web.apps.domain-moved.test", host)
	}

	hosts, err := HostsForApp(ctx, q, a.ID)
	if err != nil {
		t.Fatalf("HostsForApp: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "web.apps.domain-moved.test" {
		t.Fatalf("hosts = %v, want exactly [web.apps.domain-moved.test]", hosts)
	}
}

func TestEnsureManagedWithNoAppDomainIsNotAnError(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-nodomain", "web", "ns-test-nodomain")
	q := dbgen.New(pool)

	host, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name, AppDomain: "",
	})
	if err != nil {
		t.Fatalf("with no app domain configured this must succeed quietly: %v", err)
	}
	if host != "" {
		t.Fatalf("host = %q, want empty", host)
	}

	hosts, err := HostsForApp(ctx, q, a.ID)
	if err != nil {
		t.Fatalf("HostsForApp: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("hosts = %v, want none", hosts)
	}
}

func TestEnsureManagedReportsACollisionClearly(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-collide-a", "web", "ns-test-collide-a")
	b := seedApp(t, pool, "test-collide-b", "web", "ns-test-collide-b")
	q := dbgen.New(pool)

	if _, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.domain.test",
	}); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// Two owners, both with an app called "web", resolve to one hostname. The
	// engine is single-owner so this cannot happen there, but a multi-tenant
	// wrapper can hit it — and it must read as a taken name, not a raw
	// constraint violation.
	_, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: b.OwnerID, AppID: b.ID, AppName: b.Name,
		AppDomain: "apps.domain.test",
	})
	if !errors.Is(err, ErrHostTaken) {
		t.Fatalf("want ErrHostTaken, got %v", err)
	}
}

// An operator can put a name out of reach, and an app claiming it is refused.
//
// Without this the list is parsed from the environment and read by nothing: an
// app called "admin" takes admin.<app domain> simply by being created first,
// whatever the operator reserved.
func TestAReservedHostnameIsNotIssued(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-reserved", "admin", "ns-test-reserved")
	q := dbgen.New(pool)

	_, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.domain.test", TLS: true,
		Reserved: []string{"admin.apps.domain.test"},
	})
	if !errors.Is(err, ErrHostReserved) {
		t.Fatalf("EnsureManaged = %v, want ErrHostReserved", err)
	}

	// And nothing was written. A refusal that still claims the globally unique
	// name would hold it against every other app.
	hosts, err := HostsForApp(ctx, q, a.ID)
	if err != nil {
		t.Fatalf("HostsForApp: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("a refused hostname was stored anyway: %v", hosts)
	}
}

// Reserving a suffix reserves what is under it, on the label boundary.
//
// The bug this rules out is the classic one: "eviladmin.apps.domain.test" ends
// with "admin.apps.domain.test" as a substring while being a different name
// entirely, and a suffix test on the raw string refuses it.
func TestReservingASuffixMatchesOnLabels(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	q := dbgen.New(pool)
	reserved := []string{"internal.domain.test"}

	under := seedApp(t, pool, "test-reserved-under", "thing", "ns-test-res-under")
	if _, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: under.OwnerID, AppID: under.ID, AppName: under.Name,
		AppDomain: "internal.domain.test", TLS: true, Reserved: reserved,
	}); !errors.Is(err, ErrHostReserved) {
		t.Fatalf("a name under the reserved domain = %v, want ErrHostReserved", err)
	}

	beside := seedApp(t, pool, "test-reserved-beside", "thing", "ns-test-res-beside")
	host, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: beside.OwnerID, AppID: beside.ID, AppName: beside.Name,
		AppDomain: "notinternal.domain.test", TLS: true, Reserved: reserved,
	})
	if err != nil {
		t.Fatalf("a name merely sharing a substring was refused: %v", err)
	}
	if host != "thing.notinternal.domain.test" {
		t.Fatalf("host = %q, want thing.notinternal.domain.test", host)
	}
}

// The app domain itself is not a reserved list. Reserved treats everything
// under it as reserved — right for "may a tenant bring this name", wrong here,
// where every issued host is under it by construction. Getting this backwards
// refuses every app on an install that reserves nothing.
func TestReservingNothingIssuesNormally(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	a := seedApp(t, pool, "test-reserved-none", "web", "ns-test-res-none")
	q := dbgen.New(pool)

	host, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.domain.test", TLS: true, Reserved: nil,
	})
	if err != nil {
		t.Fatalf("EnsureManaged with nothing reserved: %v", err)
	}
	if host != "web.apps.domain.test" {
		t.Fatalf("host = %q, want web.apps.domain.test", host)
	}
}
