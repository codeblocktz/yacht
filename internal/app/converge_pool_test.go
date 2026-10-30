package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// As many deploys at once as the pool has connections — four, the pool of a
// four-core machine — each holding its app's lock and reading through the
// pool, still all finish rather than each waiting on another's connection.
func TestAsManyDeploysAsThePoolHasConnectionsStillFinish(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run app service tests")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := NewService(pool, orchestrator.NewNoop(), slog.New(slog.NewTextHandler(io.Discard, nil)), Options{RolloutSettle: -1})

	var wg sync.WaitGroup
	errs := make(chan error, 2*cfg.MaxConns)
	for range 2 * cfg.MaxConns {
		wg.Go(func() {
			errs <- s.withAppConvergenceLock(ctx, uuid.New(), func() error {
				// Long enough for every other deploy to have taken its lock.
				time.Sleep(100 * time.Millisecond)
				_, err := s.pool.Exec(ctx, "SELECT 1")
				return err
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a deploy did not finish: %v", err)
		}
	}
}
