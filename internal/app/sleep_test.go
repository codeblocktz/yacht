package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store"
)

// sleepyOrchestrator is the in-memory orchestrator with the two things
// sleeping needs from a cluster — request counts and a Service address — and
// a way to make pods slow, never ready, or impossible to place.
type sleepyOrchestrator struct {
	*recordingOrchestrator

	mu            sync.Mutex
	counts        map[orchestrator.Ref]int64
	countErr      error
	readyAfter    int32 // AppStatus polls before a woken pod is ready
	neverReady    bool
	unschedulable bool
	polls         atomic.Int32

	// wakes counts applies that asked for pods with the route still at the
	// waker: the first half of a wake, which happens once per wake.
	wakes atomic.Int32
}

func (o *sleepyOrchestrator) ApplyApp(ctx context.Context, spec orchestrator.AppSpec) error {
	if spec.Waker != nil && spec.Replicas > 0 {
		o.wakes.Add(1)
	}
	return o.recordingOrchestrator.ApplyApp(ctx, spec)
}

func (o *sleepyOrchestrator) RequestCounts(
	_ context.Context, refs []orchestrator.Ref,
) (map[orchestrator.Ref]int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.countErr != nil {
		return nil, o.countErr
	}
	out := make(map[orchestrator.Ref]int64, len(refs))
	for _, r := range refs {
		out[r] = o.counts[r]
	}
	return out, nil
}

func (o *sleepyOrchestrator) setCount(ref orchestrator.Ref, n int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.counts[ref] = n
}

func (o *sleepyOrchestrator) AppStatus(ctx context.Context, ref orchestrator.Ref) (orchestrator.AppStatus, error) {
	st, err := o.Noop.AppStatus(ctx, ref)
	if err != nil || st.Desired == 0 {
		return st, err
	}
	o.mu.Lock()
	never, after := o.neverReady, o.readyAfter
	o.mu.Unlock()
	if never || o.polls.Add(1) <= after {
		st.Phase, st.Ready, st.Available = orchestrator.PhasePending, 0, 0
	}
	return st, nil
}

func (o *sleepyOrchestrator) Pods(ctx context.Context, opts orchestrator.PodListOptions) ([]orchestrator.PodInfo, error) {
	o.mu.Lock()
	stuck := o.unschedulable
	o.mu.Unlock()
	if stuck {
		return []orchestrator.PodInfo{{
			Name: "web-0", Namespace: opts.Namespace, Phase: "Pending",
			Reason:  orchestrator.ReasonUnschedulable,
			Message: "0/1 nodes are available: 1 Insufficient memory.",
		}}, nil
	}
	return o.Noop.Pods(ctx, opts)
}

func (o *sleepyOrchestrator) ServiceAddress(context.Context, orchestrator.Ref) (string, error) {
	return "10.43.0.10:80", nil
}

var testWaker = &orchestrator.WakerEndpoint{IP: "10.0.0.5", Port: 8090}

// sleepService is a service that can put apps to sleep, over a fake cluster.
func sleepService(t *testing.T, opts Options) (*Service, *sleepyOrchestrator, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run app service tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	orch := &sleepyOrchestrator{
		recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()},
		counts:                map[orchestrator.Ref]int64{},
	}
	if opts.Manifests == nil {
		opts.Manifests = staticManifests{}
	}
	if opts.AppDomain == "" {
		opts.AppDomain = "sleep.test"
	}
	if opts.WakeTimeout == 0 {
		opts.WakeTimeout = 3 * time.Second
	}
	opts.WakePollInterval = 5 * time.Millisecond
	s := NewService(pool, orch, log, opts)
	t.Cleanup(func() {
		s.wakeWG.Wait()
		s.refusals.Wait()
	})
	return s, orch, pool
}

