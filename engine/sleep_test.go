package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// countingCluster is the in-memory orchestrator with what sleeping reads
// from a cluster: request counts, and the address an app's Service answers
// on — here, a test server standing in for the app.
type countingCluster struct {
	*orchestrator.Noop
	backend string
}

func (c countingCluster) RequestCounts(_ context.Context, refs []orchestrator.Ref) (map[orchestrator.Ref]int64, error) {
	out := map[orchestrator.Ref]int64{}
	for _, r := range refs {
		out[r] = 0
	}
	return out, nil
}

func (c countingCluster) ServiceAddress(context.Context, orchestrator.Ref) (string, error) {
	return c.backend, nil
}

// What Kilicore does with sleeping: turns it on for a plan's teams, reads
// and overrides it per app, and meters an app by the time it was awake.
func TestAWrapperPutsItsSmallPlansToSleepAndMetersTheirAwakeTime(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run engine composition tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from "+r.Host)
	}))
	defer site.Close()

	const team = "kilicore-sleep"
	e, err := New(ctx, Config{
		DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
		OwnerID: team, OwnerName: "Kilicore Hobby", MaxConcurrentBuilds: 2,
		AppDomain: "kilicore-sleep.test", WakerAddr: "10.0.0.5:8090", WakeTimeout: 10 * time.Second,
	}, Overrides{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Orchestrator: countingCluster{Noop: orchestrator.NewNoop(), backend: strings.TrimPrefix(site.URL, "http://")},
		WakerBrand:   "Kilicore",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(e.Close)
	t.Cleanup(func() {
		if _, err := e.Pool.Exec(context.Background(), `DELETE FROM apps WHERE owner_id = $1`, team); err != nil {
			t.Errorf("remove the test's apps: %v", err)
		}
		if _, err := e.Pool.Exec(context.Background(), `DELETE FROM team_sleep_defaults WHERE owner_id = $1`, team); err != nil {
			t.Errorf("remove the test's default: %v", err)
		}
	})
	// The install's own defaults, whatever an earlier test left behind.
	if err := e.Apps.SetCapacityPolicy(ctx, app.DefaultCapacityPolicy()); err != nil {
		t.Fatalf("SetCapacityPolicy: %v", err)
	}
	// The operation worker deploys what Create admits.
	e.Start(ctx)

	// Off until the wrapper says otherwise.
	def, err := e.Apps.TeamSleepDefault(ctx, team)
	if err != nil || def.Enabled {
		t.Fatalf("TeamSleepDefault = %+v, %v; want off", def, err)
	}
	// The hobby plan's teams sleep after 30 idle minutes.
	if err := e.Apps.SetTeamSleepDefault(ctx, team, SleepPolicy{Enabled: true, After: DefaultSleepAfter}); err != nil {
		t.Fatalf("SetTeamSleepDefault: %v", err)
	}

	created, err := e.Apps.Create(ctx, team, app.CreateInput{
		Name: "blog", Replicas: 1, Port: 8080,
		Image: "nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	blog := waitDeployed(t, e, team, created.Name)

	st, err := e.Apps.SleepStatus(ctx, team, blog.Name)
	if err != nil {
		t.Fatalf("SleepStatus: %v", err)
	}
	if st.Setting.Mode != SleepInherit || !st.Effective.Enabled || st.Effective.After != 30*time.Minute {
		t.Fatalf("an app on the plan = %+v; want it following the team's 30 minutes", st)
	}
	// One app on the plan paid to stay up.
	if _, err := e.Apps.SetSleepSetting(ctx, team, blog.Name, SleepSetting{Mode: SleepOff}); err != nil {
		t.Fatalf("SetSleepSetting: %v", err)
	}
	if st, _ := e.Apps.SleepStatus(ctx, team, blog.Name); st.Effective.Enabled {
		t.Fatalf("an app set never to sleep follows its team's default")
	}
	if _, err := e.Apps.SetSleepSetting(ctx, team, blog.Name, SleepSetting{Mode: SleepInherit}); err != nil {
		t.Fatalf("SetSleepSetting: %v", err)
	}

	// Asleep, it runs nothing: what a replica-hour meter reads.
	windowStart := time.Now()
	if err := e.Apps.SleepNow(ctx, team, blog.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	asleep, err := e.Apps.Get(ctx, team, blog.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if asleep.Sleep.State != SleepAsleep || asleep.RunningReplicas() != 0 || asleep.Replicas != 1 {
		t.Fatalf("asleep = %s running %d of %d; want asleep, none of its one replica running",
			asleep.Sleep.State, asleep.RunningReplicas(), asleep.Replicas)
	}
	snap, err := e.Capacity(ctx)
	if err != nil || snap.Sleeping.Apps < 1 || snap.Policy.WakeReservePercent != 25 {
		t.Fatalf("Capacity = %+v, %v; want the sleeper counted, a 25%% wake reserve", snap.Sleeping, err)
	}

	// A request for it reaches the waker, which wakes it and hands the request on.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = asleep.Host
	rec := httptest.NewRecorder()
	e.Waker().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello from "+asleep.Host {
		t.Fatalf("waker = %d %q; want the app's own answer", rec.Code, rec.Body.String())
	}
	awake, err := e.Apps.Get(ctx, team, blog.Name)
	if err != nil || awake.Sleep.State != SleepAwake || awake.RunningReplicas() != 1 {
		t.Fatalf("after the request = %+v, %v; want awake", awake.Sleep, err)
	}

	// The hourly meter: the window less the time asleep.
	windowEnd := time.Now().Add(time.Minute)
	intervals, err := e.Apps.SleepIntervals(ctx, team, windowStart, windowEnd)
	if err != nil || len(intervals) != 1 || intervals[0].To == nil || intervals[0].WokeBy != "request" {
		t.Fatalf("SleepIntervals = %+v, %v; want one finished sleep, woken by a request", intervals, err)
	}
	awakeFor := AwakeWithin(intervals, blog.ID, windowStart, windowEnd)
	if asleepFor := windowEnd.Sub(windowStart) - awakeFor; asleepFor <= 0 || asleepFor != intervals[0].To.Sub(intervals[0].From) {
		t.Fatalf("awake %v of %v; want the window less the %v asleep",
			awakeFor, windowEnd.Sub(windowStart), intervals[0].To.Sub(intervals[0].From))
	}

	// A wrapper's own worker, and the sentinel it tests for.
	if err := e.Apps.SleepNow(ctx, "no-such-team", "blog"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("SleepNow for an app that is not there = %v", err)
	}
}

// waitDeployed waits for the operation worker to make an app's first release
// active.
func waitDeployed(t *testing.T, e *Engine, team, name string) app.App {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		a, err := e.Apps.Get(context.Background(), team, name)
		if err == nil && a.ActiveReleaseID != nil && a.Host != "" {
			return a
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s was not deployed", name)
	return app.App{}
}
