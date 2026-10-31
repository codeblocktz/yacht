package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// recordedEvents is an EventSink that keeps what it was told.
type recordedEvents struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordedEvents) Emit(_ context.Context, e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// kinds is every event told about, in order.
func (r *recordedEvents) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *recordedEvents) last() Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return Event{}
	}
	return r.events[len(r.events)-1]
}

// healthyStatus is a rollout of one replica that has every replica up.
func healthyStatus(restarts int32) orchestrator.AppStatus {
	return orchestrator.AppStatus{
		Phase: orchestrator.PhaseRunning, Generation: 2, ObservedGeneration: 2, Desired: 1,
		Updated: 1, Ready: 1, Available: 1, AvailableCondition: true, Restarts: restarts,
	}
}

// crashedStatus is the same rollout a moment after its container died.
func crashedStatus(restarts int32, exit string) orchestrator.AppStatus {
	return orchestrator.AppStatus{
		Phase: orchestrator.PhasePending, Generation: 2, ObservedGeneration: 2, Desired: 1,
		Updated: 1, Restarts: restarts, LastExit: exit,
	}
}

// settleOptions give the settle window a length a test can wait out, with
// looks often enough to see a crash inside it.
func settleOptions(events *recordedEvents) Options {
	return Options{
		Events: events, RolloutTimeout: 2 * time.Second,
		RolloutPollInterval: time.Millisecond, RolloutSettle: 50 * time.Millisecond,
	}
}

