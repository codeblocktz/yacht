// Package store owns the engine's Postgres schema and connection handling.
//
// Migrations are embedded in the binary so a self-hoster runs one command and
// gets a working database, with no separate migration tool to install and no
// way for the schema to drift from the code that expects it.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is the path inside migrationsFS.
const migrationsDir = "migrations"

// migrationLock serialises migrators against one database.
//
// Two instances starting together both migrate at boot, and goose is not safe
// to run twice at once: the second finds a table the first has just created and
// fails with "already exists", which reads as a broken migration rather than as
// a race. An arbitrary constant, shared by every Yacht that talks to this
// database.
const migrationLock int64 = 0x796163687421

// goose keeps its dialect and filesystem in package-level state, so setting it
// once is both correct and the only way concurrent migrators avoid writing to
// the same variables at the same time.
var (
	gooseOnce sync.Once
	gooseErr  error
)

// Connect opens a pooled connection and verifies the database answers.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return pool, nil
}

// WaitForDatabase waits, up to wait, for the database to take connections.
//
// Started together with its database, as after a restart of the whole
// install, Yacht finds it refusing connections for a few seconds and would
// exit, to be restarted by its supervisor and counted as a crash. Only the
// network is waited out: a database that answers and refuses, a wrong
// password or a missing database, fails at once.
func WaitForDatabase(ctx context.Context, dsn string, wait time.Duration, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("store: parse dsn: %w", err)
	}
	deadline := time.Now().Add(wait)
	for logged := false; ; {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := pgx.ConnectConfig(attempt, cfg)
		cancel()
		if err == nil {
			_ = conn.Close(ctx)
			if logged {
				log.Info("the database is taking connections")
			}
			return nil
		}
		// A server that answered is the answer, even beside an address that
		// did not: pgx tries every address a host resolves to and keeps each
		// one's error, so localhost refused on ::1 and then refusing the login
		// on 127.0.0.1 carries both.
		var netErr *net.OpError
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) || !errors.As(err, &netErr) || time.Now().After(deadline) {
			return fmt.Errorf("store: connect: %w", err)
		}
		if !logged {
			log.Info("waiting for the database", slog.String("host", cfg.Host),
				slog.Duration("up_to", wait), slog.String("error", err.Error()))
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Migrate applies any pending migrations.
//
// Uses database/sql rather than the pgx pool because that is what goose
// expects; the connection is short-lived and closed before returning.
func Migrate(ctx context.Context, dsn string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("store: open for migration: %w", err)
	}
	defer db.Close()

	gooseOnce.Do(func() {
		goose.SetBaseFS(migrationsFS)
		goose.SetLogger(goose.NopLogger())
		gooseErr = goose.SetDialect("postgres")
	})
	if gooseErr != nil {
		return fmt.Errorf("store: set dialect: %w", gooseErr)
	}

	// Held on its own connection for the length of the migration. An advisory
	// lock is session-scoped, so a second migrator blocks here rather than
	// racing through and failing on a table this one is midway through
	// creating. Closing the connection would release it even if the unlock
	// below never ran.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: open migration lock connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // releasing the lock is the point

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("store: take migration lock: %w", err)
	}
	defer func() {
		// WithoutCancel so a cancelled migration still gives the lock back
		// rather than leaving the next instance waiting on a dead session.
		_, _ = conn.ExecContext(
			context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)
	}()

	before, _ := goose.GetDBVersionContext(ctx, db)
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	after, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	if before == after {
		log.Debug("schema up to date", slog.Int64("version", after))
	} else {
		log.Info("schema migrated",
			slog.Int64("from", before),
			slog.Int64("to", after),
		)
	}
	return nil
}
