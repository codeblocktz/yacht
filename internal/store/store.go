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
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is the path inside migrationsFS.
const migrationsDir = "migrations"

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

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: set dialect: %w", err)
	}

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
