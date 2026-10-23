package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Pools: which machines a team's apps run on.
//
// A pool is a label a node is given when it joins — see the cluster package.
// A team placed on one has every app of its scheduled onto that pool's nodes,
// and allowed onto those of them dedicated to the pool. A team on no pool runs
// anywhere, which is every team until somebody says otherwise: an install
// with one kind of machine has nothing to choose between.
//
// Set by the install's operator, or by an application wrapping the engine —
// a hosting business putting each plan's customers on the machines it sold
// that plan on. Like a quota, it is a decision about the team rather than
// about any one app, and no app can opt out of it.

// TeamPool is the pool a team's apps are placed on, empty for anywhere.
func (s *Service) TeamPool(ctx context.Context, ownerID string) (string, error) {
	return teamPool(ctx, s.q, ownerID)
}

// teamPool reads a team's pool on q, which is the caller's transaction when
// there is one: an apply reads it beside everything else it renders.
//
// An error rather than anywhere when the read fails. Rendered without its
// pool, an app on a dedicated pool would be rolled onto shared machines — and
// stay there, because the apply would have converged and nothing would look
// again until the next change.
func teamPool(ctx context.Context, q *dbgen.Queries, ownerID string) (string, error) {
	row, err := q.GetTeamPool(ctx, ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("app: read team pool: %w", err)
	}
	return row.Pool, nil
}

// SetTeamPool places a team's apps on a pool, or with "" anywhere.
//
// Unlike a quota it reaches what is already running, because that is what it
// is for: a team moved to a pool belongs on the pool's machines now, not on
// its next deploy. Every app of the team has its config version bumped in the
// same transaction as the change, which is how a changed secret reaches the
// cluster too — the reconciler sees each app behind its convergence key and
// re-applies it with the pool, and the pods roll onto it the way they roll for
// any other change, a replacement ready before the original stops. A sleeping
// app is re-applied at zero replicas, and wakes onto the pool.
//
// Setting the pool a team already has changes nothing and rolls nothing, so a
// caller that applies a plan's settings on every pass can call this freely.
//
// The pool is not checked against the nodes that exist. A team placed on a
// pool nobody has joined a machine to yet has apps that cannot be scheduled —
// they stay Pending, with Kubernetes saying why — and that is the honest
// result of the order an operator did things in, put right by adding the
// machine rather than refused here.
func (s *Service) SetTeamPool(ctx context.Context, ownerID, pool string) error {
	pool = strings.TrimSpace(pool)
	if err := orchestrator.ValidatePool(pool); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin team pool change: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	// No row is anywhere, the same as a row saying "". Two first placements
	// racing both find no row to lock, and both bump: one roll more than was
	// needed, never one fewer.
	var previous string
	row, err := q.LockTeamPool(ctx, ownerID)
	switch {
	case err == nil:
		previous = row.Pool
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("app: read team pool: %w", err)
	}
	if previous == pool {
		return nil
	}

	if _, err := q.UpsertTeamPool(ctx, dbgen.UpsertTeamPoolParams{OwnerID: ownerID, Pool: pool}); err != nil {
		if isForeignKeyViolation(err) {
			return ErrNoSuchTeam
		}
		return fmt.Errorf("app: set team pool: %w", err)
	}
	moved, err := q.IncrementTeamAppConfigVersions(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("app: version the team's apps: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit team pool change: %w", err)
	}

	s.log.Info("team pool set", slog.String("team", ownerID),
		slog.String("pool", pool), slog.String("was", previous), slog.Int64("apps", moved))
	return nil
}
