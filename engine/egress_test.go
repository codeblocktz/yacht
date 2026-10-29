package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/observe"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// An install with no metrics store says so, rather than reading as an
// install where nobody sent anything.
func TestEgressOnAnInstallWithNoMetricsStoreIsUnmeasured(t *testing.T) {
	end := time.Now().Truncate(time.Hour)
	if _, err := (&Engine{}).Egress(context.Background(), end.Add(-time.Hour), end); !errors.Is(err, ErrEgressUnmeasured) {
		t.Fatalf("Egress = %v, want ErrEgressUnmeasured", err)
	}
}

// metricsAt is a metrics store that answers each instant query with the
// rise it is given for that moment, by Traefik service label.
type metricsAt struct {
	mu   sync.Mutex
	at   map[int64]map[string]float64
	down bool
}

func (m *metricsAt) set(at time.Time, rise map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at[at.Unix()] = rise
}

func (m *metricsAt) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	_ = r.ParseForm()
	sec, _ := strconv.ParseInt(r.PostForm.Get("time"), 10, 64)
	var rows []map[string]any
	for svc, v := range m.at[sec] {
		rows = append(rows, map[string]any{
			"metric": map[string]string{"service": svc},
			"value":  []any{sec, strconv.FormatFloat(v, 'f', -1, 64)},
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "success", "data": map[string]any{"resultType": "vector", "result": rows},
	})
}

// What Kilicore reads to charge for egress: each app's bytes for an hour,
// with the team and project to charge them to.
func TestAWrapperReadsWhatEachAppSentInAnHour(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run engine composition tests")
	}
	ctx := context.Background()
	store := &metricsAt{at: map[int64]map[string]float64{}}
	srv := httptest.NewServer(store)
	defer srv.Close()

	const teamA, teamB = "kilicore-egress-a", "kilicore-egress-b"
	e, err := New(ctx, Config{
		DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
		OwnerID: teamA, OwnerName: "Kilicore Egress A", MaxConcurrentBuilds: 2,
		MetricsURL: srv.URL,
	}, Overrides{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Orchestrator: orchestrator.NewNoop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(e.Close)
	t.Cleanup(func() {
		if _, err := e.Pool.Exec(context.Background(),
			`DELETE FROM apps WHERE owner_id IN ($1, $2)`, teamA, teamB); err != nil {
			t.Errorf("remove the test's apps: %v", err)
		}
	})
	if err := e.Apps.EnsureOwner(ctx, teamB, "Kilicore Egress B", ""); err != nil {
		t.Fatalf("EnsureOwner: %v", err)
	}
	create := func(team, name string) app.App {
		t.Helper()
		a, err := e.Apps.Create(ctx, team, app.CreateInput{
			Name: name, Replicas: 1, Port: 8080,
			Image: "nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return a
	}
	web, api, quiet := create(teamA, "web"), create(teamB, "api"), create(teamB, "quiet")
	label := func(a app.App, port int32) string {
		return observe.TraefikService(observe.Service{Name: a.Name, Namespace: a.Namespace, Port: port})
	}

	from := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	to := from.Add(time.Hour)
	// The rise from an hour before the window to its end, and to its start:
	// what was sent in the window is the difference.
	store.set(to, map[string]float64{
		label(web, 8080):   5000,
		label(web, 80):     500, // the same app, after its port changed
		label(api, 8080):   300,
		label(quiet, 8080): 40,
		"datahuru-buckets-garage-3900@kubernetes":     9999,
		"yacht-ffffffffffffffff-gone-8080@kubernetes": 77,
	})
	store.set(from, map[string]float64{
		label(web, 8080):   1000,
		label(api, 8080):   100,
		label(quiet, 8080): 40,
	})

	got, err := e.Egress(ctx, from, to)
	if err != nil {
		t.Fatalf("Egress: %v", err)
	}
	var mine []EgressUsage
	for _, u := range got {
		if u.OwnerID == teamA || u.OwnerID == teamB {
			mine = append(mine, u)
		}
	}
	want := []EgressUsage{
		{OwnerID: teamA, ProjectID: web.ProjectID, AppID: web.ID, App: "web", Bytes: 4500},
		{OwnerID: teamB, ProjectID: api.ProjectID, AppID: api.ID, App: "api", Bytes: 200},
	}
	if len(mine) != len(want) {
		t.Fatalf("Egress = %+v; want %+v", mine, want)
	}
	for i := range want {
		if mine[i] != want[i] {
			t.Errorf("row %d = %+v; want %+v", i, mine[i], want[i])
		}
	}

	// Too soon after the window, and with the store away: errors, not zero.
	end := time.Now().Truncate(time.Second)
	if _, err := e.Egress(ctx, end.Add(-time.Hour), end); !errors.Is(err, ErrEgressWindowOpen) {
		t.Errorf("a window that just ended = %v, want ErrEgressWindowOpen", err)
	}
	store.mu.Lock()
	store.down = true
	store.mu.Unlock()
	if got, err := e.Egress(ctx, from, to); err == nil {
		t.Errorf("an unreachable store was read as %+v", got)
	}
}
