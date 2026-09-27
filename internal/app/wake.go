package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Waking a sleeping app.
//
// In three steps, each its own write: the pods are asked for with the route
// still at the waker, the app is watched until one of them is ready, and only
// then is the route switched back. The other order — route and pods together —
// sends the next requests to a Service with nothing ready behind it, which the
// ingress controller answers with a bare 503.
//
// Every request, button and replica trying to wake one app at once shares a
// single wake: in this process through wakeCall, and between processes
// through the guarded 'asleep' → 'waking' write, which exactly one of them
// makes while the rest watch it.

const (
	// DefaultWakeTimeout is how long a wake may take before it is given up
	// and the app is put back to sleep.
	DefaultWakeTimeout = 60 * time.Second

	// wakeCooldown is how long a failed wake is remembered. The waker's page
	// says "try again in a minute", and a reload a second later that tried
	// again anyway would be the app flapping for as long as somebody kept
	// the page open.
	wakeCooldown = time.Minute
)

// wakeCall is one wake in flight in this process, shared by every caller.
type wakeCall struct {
	done chan struct{}
	err  error
}

// Wake wakes an app by hand and waits for it, up to the wake timeout. A
// failed wake is tried again straight away: somebody pressing the button is
// telling us to.
func (s *Service) Wake(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if !a.Sleep.Asleep() {
		return nil
	}
	return s.wake(ctx, a, WokeByManual)
}

// WakeTarget is the app a hostname routes to, for the waker. Not owner-scoped:
// a request for a hostname is all the waker has, and the answer is the app the
// ingress controller would have sent it to anyway. ErrNotFound for a hostname
// no app holds.
func (s *Service) WakeTarget(ctx context.Context, host string) (App, error) {
	row, err := s.q.GetAppByRoutableHost(ctx, strings.ToLower(host))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, fmt.Errorf("app: find the app for %s: %w", host, err)
	}
	return toApp(row), nil
}

// WakeForRequest wakes the app a request arrived for, and waits for it until
// ctx ends. The wake itself carries on without the request: a visitor who
// gives up does not stop the app coming up for the next one.
//
// A wake that failed less than a minute ago is not tried again; its reason is
// returned instead.
func (s *Service) WakeForRequest(ctx context.Context, a App) error {
	s.touchRequest(a)
	if !a.Sleep.Asleep() {
		return nil
	}
	return s.wake(ctx, a, WokeByRequest)
}

// touchRequest notes a request the waker saw. The ingress controller's
// counter cannot: while the app sleeps its route is the waker's.
func (s *Service) touchRequest(a App) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.q.TouchAppRequest(ctx, dbgen.TouchAppRequestParams{OwnerID: a.OwnerID, ID: a.ID}); err != nil {
		s.log.Debug("record a request at the waker", slog.String("error", err.Error()))
	}
}

// Backend is where the waker hands a held request once the app is awake: its
// own Service's address. Reachable from a cluster node or a pod, and from
// nowhere else — see orchestrator.ServiceAddresser.
func (s *Service) Backend(ctx context.Context, a App) (string, error) {
	addr, ok := s.orch.(orchestrator.ServiceAddresser)
	if !ok {
		return "", orchestrator.ErrNotSupported
	}
	return addr.ServiceAddress(ctx, a.Ref())
}

// RecentWakeFailure is the reason the last wake failed, when that was less
// than a minute ago — what the waker answers with rather than trying again.
func RecentWakeFailure(a App, now time.Time) error {
	sl := a.Sleep
	if sl.WakeFailedAt == nil || now.Sub(*sl.WakeFailedAt) >= wakeCooldown {
		return nil
	}
	switch sl.WakeFailure {
	case WakeFailedNoRoom:
		return ErrWakeNoRoom
	case WakeFailedTimeout:
		return ErrWakeTimeout
	}
	return nil
}

// wake joins this process's wake of the app, starting one if there is none,
// and waits for it until ctx ends.
func (s *Service) wake(ctx context.Context, a App, by string) error {
	c := s.joinWake(a, by)
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) joinWake(a App, by string) *wakeCall {
	s.wakesMu.Lock()
	defer s.wakesMu.Unlock()
	if c, ok := s.wakes[a.ID]; ok {
		return c
	}
	c := &wakeCall{done: make(chan struct{})}
	s.wakes[a.ID] = c
	s.wakeWG.Add(1)
	go func() {
		defer s.wakeWG.Done()
		// Detached from whoever started it, and bounded by the timeout alone.
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.WakeTimeout)
		defer cancel()
		c.err = s.runWake(ctx, a.OwnerID, a.ID, by)
		s.wakesMu.Lock()
		delete(s.wakes, a.ID)
		s.wakesMu.Unlock()
		close(c.done)
	}()
	return c
}