// firstDeploy creates an app, and leaves its first deploy to run against the
// given rollout.
func firstDeploy(
	t *testing.T, s *Service, base *recordingOrchestrator, ownerID string,
	statuses ...orchestrator.AppStatus,
) (App, error) {
	t.Helper()
	ctx := context.Background()
	a, err := s.Create(ctx, ownerID, CreateInput{Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.orch = &rolloutScriptOrchestrator{recordingOrchestrator: base, statuses: statuses}
	return a, executeLiveOperation(s, ctx, a)
}

// deploymentOf is the app's newest deployment.
func deploymentOf(t *testing.T, s *Service, ownerID string, appID uuid.UUID) dbgen.Deployment {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `
		SELECT id FROM deployments WHERE owner_id = $1 AND app_id = $2
		ORDER BY started_at DESC LIMIT 1`, ownerID, appID).Scan(&id); err != nil {
		t.Fatalf("read the newest deployment: %v", err)
	}
	d, err := s.q.GetDeployment(context.Background(), dbgen.GetDeploymentParams{OwnerID: ownerID, ID: id})
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	return d
}

// A first deploy whose container dies a moment after every start is ready
// for that moment. It was passed as "Deployment successful"; it is a deploy
// that crashed.
func TestAFirstDeployThatCrashLoopsFails(t *testing.T) {
	events := &recordedEvents{}
	s, base, pool := testService(t, settleOptions(events))
	ownerID := owner(t, s, pool, "rollout-crash-loop")
	a, err := firstDeploy(t, s, base, ownerID,
		healthyStatus(0),
		crashedStatus(1, "code 9 (Error)"),
		healthyStatus(1),
		crashedStatus(2, "code 9 (Error)"),
	)
	if err == nil {
		t.Fatal("a crash-looping first deploy succeeded")
	}
	d := deploymentOf(t, s, ownerID, a.ID)
	if d.Status != DeployFailed || !strings.Contains(d.Message, "restarted") || !strings.Contains(d.Message, "code 9") {
		t.Fatalf("deployment = %s %q, want failed, saying it restarted and how it exited", d.Status, d.Message)
	}
	if got := events.kinds(); len(got) != 1 || got[0] != EventDeployCrashed {
		t.Fatalf("events = %v, want just %s", got, EventDeployCrashed)
	}
	if app, _ := s.Get(context.Background(), ownerID, "web"); app.ActiveReleaseID != nil {
		t.Fatal("the crashed release became active")
	}
}

// Killed for its memory a moment after it came up: the same, saying so.
func TestAFirstDeployKilledForItsMemoryFails(t *testing.T) {
	events := &recordedEvents{}
	s, base, pool := testService(t, settleOptions(events))
	ownerID := owner(t, s, pool, "rollout-oom")
	a, err := firstDeploy(t, s, base, ownerID,
		healthyStatus(0),
		healthyStatus(0),
		crashedStatus(1, "code 137 (OOMKilled)"),
	)
	if err == nil {
		t.Fatal("a first deploy killed for its memory succeeded")
	}
	d := deploymentOf(t, s, ownerID, a.ID)
	if d.Status != DeployFailed || !strings.Contains(d.Message, "OOMKilled") {
		t.Fatalf("deployment = %s %q, want failed with OOMKilled", d.Status, d.Message)
	}
	if got := events.kinds(); len(got) != 1 || got[0] != EventDeployCrashed ||
		!strings.Contains(events.last().Message, "OOMKilled") {
		t.Fatalf("events = %v (%q), want just %s with OOMKilled", got, events.last().Message, EventDeployCrashed)
	}
}

// A health check that never passes fails the deploy at its healthcheck
// timeout, naming the check, and the old version stays the active one.
func TestADeployWhoseHealthCheckNeverPassesFails(t *testing.T) {
	ctx := context.Background()
	events := &recordedEvents{}
	s, base, pool := testService(t, settleOptions(events))
	ownerID := owner(t, s, pool, "rollout-health")
	prior, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// A healthcheck timeout of a second, so the test waits out one.
	if _, err := pool.Exec(ctx, `UPDATE apps SET health_timeout_seconds = 1 WHERE id = $1`, prior.ID); err != nil {
		t.Fatalf("set the healthcheck timeout: %v", err)
	}
	events.mu.Lock()
	events.events = nil
	events.mu.Unlock()
	// Running, and never ready: what a readiness probe that keeps failing
	// looks like.
	s.orch = &rolloutScriptOrchestrator{recordingOrchestrator: base, statuses: []orchestrator.AppStatus{{
		Phase: orchestrator.PhaseDegraded, Generation: 2, ObservedGeneration: 2, Desired: 1,
		Updated: 1, Ready: 0,
	}}}
	if err := s.SetHealth(ctx, ownerID, "web", "/healthz", false); err != nil {
		t.Fatalf("SetHealth: %v", err)
	}
	if err := executeNamedOperation(s, ctx, ownerID, "web"); err == nil {
		t.Fatal("a deploy whose health check never passed succeeded")
	}
	d := deploymentOf(t, s, ownerID, prior.ID)
	if d.Status != DeployFailed || !strings.Contains(d.Message, "health check at /healthz") {
		t.Fatalf("deployment = %s %q, want failed naming the health check", d.Status, d.Message)
	}
	if got := events.kinds(); len(got) != 1 || got[0] != EventDeployFailed {
		t.Fatalf("events = %v, want just %s", got, EventDeployFailed)
	}
	if app, _ := s.Get(ctx, ownerID, "web"); app.ActiveReleaseID == nil || *app.ActiveReleaseID != *prior.ActiveReleaseID {
		t.Fatal("a failed health check moved the active release")
	}
}

// Slow to come up and then fine: it succeeds, once it has stayed up for the
// settle window and not before.
func TestASlowButHealthyFirstDeploySucceedsAfterSettling(t *testing.T) {
	events := &recordedEvents{}
	opts := settleOptions(events)
	// Longer than creating the app takes, so finishing after it is the
	// window's doing.
	opts.RolloutSettle = 300 * time.Millisecond
	s, base, pool := testService(t, opts)
	ownerID := owner(t, s, pool, "rollout-slow")
	waiting := orchestrator.AppStatus{Phase: orchestrator.PhasePending, Generation: 2, ObservedGeneration: 2,
		Desired: 1, Updated: 1}
	started := time.Now()
	a, err := firstDeploy(t, s, base, ownerID,
		orchestrator.AppStatus{Generation: 2, ObservedGeneration: 1, Desired: 1},
		waiting, waiting, waiting, waiting,
		healthyStatus(0),
	)
	if err != nil {
		t.Fatalf("a slow but healthy first deploy: %v", err)
	}
	if took := time.Since(started); took < opts.RolloutSettle {
		t.Fatalf("succeeded after %s, before the %s settle window", took, opts.RolloutSettle)
	}
	d := deploymentOf(t, s, ownerID, a.ID)
	if d.Status != DeploySucceeded {
		t.Fatalf("deployment = %s %q, want succeeded", d.Status, d.Message)
	}
	if got := events.kinds(); len(got) != 1 || got[0] != EventDeploySucceeded {
		t.Fatalf("events = %v, want just %s", got, EventDeploySucceeded)
	}
}

// A redeploy whose new version crashes after coming up fails, and the app
// keeps its old version: it stays the active release, and is what the app
// is converged back to.
func TestARedeployThatCrashesFailsAndKeepsTheOldVersion(t *testing.T) {
	ctx := context.Background()
	events := &recordedEvents{}
	s, base, pool := testService(t, settleOptions(events))
	ownerID := owner(t, s, pool, "rollout-redeploy-crash")
	prior, candidate, claimed := claimedCandidate(t, s, ownerID, "web")
	events.mu.Lock()
	events.events = nil
	events.mu.Unlock()
	s.orch = &rolloutScriptOrchestrator{recordingOrchestrator: base, statuses: []orchestrator.AppStatus{
		healthyStatus(0),
		crashedStatus(1, "code 1 (Error)"),
	}}
	if err := s.executeOperation(ctx, claimed); err == nil {
		t.Fatal("a crashing redeploy succeeded")
	}
	d, err := s.q.GetDeployment(ctx, dbgen.GetDeploymentParams{OwnerID: ownerID, ID: claimed.DeploymentID})
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if d.Status != DeployFailed || !strings.Contains(d.Message, "restarted") {
		t.Fatalf("deployment = %s %q, want failed, saying it restarted", d.Status, d.Message)
	}
	if got := events.kinds(); len(got) != 1 || got[0] != EventDeployCrashed {
		t.Fatalf("events = %v, want just %s", got, EventDeployCrashed)
	}
	app, err := s.Get(ctx, ownerID, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if app.ActiveReleaseID == nil || *app.ActiveReleaseID != *prior.ActiveReleaseID {
		t.Fatal("the crashed release replaced the old one")
	}
	if last := base.lastAppSpec(); last.ReleaseID != candidate.ID.String() {
		t.Fatalf("the cluster was last given %s, want the candidate %s before converging", last.ReleaseID, candidate.ID)
	}
	if err := s.reconcileApp(ctx, app); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if last := base.lastAppSpec(); last.ReleaseID != prior.ActiveReleaseID.String() {
		t.Fatalf("the app was converged to %s, want its old release %s", last.ReleaseID, prior.ActiveReleaseID)
	}
}

// A project's apps carry their latest deployment, so the canvas can say a
// first deploy is under way, or failed, before there is a workload to ask.
func TestProjectAppsCarryTheirLatestDeployment(t *testing.T) {
	ctx := context.Background()
	s, base, pool := testService(t, Options{RolloutTimeout: time.Second, RolloutPollInterval: time.Millisecond})
	ownerID := owner(t, s, pool, "project-last-deploy")
	p, err := s.CreateProject(ctx, ownerID, "Shop")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	a, err := s.Create(ctx, ownerID, CreateInput{Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080, ProjectID: p.ID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	listed := func() App {
		t.Helper()
		apps, err := s.ListInProject(ctx, ownerID, p.ID)
		if err != nil || len(apps) != 1 {
			t.Fatalf("ListInProject = %v, %v", apps, err)
		}
		return apps[0]
	}
	queued := listed()
	if queued.LastDeploy == "" || queued.LastDeploy == DeployFailed || queued.LastDeployID == uuid.Nil {
		t.Fatalf("a queued first deploy lists as %q (%s)", queued.LastDeploy, queued.LastDeployID)
	}
	s.orch = &rolloutScriptOrchestrator{recordingOrchestrator: base, statuses: []orchestrator.AppStatus{{
		Generation: 2, ObservedGeneration: 2, Desired: 1, Terminal: true, Reason: "CrashLoopBackOff",
	}}}
	if err := executeLiveOperation(s, ctx, a); err == nil {
		t.Fatal("the scripted failure succeeded")
	}
	if failed := listed(); failed.LastDeploy != DeployFailed || failed.LastDeployID != queued.LastDeployID {
		t.Fatalf("a failed first deploy lists as %q (%s), want failed (%s)", failed.LastDeploy, failed.LastDeployID, queued.LastDeployID)
	}
}

// Without a health check, ready is listening on the app's port; a version
// that never listens fails its deploy saying which port nothing answered on.
func TestADeployThatNeverListensFailsNamingThePort(t *testing.T) {
	events := &recordedEvents{}
	opts := settleOptions(events)
	opts.RolloutTimeout = 200 * time.Millisecond
	s, base, pool := testService(t, opts)
	ownerID := owner(t, s, pool, "rollout-never-listens")
	a, err := firstDeploy(t, s, base, ownerID, orchestrator.AppStatus{
		Phase: orchestrator.PhasePending, Generation: 2, ObservedGeneration: 2, Desired: 1, Updated: 1,
	})
	if err == nil {
		t.Fatal("a deploy that never became ready succeeded")
	}
	d := deploymentOf(t, s, ownerID, a.ID)
	if d.Status != DeployFailed || !strings.Contains(d.Message, "never answered on port 8080") {
		t.Fatalf("deployment = %s %q, want failed naming port 8080", d.Status, d.Message)
	}
	if !strings.Contains(d.Message, "nothing is serving yet") || !strings.Contains(d.Message, "listen on $PORT (8080)") {
		t.Fatalf("a first deploy's failure = %q, want it to say nothing serves yet and what to listen on", d.Message)
	}
	if got := events.kinds(); len(got) != 1 || got[0] != EventDeployFailed {
		t.Fatalf("events = %v, want just %s", got, EventDeployFailed)
	}
}

// A redeploy whose new pod crash-loops beside the old one, which keeps
// serving: the Deployment's ready and available counts include the old pod,
// and were taken as the new version's. With a health check or without, the
// deploy fails, the old release stays active, and the app is converged back
// to it.
func TestARedeployCrashingBesideTheOldPodFailsAndKeepsTheOldVersion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		healthPath string
	}{{"without a health check", ""}, {"with a health check", "/healthz"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			events := &recordedEvents{}
			// No settle window: the counts alone must not pass the rollout.
			opts := settleOptions(events)
			opts.RolloutSettle = -1
			s, base, pool := testService(t, opts)
			ownerID := owner(t, s, pool, "rollout-beside-old")
			prior, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
				Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
			})
			if err != nil {
				t.Fatalf("first deploy: %v", err)
			}
			// Ready and available: the old pod. Updated: the new one, which
			// is neither, and crashes until its restarts run out.
			beside := orchestrator.AppStatus{
				Phase: orchestrator.PhaseRunning, Generation: 2, ObservedGeneration: 2, Desired: 1,
				Pods: 2, Updated: 1, Ready: 1, Available: 1, AvailableCondition: true,
			}
			crashing := beside
			crashing.Restarts, crashing.LastExit = 1, "code 127 (Error)"
			given := crashing
			given.Restarts, given.Terminal = 3, true
			given.Reason, given.Message = "CrashLoopBackOff", "the container exited 3 times; the last time with code 127 (Error)"
			s.orch = &rolloutScriptOrchestrator{recordingOrchestrator: base,
				statuses: []orchestrator.AppStatus{beside, beside, crashing, given}}
			events.mu.Lock()
			events.events = nil
			events.mu.Unlock()
			if tc.healthPath != "" {
				err = s.SetHealth(ctx, ownerID, "web", tc.healthPath, false)
			} else {
				_, err = s.Update(ctx, ownerID, "web", UpdateInput{Image: "nginx:1.28", Port: 8080})
			}
			if err != nil {
				t.Fatalf("change: %v", err)
			}
			if err := executeNamedOperation(s, ctx, ownerID, "web"); err == nil {
				t.Fatal("a redeploy crashing beside the old pod succeeded")
			}
			d := deploymentOf(t, s, ownerID, prior.ID)
			if d.Status != DeployFailed || !strings.Contains(d.Message, "CrashLoopBackOff") {
				t.Fatalf("deployment = %s %q, want failed with CrashLoopBackOff", d.Status, d.Message)
			}
			if got := events.kinds(); len(got) != 1 || got[0] != EventDeployCrashed {
				t.Fatalf("events = %v, want just %s", got, EventDeployCrashed)
			}
			app, err := s.Get(ctx, ownerID, "web")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if app.ActiveReleaseID == nil || *app.ActiveReleaseID != *prior.ActiveReleaseID {
				t.Fatal("the crashing release replaced the old one")
			}
			if err := s.reconcileApp(ctx, app); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if last := base.lastAppSpec(); last.ReleaseID != prior.ActiveReleaseID.String() {
				t.Fatalf("the app was converged to %s, want its old release %s", last.ReleaseID, prior.ActiveReleaseID)
			}
		})
	}
}