// sleepyApp creates and deploys an app with a public hostname.
func sleepyApp(t *testing.T, s *Service, ownerID, name string) App {
	t.Helper()
	a, err := createAndDeploy(t, s, context.Background(), ownerID, CreateInput{
		Name: name, Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if a.Host == "" {
		t.Fatalf("%s has no hostname; sleeping needs one", name)
	}
	return a
}

// idleFor backdates an app's idle clock, as though it had been counted and
// quiet for d.
func backdate(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE apps SET awake_since = now() - make_interval(secs => $2),
		                requests_seen_since = now() - make_interval(secs => $2),
		                last_request_at = NULL
		WHERE id = $1`, id, d.Seconds()); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func mustGet(t *testing.T, s *Service, ownerID, name string) App {
	t.Helper()
	a, err := s.Get(context.Background(), ownerID, name)
	if err != nil {
		t.Fatalf("Get %s: %v", name, err)
	}
	return a
}

func sleepOn(t *testing.T, s *Service, ownerID, name string, after time.Duration) {
	t.Helper()
	if _, err := s.SetSleepSetting(context.Background(), ownerID, name,
		SleepSetting{Mode: SleepOn, After: after}); err != nil {
		t.Fatalf("SetSleepSetting: %v", err)
	}
}

func TestSleepingIsOffUntilSomebodyTurnsItOn(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-defaults")
	a := sleepyApp(t, s, id, "sleepy-default")

	if a.Sleep.Setting.Mode != SleepInherit || a.Sleep.State != SleepAwake {
		t.Fatalf("a new app's sleep = %+v; want inheriting, awake", a.Sleep)
	}
	team, err := s.TeamSleepDefault(ctx, id)
	if err != nil {
		t.Fatalf("TeamSleepDefault: %v", err)
	}
	if team.Enabled || team.After != 30*time.Minute {
		t.Fatalf("team default = %+v; want off, 30 minutes", team)
	}
	if p := DefaultCapacityPolicy(); p.WakeReservePercent != 25 {
		t.Fatalf("wake reserve = %d%%, want 25%% by default", p.WakeReservePercent)
	}

	// A day without a request changes nothing while it is off.
	orch.setCount(a.Ref(), 7)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	backdate(t, pool, a.ID, 24*time.Hour)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("an app with sleeping off went to sleep: %+v", got.Sleep)
	}
}

func TestAnAppIdleForItsTimeSleepsAndIsRoutedToTheWaker(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-idle")
	a := sleepyApp(t, s, id, "sleepy-idle")
	sleepOn(t, s, id, a.Name, 5*time.Minute)

	// The first pass only starts counting: an app nobody has counted
	// requests for is never idle, however long it has been up.
	orch.setCount(a.Ref(), 40)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake || got.Sleep.CountedSince == nil {
		t.Fatalf("after the first count: %+v; want awake and counted", got.Sleep)
	}

	// Four minutes quiet is not five.
	backdate(t, pool, a.ID, 4*time.Minute)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("slept after four minutes of a five-minute policy")
	}

	backdate(t, pool, a.ID, 6*time.Minute)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	got := mustGet(t, s, id, a.Name)
	if got.Sleep.State != SleepAsleep || got.Sleep.Since == nil || got.RunningReplicas() != 0 {
		t.Fatalf("after six idle minutes: %+v, running %d; want asleep, none running",
			got.Sleep, got.RunningReplicas())
	}
	spec := orch.lastAppSpec()
	if spec.Replicas != 0 || spec.Waker == nil || *spec.Waker != *testWaker {
		t.Fatalf("applied %d replicas, waker %v; want 0, routed to %v", spec.Replicas, spec.Waker, testWaker)
	}
	if len(spec.Hosts) == 0 {
		t.Fatalf("a sleeping app kept no hostnames for the waker to answer")
	}

	st, err := s.SleepStatus(ctx, id, a.Name)
	if err != nil {
		t.Fatalf("SleepStatus: %v", err)
	}
	if len(st.History) != 1 || st.History[0].Describe() != "Slept after 5 min without requests" ||
		st.History[0].To != nil {
		t.Fatalf("history = %+v; want one open sleep after 5 minutes", st.History)
	}

	// The reconciler restores it asleep, not awake: sleeping is desired state.
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Replicas != 0 || spec.Waker == nil {
		t.Fatalf("reconciled a sleeping app to %d replicas, waker %v", spec.Replicas, spec.Waker)
	}
}

func TestARequestKeepsAnAppAwake(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-busy")
	a := sleepyApp(t, s, id, "sleepy-busy")
	sleepOn(t, s, id, a.Name, 5*time.Minute)

	orch.setCount(a.Ref(), 10)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	backdate(t, pool, a.ID, time.Hour)

	// The counter moved: somebody asked for it since the last look.
	orch.setCount(a.Ref(), 11)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	got := mustGet(t, s, id, a.Name)
	if got.Sleep.State != SleepAwake || got.Sleep.LastRequestAt == nil {
		t.Fatalf("an app with a new request: %+v; want awake with its last request recorded", got.Sleep)
	}

	// A counter that went backwards is a controller that restarted, which is
	// not proof of silence either.
	backdate(t, pool, a.ID, time.Hour)
	orch.setCount(a.Ref(), 3)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("a reset counter was taken for an idle app")
	}
}

func TestNothingSleepsWhileRequestsCannotBeCounted(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-blind")
	a := sleepyApp(t, s, id, "sleepy-blind")
	sleepOn(t, s, id, a.Name, 5*time.Minute)
	orch.setCount(a.Ref(), 1)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	backdate(t, pool, a.ID, time.Hour)

	orch.mu.Lock()
	orch.countErr = orchestrator.ErrNotSupported
	orch.mu.Unlock()
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("an app slept on a pass that could not count its requests")
	}
}

func TestAnAppIsNeverPutToSleepMidDeploy(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-deploying")
	a := sleepyApp(t, s, id, "sleepy-deploying")
	sleepOn(t, s, id, a.Name, 5*time.Minute)
	orch.setCount(a.Ref(), 1)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}

	// Admitted and not yet executed: an operation is live.
	if err := s.Redeploy(ctx, id, a.Name); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	backdate(t, pool, a.ID, time.Hour)
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("an app slept with a deploy in flight")
	}
	if err := s.SleepNow(ctx, id, a.Name); !errors.Is(err, ErrOperationInFlight) {
		t.Fatalf("SleepNow mid-deploy = %v, want ErrOperationInFlight", err)
	}

	// Once it has finished, the idle clock counts from the deploy.
	if err := executeNamedOperation(s, ctx, id, a.Name); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("an app slept the moment its deploy finished")
	}
}

func TestARequestWakesASleepingAppAndRoutesItBack(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-wake")
	a := sleepyApp(t, s, id, "sleepy-wake")
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}

	target, err := s.WakeTarget(ctx, strings.ToUpper(a.Host))
	if err != nil || target.ID != a.ID {
		t.Fatalf("WakeTarget(%s) = %v, %v; want the app", a.Host, target.Name, err)
	}
	if _, err := s.WakeTarget(ctx, "nobody.sleep.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("WakeTarget of a hostname nobody holds = %v, want ErrNotFound", err)
	}

	if err := s.WakeForRequest(ctx, target); err != nil {
		t.Fatalf("WakeForRequest: %v", err)
	}
	got := mustGet(t, s, id, a.Name)
	if got.Sleep.State != SleepAwake || got.Sleep.LastRequestAt == nil {
		t.Fatalf("after a wake: %+v; want awake, with the request recorded", got.Sleep)
	}
	if spec := orch.lastAppSpec(); spec.Replicas != 1 || spec.Waker != nil {
		t.Fatalf("after a wake the app runs %d replicas, waker %v; want 1 and its own route",
			spec.Replicas, spec.Waker)
	}
	if addr, err := s.Backend(ctx, got); err != nil || addr == "" {
		t.Fatalf("Backend = %q, %v", addr, err)
	}

	st, err := s.SleepStatus(ctx, id, a.Name)
	if err != nil {
		t.Fatalf("SleepStatus: %v", err)
	}
	if len(st.History) != 1 || st.History[0].To == nil || st.History[0].DescribeWake() != "Woke on a request" ||
		st.History[0].Describe() != "Put to sleep by hand" {
		t.Fatalf("history = %+v; want one closed interval, by hand, woken by a request", st.History)
	}
}

func TestConcurrentRequestsShareOneWake(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-herd")
	a := sleepyApp(t, s, id, "sleepy-herd")
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	asleep := mustGet(t, s, id, a.Name)
	// Slow enough that every request arrives while the wake is under way.
	orch.mu.Lock()
	orch.readyAfter = 20
	orch.mu.Unlock()
	orch.wakes.Store(0)

	var wg sync.WaitGroup
	errs := make(chan error, 25)
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.WakeForRequest(ctx, asleep)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a request's wake: %v", err)
		}
	}
	if n := orch.wakes.Load(); n != 1 {
		t.Fatalf("25 requests asked for the pods %d times; want once", n)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("after the shared wake: %s", got.Sleep.State)
	}
}

func TestAWakeThatTimesOutGoesBackToSleep(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker, WakeTimeout: 150 * time.Millisecond})
	id := owner(t, s, pool, "sleep-slow")
	a := sleepyApp(t, s, id, "sleepy-slow")
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	orch.mu.Lock()
	orch.neverReady = true
	orch.mu.Unlock()

	if err := s.WakeForRequest(ctx, mustGet(t, s, id, a.Name)); !errors.Is(err, ErrWakeTimeout) {
		t.Fatalf("WakeForRequest = %v, want ErrWakeTimeout", err)
	}
	got := mustGet(t, s, id, a.Name)
	if got.Sleep.State != SleepAsleep || got.Sleep.WakeFailure != WakeFailedTimeout {
		t.Fatalf("after a timed-out wake: %+v; want asleep, failed for timeout", got.Sleep)
	}
	if spec := orch.lastAppSpec(); spec.Replicas != 0 || spec.Waker == nil {
		t.Fatalf("a timed-out wake left %d replicas asked for, waker %v", spec.Replicas, spec.Waker)
	}

	// For a minute the waker answers from the failure rather than trying
	// again on every reload.
	orch.wakes.Store(0)
	if err := s.WakeForRequest(ctx, got); !errors.Is(err, ErrWakeTimeout) {
		t.Fatalf("a request straight after = %v, want the same answer", err)
	}
	if n := orch.wakes.Load(); n != 0 {
		t.Fatalf("a request inside the cooldown asked for pods %d times", n)
	}
	// A person pressing the button is telling it to try again.
	orch.mu.Lock()
	orch.neverReady = false
	orch.mu.Unlock()
	if err := s.Wake(ctx, id, a.Name); err != nil {
		t.Fatalf("Wake by hand: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("after waking by hand: %s", got.Sleep.State)
	}
}

func TestAWakeWithNoRoomIsRefusedAndRecorded(t *testing.T) {
	ctx := context.Background()
	s, _, pool := sleepService(t, Options{Waker: testWaker})
	t.Cleanup(func() {
		s.refusals.Wait()
		if err := s.SetCapacityPolicy(context.Background(), DefaultCapacityPolicy()); err != nil {
			t.Errorf("restore the capacity policy: %v", err)
		}
	})
	id := owner(t, s, pool, "sleep-full")
	a := sleepyApp(t, s, id, "sleepy-full")
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	since := time.Now().Add(-time.Second)

	// Room for everything awake and half of this app.
	apps, _, err := installFootprints(ctx, s.q)
	if err != nil {
		t.Fatalf("footprints: %v", err)
	}
	var cpu, mem int64
	for _, f := range apps {
		c, m := f.cost()
		if !f.Asleep {
			cpu, mem = cpu+c, mem+m
		}
	}
	appCPU, appMem := shape{Replicas: 1}.cost()
	setRoom(s, cpu+appCPU/2, mem+appMem/2)
	enforce(t, s, true)

	if err := s.WakeForRequest(ctx, mustGet(t, s, id, a.Name)); !errors.Is(err, ErrWakeNoRoom) {
		t.Fatalf("WakeForRequest = %v, want ErrWakeNoRoom", err)
	}
	s.refusals.Wait()
	got := mustGet(t, s, id, a.Name)
	if got.Sleep.State != SleepAsleep || got.Sleep.WakeFailure != WakeFailedNoRoom {
		t.Fatalf("after a refused wake: %+v; want asleep, refused for room", got.Sleep)
	}
	refusals, err := s.CapacityRefusals(ctx, since, 50)
	if err != nil {
		t.Fatalf("CapacityRefusals: %v", err)
	}
	found := false
	for _, r := range refusals {
		found = found || r.TeamID == id
	}
	if !found {
		t.Fatalf("a refused wake was not recorded for the operator: %+v", refusals)
	}
	snap, err := s.Capacity(ctx)
	if err != nil || snap.RefusedLast24h == 0 {
		t.Fatalf("Capacity = %+v, %v; want the refusal counted, which is what raises the banner", snap, err)
	}

	// Room enough, and a person asks again.
	setRoom(s, cpu+appCPU*2, mem+appMem*2)
	if err := s.Wake(ctx, id, a.Name); err != nil {
		t.Fatalf("Wake with room: %v", err)
	}
}

func TestAWakeWhosePodsCannotBePlacedIsRefused(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-unplaced")
	a := sleepyApp(t, s, id, "sleepy-unplaced")
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	since := time.Now().Add(-time.Second)
	orch.mu.Lock()
	orch.neverReady, orch.unschedulable = true, true
	orch.mu.Unlock()

	if err := s.WakeForRequest(ctx, mustGet(t, s, id, a.Name)); !errors.Is(err, ErrWakeNoRoom) {
		t.Fatalf("WakeForRequest = %v, want ErrWakeNoRoom", err)
	}
	s.wakeWG.Wait()
	s.refusals.Wait()
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAsleep || got.Sleep.WakeFailure != WakeFailedNoRoom {
		t.Fatalf("after an unplaceable wake: %+v", got.Sleep)
	}
	refusals, err := s.CapacityRefusals(ctx, since, 50)
	if err != nil {
		t.Fatalf("CapacityRefusals: %v", err)
	}
	for _, r := range refusals {
		if r.TeamID == id && r.Resource == "memory" {
			return
		}
	}
	t.Fatalf("no memory refusal recorded for the team: %+v", refusals)
}

func TestCapacityCountsASleeperAtTheWakeReserve(t *testing.T) {
	ctx := context.Background()
	s, _, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-capacity")
	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "sleepy-capacity", Image: "nginx:alpine", Replicas: 2, Port: 8080,
		CPULimit: "1", MemoryLimit: "1Gi",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := s.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	after, err := s.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}

	// Two replicas of 1 vCPU and 1 GiB, counted at a quarter asleep.
	if d := before.CPU.Committed - after.CPU.Committed; d != 1500 {
		t.Fatalf("committed CPU fell by %dm; want 1500m — 2000m in full, 500m kept for the wake", d)
	}
	if d := before.Memory.Committed - after.Memory.Committed; d != (3<<30)/2 {
		t.Fatalf("committed memory fell by %d; want 1.5 GiB", d)
	}
	if after.Sleeping.Apps < 1 || after.Sleeping.ReservePercent != 25 ||
		after.Sleeping.CPUMillis-before.Sleeping.CPUMillis != 2000 ||
		after.Sleeping.ReserveCPUMillis-before.Sleeping.ReserveCPUMillis != 500 {
		t.Fatalf("sleeping = %+v (before %+v); want the app at 2000m, 500m of it reserved",
			after.Sleeping, before.Sleeping)
	}

	// A quota still counts it in full: it can wake at any moment.
	u, err := s.TeamUsage(ctx, id)
	if err != nil || u.Usage.CPUMillis != 2000 {
		t.Fatalf("team usage = %+v, %v; want the sleeper counted in full", u.Usage, err)
	}
}

func TestADeployWakesASleepingApp(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-deploy")
	a := sleepyApp(t, s, id, "sleepy-deploy")
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	if err := s.Redeploy(ctx, id, a.Name); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("admitting a deploy left the app %s", got.Sleep.State)
	}
	if err := executeNamedOperation(s, ctx, id, a.Name); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Replicas != 1 || spec.Waker != nil {
		t.Fatalf("deployed %d replicas, waker %v; want the app awake", spec.Replicas, spec.Waker)
	}
	st, err := s.SleepStatus(ctx, id, a.Name)
	if err != nil || len(st.History) != 1 || st.History[0].WokeBy != WokeByDeploy {
		t.Fatalf("history = %+v, %v; want woken for a deploy", st.History, err)
	}
}

func TestTeamDefaultsAndAppSettings(t *testing.T) {
	ctx := context.Background()
	s, _, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-team")
	a := sleepyApp(t, s, id, "sleepy-team")

	if err := s.SetTeamSleepDefault(ctx, id, SleepPolicy{Enabled: true, After: 4 * time.Minute}); err == nil {
		t.Fatalf("a four-minute default was accepted; five is the least")
	}
	if err := s.SetTeamSleepDefault(ctx, id, SleepPolicy{Enabled: true, After: 10 * time.Minute}); err != nil {
		t.Fatalf("SetTeamSleepDefault: %v", err)
	}
	if err := s.SetTeamSleepDefault(ctx, "no-such-team", DefaultSleepPolicy()); !errors.Is(err, ErrNoSuchTeam) {
		t.Fatalf("a default for a team that does not exist = %v, want ErrNoSuchTeam", err)
	}
	st, err := s.SleepStatus(ctx, id, a.Name)
	if err != nil {
		t.Fatalf("SleepStatus: %v", err)
	}
	if !st.Effective.Enabled || st.Effective.After != 10*time.Minute || st.Unavailable != nil {
		t.Fatalf("an inheriting app under an on default = %+v, unavailable %v", st.Effective, st.Unavailable)
	}

	// The app's own setting wins either way.
	if _, err := s.SetSleepSetting(ctx, id, a.Name, SleepSetting{Mode: SleepOff}); err != nil {
		t.Fatalf("SetSleepSetting: %v", err)
	}
	if st, _ = s.SleepStatus(ctx, id, a.Name); st.Effective.Enabled {
		t.Fatalf("an app turned off follows its team's default on")
	}
	if _, err := s.SetSleepSetting(ctx, id, a.Name, SleepSetting{Mode: "sometimes"}); err == nil {
		t.Fatalf("a mode that is not one was accepted")
	}
	if _, err := s.SetSleepSetting(ctx, id, a.Name, SleepSetting{Mode: SleepOn, After: 90 * time.Second}); err == nil {
		t.Fatalf("an idle time that is not whole minutes was accepted")
	}
}

func TestAnInstallWithoutAWakerCannotSleepAndWakesItsSleepers(t *testing.T) {
	ctx := context.Background()
	s, _, pool := sleepService(t, Options{})
	id := owner(t, s, pool, "sleep-nowaker")
	a := sleepyApp(t, s, id, "sleepy-nowaker")

	if err := s.SleepNow(ctx, id, a.Name); !errors.Is(err, ErrSleepUnavailable) {
		t.Fatalf("SleepNow without a waker = %v, want ErrSleepUnavailable", err)
	}
	// Asleep from when the install still had one.
	if _, err := pool.Exec(ctx, `UPDATE apps SET sleep_state = 'asleep', sleeping_since = now()
		WHERE id = $1`, a.ID); err != nil {
		t.Fatalf("mark asleep: %v", err)
	}
	if err := s.SleepIdleApps(ctx); err != nil {
		t.Fatalf("SleepIdleApps: %v", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAwake {
		t.Fatalf("a sleeper on an install with no waker stayed %s", got.Sleep.State)
	}
}

func TestAWorkerCannotSleep(t *testing.T) {
	ctx := context.Background()
	s, _, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-worker")
	w, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "sleepy-worker", Image: "busybox", Replicas: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SleepNow(ctx, id, w.Name); !errors.Is(err, ErrCannotSleep) {
		t.Fatalf("SleepNow on a worker = %v, want ErrCannotSleep — nothing could wake it", err)
	}
}

func TestSleepIntervalsSayHowLongAnAppWasAwake(t *testing.T) {
	ctx := context.Background()
	s, _, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "sleep-meter")
	a := sleepyApp(t, s, id, "sleepy-meter")

	hour := time.Now().Truncate(time.Hour).Add(-2 * time.Hour)
	// Asleep from :10 to :40 of the hour, and again from :50 on.
	if _, err := pool.Exec(ctx, `
		INSERT INTO app_sleeps (owner_id, app_id, slept_at, idle_minutes, woke_at, woke_by)
		VALUES ($1, $2, $3, 30, $4, 'request'), ($1, $2, $5, 30, NULL, NULL)`,
		id, a.ID, hour.Add(10*time.Minute), hour.Add(40*time.Minute), hour.Add(50*time.Minute)); err != nil {
		t.Fatalf("insert intervals: %v", err)
	}
	intervals, err := s.SleepIntervals(ctx, id, hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("SleepIntervals: %v", err)
	}
	if len(intervals) != 2 || intervals[1].To != nil {
		t.Fatalf("intervals = %+v; want two, the second still open", intervals)
	}
	if got := AwakeWithin(intervals, a.ID, hour, hour.Add(time.Hour)); got != 20*time.Minute {
		t.Fatalf("awake %v of the hour; want 20m", got)
	}
	if got := AwakeWithin(intervals, uuid.New(), hour, hour.Add(time.Hour)); got != time.Hour {
		t.Fatalf("an app with no intervals was awake %v; want the whole hour", got)
	}
}
