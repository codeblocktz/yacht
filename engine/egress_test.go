package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
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

// What Kilicore reads to charge for egress: each app's bytes for an hour,
// with the team and project to charge them to — an app deleted since
// included, for what it sent before it went.
//
// Against the real stores CI runs: Postgres, and VictoriaMetrics with
// Traefik's counters written into it as its scraper would have.
func TestAWrapperReadsWhatEachAppSentInAnHour(t *testing.T) {
	dsn, vm := os.Getenv("YACHT_TEST_DATABASE_URL"), os.Getenv("YACHT_TEST_VICTORIAMETRICS_URL")
	if dsn == "" || vm == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL and YACHT_TEST_VICTORIAMETRICS_URL to read egress through the engine")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Teams of this run's own: a namespace is named for its team and app,
	// so another run's samples in the store would otherwise be this one's.
	b := make([]byte, 4)
	rand.Read(b)
	run := hex.EncodeToString(b)
	teamA, teamB := "kilicore-egress-a-"+run, "kilicore-egress-b-"+run
	e, err := New(ctx, Config{
		DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
		OwnerID: teamA, OwnerName: "Kilicore Egress A", MaxConcurrentBuilds: 2,
		MetricsURL: vm,
	}, Overrides{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Orchestrator: orchestrator.NewNoop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(e.Close)
	clean := func() {
		for _, q := range []string{
			`DELETE FROM apps WHERE owner_id IN ($1, $2)`,
			`DELETE FROM deleted_apps WHERE owner_id IN ($1, $2)`,
			`DELETE FROM teams WHERE id IN ($1, $2)`,
		} {
			if _, err := e.Pool.Exec(context.Background(), q, teamA, teamB); err != nil {
				t.Errorf("remove the test's apps: %v", err)
			}
		}
	}
	t.Cleanup(clean)
	// The operation worker deploys what Create admits, which Delete waits for.
	e.Start(ctx)
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
	web, api := create(teamA, "web"), create(teamB, "api")
	gone, old := create(teamB, "preview-12"), create(teamB, "old")

	// The hour that ended two hours ago; Traefik replica a, of this run's
	// own, scraped every 30 s from an hour before it to its end.
	from := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	to := from.Add(time.Hour)
	instance := "egress-" + run
	var body strings.Builder
	counter := func(a app.App, until time.Time, step int) {
		label := observe.TraefikService(observe.Service{Name: a.Name, Namespace: a.Namespace, Port: 8080})
		v := 1_000_000
		for at := from.Add(-time.Hour); !at.After(until); at = at.Add(30 * time.Second) {
			fmt.Fprintf(&body, "traefik_service_responses_bytes_total{job=\"traefik\",instance=%q,service=%q,code=\"200\"} %d %d\n",
				instance, label, v, at.UnixMilli())
			v += step
		}
	}
	for at := from.Add(-time.Hour); !at.After(to); at = at.Add(30 * time.Second) {
		fmt.Fprintf(&body, "up{job=\"traefik\",instance=%q} 1 %d\n", instance, at.UnixMilli())
	}
	counter(web, to, 100)
	counter(api, to, 10)
	// The pull request closed at 40 minutes past, and its environment went.
	counter(gone, from.Add(40*time.Minute), 1_000)
	counter(old, to, 7)
	resp, err := http.Post(vm+"/api/v1/import/prometheus", "text/plain", strings.NewReader(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// A record from before the last week, which the next delete prunes.
	if _, err := e.Pool.Exec(ctx, `INSERT INTO deleted_apps (id, owner_id, name, namespace, deleted_at)
		VALUES (gen_random_uuid(), $1, 'ancient', 'yacht-0000000000000000', now() - interval '8 days')`, teamB); err != nil {
		t.Fatal(err)
	}

	// Deleted now, after the hour: its traffic in the hour is still its
	// team's. And one deleted before the hour began, whose namespace is
	// not read for it.
	for _, a := range []app.App{gone, old} {
		deadline := time.Now().Add(20 * time.Second)
		for {
			got, err := e.Apps.Get(ctx, teamB, a.Name)
			if err == nil && got.ActiveReleaseID != nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err := e.Apps.Delete(ctx, teamB, a.Name); err != nil {
			t.Fatalf("Delete %s: %v", a.Name, err)
		}
	}
	if _, err := e.Pool.Exec(ctx, `UPDATE deleted_apps SET deleted_at = $2 WHERE id = $1`,
		old.ID, from.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	var ancient int
	if err := e.Pool.QueryRow(ctx, `SELECT count(*) FROM deleted_apps WHERE owner_id = $1 AND name = 'ancient'`,
		teamB).Scan(&ancient); err != nil || ancient != 0 {
		t.Errorf("a deleted app from 8 days ago is still kept (%d, %v)", ancient, err)
	}

	want := []EgressUsage{
		{OwnerID: teamA, ProjectID: web.ProjectID, AppID: web.ID, App: "web", Bytes: 120 * 100},
		{OwnerID: teamB, ProjectID: api.ProjectID, AppID: api.ID, App: "api", Bytes: 120 * 10},
		{OwnerID: teamB, ProjectID: gone.ProjectID, AppID: gone.ID, App: "preview-12", Bytes: 80 * 1_000},
	}
	var mine []EgressUsage
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := e.Egress(ctx, from, to)
		if err != nil {
			t.Fatalf("Egress: %v", err)
		}
		mine = mine[:0]
		for _, u := range got {
			if u.OwnerID == teamA || u.OwnerID == teamB {
				mine = append(mine, u)
			}
		}
		if len(mine) >= len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(mine) != len(want) {
		t.Fatalf("Egress = %+v; want %+v", mine, want)
	}
	for i := range want {
		if mine[i] != want[i] {
			t.Errorf("row %d = %+v; want %+v", i, mine[i], want[i])
		}
	}

	end := time.Now().Truncate(time.Second)
	if _, err := e.Egress(ctx, end.Add(-time.Hour), end); !errors.Is(err, ErrEgressWindowOpen) {
		t.Errorf("a window that just ended = %v, want ErrEgressWindowOpen", err)
	}
}