// A release that only scales keeps the running pods (CONFIG-9): the same
// ReplicaSet, so every pod is "updated" from the start and no new revision
// ever comes. It verifies once the wanted replicas are available and have
// settled, rather than waiting for one.
func TestAScaleOnlyReleaseVerifiesOnceTheReplicasAreUp(t *testing.T) {
	ctx := context.Background()
	events := &recordedEvents{}
	s, base, pool := testService(t, settleOptions(events))
	ownerID := owner(t, s, pool, "rollout-scale-only")
	if _, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// The one running pod has restarted before, which is not the scale's.
	scaling := orchestrator.AppStatus{
		Phase: orchestrator.PhaseDegraded, Generation: 3, ObservedGeneration: 3, Desired: 3,
		Pods: 3, Updated: 3, Ready: 1, Available: 1, AvailableCondition: true, Restarts: 2,
	}
	up := scaling
	up.Phase, up.Ready, up.Available = orchestrator.PhaseRunning, 3, 3
	s.orch = &rolloutScriptOrchestrator{recordingOrchestrator: base,
		statuses: []orchestrator.AppStatus{scaling, scaling, up}}
	events.mu.Lock()
	events.events = nil
	events.mu.Unlock()
	if _, err := s.Scale(ctx, ownerID, "web", 3); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	app, err := s.Get(ctx, ownerID, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := executeLiveOperation(s, ctx, app); err != nil {
		t.Fatalf("a scale whose replicas came up: %v", err)
	}
	d := deploymentOf(t, s, ownerID, app.ID)
	if d.Status != DeploySucceeded {
		t.Fatalf("deployment = %s %q, want succeeded", d.Status, d.Message)
	}
	if got := events.kinds(); len(got) != 1 || got[0] != EventDeploySucceeded {
		t.Fatalf("events = %v, want just %s", got, EventDeploySucceeded)
	}
}

// logsOrchestrator is a rollout script whose cluster has two of the app's
// pods, the old version's, ready, and the new version's, which restarted, and
// the output of the new one's last run.
type logsOrchestrator struct {
	*rolloutScriptOrchestrator
	mu    sync.Mutex
	asked []orchestrator.LogOptions
}

func (o *logsOrchestrator) Pods(context.Context, orchestrator.PodListOptions) ([]orchestrator.PodInfo, error) {
	now := time.Now()
	return []orchestrator.PodInfo{
		{Name: "web-old", Namespace: "ns", App: "web", Ready: 1, Total: 1, CreatedAt: now.Add(-time.Hour)},
		{Name: "web-new", Namespace: "ns", App: "web", Ready: 0, Total: 1, Restarts: 2, CreatedAt: now},
		{Name: "db-new", Namespace: "ns", App: "db", Ready: 0, Total: 1, CreatedAt: now.Add(time.Minute)},
	}, nil
}

func (o *logsOrchestrator) Logs(_ context.Context, opts orchestrator.LogOptions) ([]orchestrator.LogLine, error) {
	o.mu.Lock()
	o.asked = append(o.asked, opts)
	o.mu.Unlock()
	return []orchestrator.LogLine{
		{Text: "starting server"},
		{Text: "listening on 0.0.0.0:3000\n"},
	}, nil
}

// A redeploy that never passes its readiness check says what to fix: the
// port datahuru checked and a PORT that disagrees with it, or the health
// check's path; that the previous version is still serving; and how the new
// version last exited and what it printed.
func TestAFailedReadinessCheckSaysWhatToFix(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, s *Service, ctx context.Context, ownerID string)
		want []string
	}{{
		name: "a PORT that is not the port",
		set: func(t *testing.T, s *Service, ctx context.Context, ownerID string) {
			if err := s.SetVariable(ctx, ownerID, "web", VariableInput{Key: "PORT", Value: "3000"}); err != nil {
				t.Fatalf("SetVariable: %v", err)
			}
		},
		want: []string{
			"The new version never answered on port 8080 within the healthcheck timeout of 1s, so the previous version is still serving.",
			"Your PORT variable is 3000, but traffic and the check go to port 8080: remove PORT, or set the port in Settings to 3000",
		},
	}, {
		name: "a health check",
		set: func(t *testing.T, s *Service, ctx context.Context, ownerID string) {
			if err := s.SetHealth(ctx, ownerID, "web", "/healthz", false); err != nil {
				t.Fatalf("SetHealth: %v", err)
			}
		},
		want: []string{
			"The new version's health check at /healthz never passed within the healthcheck timeout of 1s, so the previous version is still serving.",
			"Make your app listen on $PORT (8080), and make /healthz answer with a 2xx once it is up",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, base, pool := testService(t, settleOptions(&recordedEvents{}))
			ownerID := owner(t, s, pool, "rollout-what-to-fix")
			prior, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
				Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
			})
			if err != nil {
				t.Fatalf("first deploy: %v", err)
			}
			if _, err := pool.Exec(ctx, `UPDATE apps SET health_timeout_seconds = 1 WHERE id = $1`, prior.ID); err != nil {
				t.Fatalf("set the healthcheck timeout: %v", err)
			}
			orch := &logsOrchestrator{rolloutScriptOrchestrator: &rolloutScriptOrchestrator{
				recordingOrchestrator: base, statuses: []orchestrator.AppStatus{
					crashedStatus(2, "code 1 (Error)"),
				},
			}}
			s.orch = orch
			tc.set(t, s, ctx, ownerID)
			if err := executeNamedOperation(s, ctx, ownerID, "web"); err == nil {
				t.Fatal("a redeploy that never became ready succeeded")
			}
			d := deploymentOf(t, s, ownerID, prior.ID)
			if d.Status != DeployFailed {
				t.Fatalf("deployment = %s %q, want failed", d.Status, d.Message)
			}
			want := append(tc.want,
				"It last exited with code 1 (Error).",
				"Its last output:\n  starting server\n  listening on 0.0.0.0:3000")
			for _, w := range want {
				if !strings.Contains(d.Message, w) {
					t.Errorf("message = %q\nwant it to contain %q", d.Message, w)
				}
			}
			orch.mu.Lock()
			defer orch.mu.Unlock()
			if len(orch.asked) != 1 || orch.asked[0].Pod != "web-new" || !orch.asked[0].Previous ||
				orch.asked[0].Tail != readinessLogTail {
				t.Fatalf("logs read = %+v, want the new pod's last run, %d lines", orch.asked, readinessLogTail)
			}
		})
	}
}