// runWake is one wake, from start to finish.
func (s *Service) runWake(ctx context.Context, ownerID string, id uuid.UUID, by string) error {
	a, err := s.appByID(ctx, ownerID, id)
	if err != nil {
		return err
	}
	switch a.Sleep.State {
	case SleepAwake:
		return nil
	case SleepWaking:
		return s.awaitWake(ctx, a)
	}
	if by == WokeByRequest {
		if err := RecentWakeFailure(a, time.Now()); err != nil {
			return err
		}
	}

	waking, err := s.admitWake(ctx, a)
	if errors.Is(err, errNotAsleep) {
		// Somebody else — another replica, a deploy — got there first.
		return s.awaitWake(ctx, a)
	}
	if err != nil {
		return err
	}
	s.log.Info("waking app", slog.String("app", a.Name), slog.String("owner", a.OwnerID),
		slog.String("by", by))

	err = s.applyApp(ctx, waking)
	if err == nil {
		err = s.awaitReady(ctx, waking)
	}
	if errors.Is(err, errWakeOvertaken) {
		return nil
	}
	if err == nil {
		return s.finishWake(ctx, waking, by)
	}
	s.failWake(waking, err)
	return err
}

// errNotAsleep is a wake that found the app no longer asleep when it came to
// mark it waking.
var errNotAsleep = errors.New("app: not asleep")

// errWakeOvertaken is a wake a deploy finished for it.
var errWakeOvertaken = errors.New("app: the wake was overtaken")

// admitWake marks the app waking, once the install has room for it.
func (s *Service) admitWake(ctx context.Context, a App) (App, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return App{}, fmt.Errorf("app: begin wake: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	short, refused, err := s.wakeRoom(ctx, q, a.ID)
	if err != nil {
		return App{}, err
	}
	if refused {
		_ = tx.Rollback(ctx)
		s.refuseWake(a, short)
		return App{}, ErrWakeNoRoom
	}
	row, err := q.MarkAppWaking(ctx, dbgen.MarkAppWakingParams{OwnerID: a.OwnerID, ID: a.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return App{}, errNotAsleep
		}
		return App{}, fmt.Errorf("app: mark waking: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return App{}, fmt.Errorf("app: commit wake: %w", err)
	}
	return toApp(row), nil
}

// refuseWake records a wake there was no room for: on the app, for its owner
// and the waker's cooldown, and as a capacity refusal, for the operator.
func (s *Service) refuseWake(a App, short shortfall) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.q.MarkAppWakeRefused(ctx, dbgen.MarkAppWakeRefusedParams{
		OwnerID: a.OwnerID, ID: a.ID,
	}); err != nil {
		s.log.Error("record a refused wake", slog.String("error", err.Error()))
	}
	s.recordRefusal(a.OwnerID, short)
}

// applyApp converges the app's workload under the convergence lock, from a
// fresh read of it.
func (s *Service) applyApp(ctx context.Context, a App) error {
	return s.withAppConvergenceLock(ctx, a.ID, func() error {
		fresh, err := s.appByID(ctx, a.OwnerID, a.ID)
		if err != nil {
			return err
		}
		return s.apply(ctx, s.q, fresh)
	})
}

// awaitReady watches a waking app until one of its pods is ready, the
// cluster says there is no room for them, or ctx ends.
func (s *Service) awaitReady(ctx context.Context, a App) error {
	tick := time.NewTicker(s.opts.WakePollInterval)
	defer tick.Stop()
	for n := 0; ; n++ {
		status, err := s.orch.AppStatus(ctx, a.Ref())
		if err == nil && status.Desired > 0 && status.Ready > 0 {
			return nil
		}
		// Every few polls rather than every one: whether a deploy took over,
		// and whether the pods can be placed at all, are both slower news.
		if n%4 == 3 {
			if err := s.stillWaking(ctx, a); err != nil {
				return err
			}
			if short, ok := s.unplaceable(ctx, a); ok {
				s.recordRefusal(a.OwnerID, short)
				return ErrWakeNoRoom
			}
		}
		select {
		case <-ctx.Done():
			return ErrWakeTimeout
		case <-tick.C:
		}
	}
}

// stillWaking reports errWakeOvertaken when the app is no longer waking — a
// deploy woke it outright, or it was deleted.
func (s *Service) stillWaking(ctx context.Context, a App) error {
	now, err := s.appByID(ctx, a.OwnerID, a.ID)
	if errors.Is(err, ErrNotFound) {
		return errWakeOvertaken
	}
	if err != nil || now.Sleep.State == SleepWaking {
		return nil
	}
	return errWakeOvertaken
}

// unplaceable reports a pod of the app that no node has room for, and how
// much of which resource the app needs — the refusal the operator is shown.
func (s *Service) unplaceable(ctx context.Context, a App) (shortfall, bool) {
	pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{Namespace: a.Namespace, ManagedOnly: true})
	if err != nil {
		return shortfall{}, false
	}
	for _, p := range pods {
		if p.Reason != orchestrator.ReasonUnschedulable {
			continue
		}
		cpu, mem := shape{Replicas: 1, CPULimit: a.CPULimit, MemoryLimit: a.MemoryLimit}.cost()
		if strings.Contains(strings.ToLower(p.Message), "memory") {
			return shortfall{"memory", mem}, true
		}
		return shortfall{"cpu", cpu}, true
	}
	return shortfall{}, false
}

