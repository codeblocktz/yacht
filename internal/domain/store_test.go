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
		Image: "nginx:alpine", Replicas: 1, Port: 8080, Env: []byte(`{}`),
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