// A version that never listens, where the cluster's progress deadline, which
// is the deploy's own budget, is the first to say so. Its "ReplicaSet … has
// timed out progressing" won over the message that names the port.
func TestAProgressDeadlineSaysWhatToFix(t *testing.T) {
	events := &recordedEvents{}
	s, base, pool := testService(t, settleOptions(events))
	ownerID := owner(t, s, pool, "rollout-progress-deadline")
	a, err := firstDeploy(t, s, base, ownerID, orchestrator.AppStatus{
		Phase: orchestrator.PhaseFailing, Generation: 2, ObservedGeneration: 2, Desired: 1, Updated: 1,
		Terminal: true, Reason: orchestrator.ReasonProgressDeadlineExceeded,
		Message: `ReplicaSet "web-767d9b44c8" has timed out progressing.`,
	})
	if err == nil {
		t.Fatal("a deploy past its progress deadline succeeded")
	}
	d := deploymentOf(t, s, ownerID, a.ID)
	if d.Status != DeployFailed || strings.Contains(d.Message, "rollout failed") || !strings.Contains(d.Message, "The new version never answered on port 8080") ||
		!strings.Contains(d.Message, "listen on $PORT (8080)") {
		t.Fatalf("deployment = %s %q, want failed naming port 8080, not the cluster's timeout", d.Status, d.Message)
	}
	if got := events.kinds(); len(got) != 1 || got[0] != EventDeployFailed {
		t.Fatalf("events = %v, want just %s", got, EventDeployFailed)
	}
}