// finishWake records the app awake, closes the interval it slept, and routes
// its hostnames back to it.
func (s *Service) finishWake(ctx context.Context, a App, by string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin finishing a wake: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	row, err := q.MarkAppAwake(ctx, dbgen.MarkAppAwakeParams{
		OwnerID: a.OwnerID, ID: a.ID, FromStates: []string{string(SleepWaking)},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("app: mark awake: %w", err)
	}
	// From when its pods were asked for: that is when it started using the
	// machines again.
	woke := time.Now()
	if a.Sleep.wakingSince != nil {
		woke = *a.Sleep.wakingSince
	}
	if err := q.CloseAppSleep(ctx, dbgen.CloseAppSleepParams{
		OwnerID: a.OwnerID, AppID: a.ID, WokeAt: woke, WokeBy: by,
	}); err != nil {
		return fmt.Errorf("app: record wake: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit wake: %w", err)
	}
	s.log.Info("app awake", slog.String("app", a.Name), slog.String("owner", a.OwnerID),
		slog.Duration("took", time.Since(woke)))
	return s.applyApp(ctx, toApp(row))
}

// failWake puts a wake that did not finish back to sleep, with its reason.
// Its own context: the wake's own has usually just run out, which is why it
// failed.
func (s *Service) failWake(a App, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	failure := WakeFailedTimeout
	if errors.Is(cause, ErrWakeNoRoom) {
		failure = WakeFailedNoRoom
	}
	s.log.Warn("app did not wake", slog.String("app", a.Name), slog.String("owner", a.OwnerID),
		slog.String("error", cause.Error()))
	row, err := s.q.MarkAppWakeFailed(ctx, dbgen.MarkAppWakeFailedParams{
		OwnerID: a.OwnerID, ID: a.ID, WakeFailure: failure,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Error("record a failed wake", slog.String("error", err.Error()))
		}
		return
	}
	if err := s.applyApp(ctx, toApp(row)); err != nil {
		s.log.Warn("put an app back to sleep", slog.String("app", a.Name),
			slog.String("error", err.Error()))
	}
}

// awaitWake watches a wake some other process is running until it ends.
func (s *Service) awaitWake(ctx context.Context, a App) error {
	tick := time.NewTicker(s.opts.WakePollInterval)
	defer tick.Stop()
	for {
		now, err := s.appByID(ctx, a.OwnerID, a.ID)
		if err != nil {
			return err
		}
		switch now.Sleep.State {
		case SleepAwake:
			return nil
		case SleepAsleep:
			if err := RecentWakeFailure(now, time.Now()); err != nil {
				return err
			}
			return ErrWakeTimeout
		}
		select {
		case <-ctx.Done():
			return ErrWakeTimeout
		case <-tick.C:
		}
	}
}

// wakeForDeploy wakes an app a deploy is being admitted for, in the
// admission's transaction. A deploy of a sleeping app is somebody about to
// look at it, and its rollout needs pods to verify — so it wakes, room
// permitting, and the operation applies it awake. An app already awake has
// its idle clock restarted for the same reason.
func (s *Service) wakeForDeploy(ctx context.Context, q *dbgen.Queries, ownerID string, id uuid.UUID) error {
	row, err := q.GetAppByID(ctx, dbgen.GetAppByIDParams{OwnerID: ownerID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("app: read app to deploy: %w", err)
	}
	a := toApp(row)
	if !a.Sleep.Asleep() {
		if err := q.ResetIdleClock(ctx, dbgen.ResetIdleClockParams{OwnerID: ownerID, ID: id}); err != nil {
			return fmt.Errorf("app: restart idle clock: %w", err)
		}
		return nil
	}
	if a.Sleep.State == SleepAsleep {
		short, refused, err := s.wakeRoom(ctx, q, a.ID)
		if err != nil {
			return err
		}
		if refused {
			s.recordRefusal(ownerID, short)
			return short.refusal()
		}
	}
	return recordAwake(ctx, q, ownerID, id, WokeByDeploy)
}

// appByID reads an app by id, ErrNotFound when it has gone.
func (s *Service) appByID(ctx context.Context, ownerID string, id uuid.UUID) (App, error) {
	row, err := s.q.GetAppByID(ctx, dbgen.GetAppByIDParams{OwnerID: ownerID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, fmt.Errorf("app: read app: %w", err)
	}
	return toApp(row), nil
}
