package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// The sleeper: the background pass that counts each app's requests and puts
// the idle ones to sleep.

// DefaultSleepCheckInterval is how often idle apps are looked for, and so
// how finely an app's last request is known.
const DefaultSleepCheckInterval = time.Minute

// staleWakeGrace is how far past the wake timeout a wake is left before
// another pass takes it to have been abandoned by its process.
const staleWakeGrace = 30 * time.Second

// RunSleeper looks for idle apps every SleepCheckInterval until ctx ends.
// Safe on several replicas at once: a count is recorded idempotently, and an
// app is put to sleep by a guarded write under its convergence lock.
func (s *Service) RunSleeper(ctx context.Context) {
	tick := time.NewTicker(s.opts.SleepCheckInterval)
	defer tick.Stop()
	for {
		if err := s.SleepIdleApps(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("looking for idle apps paused", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// SleepIdleApps is one pass. Exported so tests and repair tooling can run one
// without starting a clock.
//
// Nothing is put to sleep on a pass whose request counts could not be read. A
// count that could not be taken is not a count of zero, and an app assumed
// idle because its traffic could not be seen is an app taken away from the
// people using it.
func (s *Service) SleepIdleApps(ctx context.Context) error {
	if !s.CanSleep() {
		return s.wakeStranded(ctx)
	}
	s.settleStaleWakes(ctx)

	rows, err := s.q.ListSleepCandidates(ctx)
	if err != nil {
		return fmt.Errorf("app: list apps that sleep: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	counts, ok := s.requestCounts(ctx, rows)
	if !ok {
		return nil
	}
	for _, row := range rows {
		if err := s.sleepIfIdle(ctx, row, counts); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Warn("could not put an idle app to sleep",
				slog.String("app", row.App.Name), slog.String("error", err.Error()))
		}
	}
	return nil
}

// sleepIfIdle records one app's count, and puts it to sleep if that leaves it
// idle for as long as its policy asks.
func (s *Service) sleepIfIdle(
	ctx context.Context, row dbgen.ListSleepCandidatesRow, counts map[orchestrator.Ref]int64,
) error {
	a := toApp(row.App)
	if s.sleepable(a) != nil {
		return nil
	}
	count, seen := counts[a.Ref()]
	if !seen {
		return nil
	}
	recorded, err := s.q.RecordRequestCount(ctx, dbgen.RecordRequestCountParams{
		OwnerID: a.OwnerID, ID: a.ID, RequestCount: count,
	})
	if err != nil {
		return fmt.Errorf("app: record request count: %w", err)
	}
	a = toApp(recorded)

	team := SleepPolicy{Enabled: row.TeamEnabled, After: time.Duration(row.TeamAfterMinutes) * time.Minute}
	policy := a.Sleep.Setting.Effective(team)
	if !policy.Enabled || !idleFor(a, policy, time.Now()) {
		return nil
	}
	idle := int32(policy.After / time.Minute)
	err = s.sleep(ctx, a, &idle, func(fresh App) bool {
		return idleFor(fresh, policy, time.Now())
	})
	if errors.Is(err, ErrOperationInFlight) || errors.Is(err, ErrCannotSleep) {
		return nil
	}
	return err
}

// idleFor reports whether an app has gone without a request for as long as
// the policy asks. Never for an app whose requests have not been counted.
func idleFor(a App, p SleepPolicy, now time.Time) bool {
	since, ok := a.Sleep.IdleSince()
	return ok && now.Sub(since) >= p.After
}

// requestCounts reads the ingress controller's counts for the candidates,
// false when they cannot be read.
func (s *Service) requestCounts(
	ctx context.Context, rows []dbgen.ListSleepCandidatesRow,
) (map[orchestrator.Ref]int64, bool) {
	counter, ok := s.orch.(orchestrator.RequestCounter)
	if !ok {
		s.requestsUnavailable(orchestrator.ErrNotSupported)
		return nil, false
	}
	refs := make([]orchestrator.Ref, len(rows))
	for i, row := range rows {
		refs[i] = toApp(row.App).Ref()
	}
	counts, err := counter.RequestCounts(ctx, refs)
	if err != nil {
		s.requestsUnavailable(err)
		return nil, false
	}
	if s.requestsUnknown.Swap(false) {
		s.log.Info("requests can be counted again — idle apps will sleep")
	}
	return counts, true
}

// requestsUnavailable logs, once, that no app will sleep until requests can
// be counted.
func (s *Service) requestsUnavailable(err error) {
	if s.requestsUnknown.Swap(true) {
		return
	}
	s.log.Warn("requests cannot be counted, so no app is put to sleep for being idle — "+
		"sleeping needs Traefik's Prometheus metrics, which its k3s chart serves by default",
		slog.String("reason", err.Error()))
}

// settleStaleWakes finishes or abandons wakes whose process went away: a
// restart mid-wake, or a replica that stopped. A pod that became ready is a
// wake that worked and only needs its route back; anything else goes back to
// sleep, and the next request tries again.
func (s *Service) settleStaleWakes(ctx context.Context) {
	rows, err := s.q.ListStaleWakes(ctx, time.Now().Add(-s.opts.WakeTimeout-staleWakeGrace))
	if err != nil {
		s.log.Warn("list abandoned wakes", slog.String("error", err.Error()))
		return
	}
	for _, row := range rows {
		a := toApp(row)
		s.wakesMu.Lock()
		_, mine := s.wakes[a.ID]
		s.wakesMu.Unlock()
		if mine {
			continue
		}
		status, err := s.orch.AppStatus(ctx, a.Ref())
		if err == nil && status.Desired > 0 && status.Ready > 0 {
			if err := s.finishWake(ctx, a, WokeByRequest); err != nil {
				s.log.Warn("finish an abandoned wake", slog.String("app", a.Name),
					slog.String("error", err.Error()))
			}
			continue
		}
		s.failWake(a, ErrWakeTimeout)
	}
}

// wakeStranded wakes every sleeping app on an install that has lost its
// waker: with nothing to route their hostnames to, a sleeper could never be
// woken again, and an app that is up is better than one that cannot be.
func (s *Service) wakeStranded(ctx context.Context) error {
	rows, err := s.q.ListSleepingApps(ctx)
	if err != nil {
		return fmt.Errorf("app: list sleeping apps: %w", err)
	}
	for _, row := range rows {
		a := toApp(row)
		if err := s.forceAwake(ctx, a, WokeByInstall); err != nil {
			s.log.Warn("wake a stranded app", slog.String("app", a.Name), slog.String("error", err.Error()))
		}
	}
	return nil
}

// forceAwake records an app awake whatever it was doing, and applies it.
func (s *Service) forceAwake(ctx context.Context, a App, by string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin wake: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := recordAwake(ctx, s.q.WithTx(tx), a.OwnerID, a.ID, by); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit wake: %w", err)
	}
	s.log.Info("app woken", slog.String("app", a.Name), slog.String("by", by))
	return s.applyApp(ctx, a)
}

// recordAwake marks an app awake from asleep or waking and closes the interval
// it slept, on the caller's transaction.
func recordAwake(ctx context.Context, q *dbgen.Queries, ownerID string, id uuid.UUID, by string) error {
	if _, err := q.MarkAppAwake(ctx, dbgen.MarkAppAwakeParams{
		OwnerID: ownerID, ID: id, FromStates: []string{string(SleepAsleep), string(SleepWaking)},
	}); err != nil {
		return fmt.Errorf("app: mark awake: %w", err)
	}
	if err := q.CloseAppSleep(ctx, dbgen.CloseAppSleepParams{
		OwnerID: ownerID, AppID: id, WokeAt: time.Now(), WokeBy: by,
	}); err != nil {
		return fmt.Errorf("app: record wake: %w", err)
	}
	return nil
}
