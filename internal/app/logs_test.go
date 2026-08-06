package app

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// testLogger discards. These tests assert on what the cluster was asked to do,
// not on what was written about it.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// loggingOrchestrator records which pod was asked for, so a test can prove the
// scoping rather than only that a call happened.
type loggingOrchestrator struct {
	*recordingOrchestrator
	pods []orchestrator.PodInfo
	last orchestrator.LogOptions
}

func (l *loggingOrchestrator) Pods(
	_ context.Context, opts orchestrator.PodListOptions,
) ([]orchestrator.PodInfo, error) {
	var out []orchestrator.PodInfo
	for _, p := range l.pods {
		if opts.Namespace == "" || p.Namespace == opts.Namespace {
			out = append(out, p)
		}
	}
	return out, nil
}

func (l *loggingOrchestrator) Logs(
	_ context.Context, opts orchestrator.LogOptions,
) ([]orchestrator.LogLine, error) {
	l.last = opts
	return []orchestrator.LogLine{{Text: "hello from " + opts.Pod}}, nil
}

// A pod name is enough to read any tenant's output, so it is checked against
// the pods the app actually has rather than trusted.
//
// Without this, "?pod=" on the logs URL reads any container in the cluster —
// including another team's database, which prints its own credentials on start.
func TestAPodNameFromTheRequestIsNotTrusted(t *testing.T) {
	ctx := context.Background()
	orch := &loggingOrchestrator{recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}}
	s, _, pool := testService(t, Options{})
	s.orch = orch
	ownerID := owner(t, s, pool, "owner-logs")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orch.pods = []orchestrator.PodInfo{
		{Name: "web-1", Namespace: a.Namespace},
		// Somebody else's, in a namespace this app does not own.
		{Name: "victim-db-1", Namespace: "yacht-someone-else"},
	}

	if _, err := s.Logs(ctx, ownerID, "web", LogRequest{Pod: "victim-db-1"}); err != ErrNotFound {
		t.Fatalf("asking for another app's pod = %v, want ErrNotFound", err)
	}
	if orch.last.Pod == "victim-db-1" {
		t.Fatal("another app's pod was read")
	}
}

// The namespace comes from the app row, never from the caller.
func TestLogsAreReadFromTheAppsOwnNamespace(t *testing.T) {
	ctx := context.Background()
	orch := &loggingOrchestrator{recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}}
	s, _, pool := testService(t, Options{})
	s.orch = orch
	ownerID := owner(t, s, pool, "owner-logs-ns")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orch.pods = []orchestrator.PodInfo{{Name: "web-1", Namespace: a.Namespace}}

	got, err := s.Logs(ctx, ownerID, "web", LogRequest{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if orch.last.Namespace != a.Namespace {
		t.Errorf("read from namespace %q, want the app's own %q", orch.last.Namespace, a.Namespace)
	}
	if got.Pod != "web-1" {
		t.Errorf("pod = %q, want the app's only pod", got.Pod)
	}
	if len(got.Lines) != 1 || !strings.Contains(got.Lines[0].Text, "web-1") {
		t.Errorf("lines = %v, want the pod's output", got.Lines)
	}
}

// Another team cannot read an app that is not theirs, because Get refuses
// before any pod is listed.
func TestAnotherTeamCannotReadYourLogs(t *testing.T) {
	ctx := context.Background()
	orch := &loggingOrchestrator{recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}}
	s, _, pool := testService(t, Options{})
	s.orch = orch
	mine := owner(t, s, pool, "owner-logs-mine")
	theirs := owner(t, s, pool, "owner-logs-theirs")

	if _, err := s.Create(ctx, mine, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.Logs(ctx, theirs, "web", LogRequest{}); err != ErrNotFound {
		t.Fatalf("another team reading the logs = %v, want ErrNotFound", err)
	}
	if orch.last.Pod != "" {
		t.Fatal("a pod was read for a team that does not own the app")
	}
}

// An empty result is explained rather than left looking like a fault.
func TestNothingToShowSaysWhy(t *testing.T) {
	ctx := context.Background()
	orch := &loggingOrchestrator{recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}}
	s, _, pool := testService(t, Options{})
	s.orch = orch
	ownerID := owner(t, s, pool, "owner-logs-empty")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// No pods at all.
	got, err := s.Logs(ctx, ownerID, "web", LogRequest{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if got.Note == "" {
		t.Error("an empty log pane says nothing about why it is empty")
	}
}

// A deployment ends up saying how it went, and the one it replaced says it was
// replaced.
//
// This shipped with FinishDeployment written and never called, so every row sat
// at "running" for ever and the history read as a list of deploys still in
// progress. Nothing failed and nothing looked broken — the page was simply
// describing a state that had not been true since the moment it was written.
func TestDeploymentHistoryKeepsTerminalAttemptsAndOneActivePointer(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-deploy-history")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	if _, err := s.Scale(ctx, ownerID, "web", 2); err != nil {
		t.Fatalf("Scale: %v", err)
	}

	deps, err := s.Deployments(ctx, ownerID, a.ID, 20)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("history has %d entries, want 3", len(deps))
	}

	var active, running int
	for _, d := range deps {
		if d.IsActive {
			active++
		}
		if d.Status == DeployRunning {
			running++
		}
		if d.Status != DeploySucceeded {
			t.Errorf("deployment status = %q, want terminal succeeded", d.Status)
		}
		if d.FinishedAt == nil {
			t.Error("a completed attempt has no finish time")
		}
	}
	if active != 1 {
		t.Errorf("%d deployments are active, want exactly the newest", active)
	}
	if running != 0 {
		t.Errorf("%d deployments are still marked running after finishing", running)
	}

	// Newest first, and it is the live one.
	if !deps[0].IsActive {
		t.Error("newest successful release is not the active pointer")
	}
	for _, d := range deps[1:] {
		if d.IsActive {
			t.Error("an earlier release is also marked active")
		}
	}
}

// A superseded deployment's logs are gone, and saying so is the feature.
//
// The failure this rules out is not a crash. Reading the current pods and
// captioning them with an old revision would render perfectly and be the live
// log — somebody would draw conclusions about a deploy from output that
// postdates it by a week.
func TestASupersededDeploymentDoesNotShowTheLiveLog(t *testing.T) {
	ctx := context.Background()
	orch := &loggingOrchestrator{recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}}
	s, _, pool := testService(t, Options{})
	s.orch = orch
	ownerID := owner(t, s, pool, "owner-deploy-logs")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orch.pods = []orchestrator.PodInfo{{Name: "web-1", Namespace: a.Namespace}}
	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}

	deps, err := s.Deployments(ctx, ownerID, a.ID, 10)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	var live, old Deployment
	for _, d := range deps {
		if d.IsActive {
			live = d
		} else {
			old = d
		}
	}
	if live.ID == uuid.Nil || old.ID == uuid.Nil {
		t.Fatalf("expected one active and one superseded, got %+v", deps)
	}

	orch.last = orchestrator.LogOptions{}
	got, err := s.DeploymentLogs(ctx, ownerID, "web", old.ID, LogRequest{})
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if got.Live {
		t.Error("a superseded deployment reports itself as live")
	}
	if len(got.Logs.Lines) != 0 {
		t.Fatalf("a replaced deployment returned %d lines of somebody else's run", len(got.Logs.Lines))
	}
	if orch.last.Pod != "" {
		t.Error("the cluster was read for a deployment whose containers are gone")
	}
	if got.Logs.Note == "" {
		t.Error("nothing explains why there are no logs")
	}

	// And the live one does read.
	got, err = s.DeploymentLogs(ctx, ownerID, "web", live.ID, LogRequest{})
	if err != nil {
		t.Fatalf("DeploymentLogs live: %v", err)
	}
	if !got.Live || len(got.Logs.Lines) == 0 {
		t.Error("the running deployment shows no output")
	}
}

// A deployment id from another app is not a way to read across apps.
func TestADeploymentIDFromAnotherAppIsRefused(t *testing.T) {
	ctx := context.Background()
	orch := &loggingOrchestrator{recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}}
	s, _, pool := testService(t, Options{})
	s.orch = orch
	ownerID := owner(t, s, pool, "owner-deploy-cross")

	one, err := s.Create(ctx, ownerID, CreateInput{
		Name: "one", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create one: %v", err)
	}
	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "two", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("Create two: %v", err)
	}

	deps, err := s.Deployments(ctx, ownerID, one.ID, 10)
	if err != nil || len(deps) == 0 {
		t.Fatalf("Deployments: %v", err)
	}

	if _, err := s.DeploymentLogs(ctx, ownerID, "two", deps[0].ID, LogRequest{}); err != ErrNotFound {
		t.Fatalf("reading one app's deployment through another = %v, want ErrNotFound", err)
	}
}

func (l *loggingOrchestrator) LogStream(
	_ context.Context, opts orchestrator.LogOptions,
) (iter.Seq2[orchestrator.LogLine, error], error) {
	l.last = opts
	return func(yield func(orchestrator.LogLine, error) bool) {
		yield(orchestrator.LogLine{Text: "streamed from " + opts.Pod}, nil)
	}, nil
}

// The streaming read is a second door onto the same output, so it needs the
// same lock. A guard applied to one path and not the other is the bug the guard
// was written to prevent.
func TestAPodNameIsNotTrustedWhenStreamingEither(t *testing.T) {
	ctx := context.Background()
	orch := &loggingOrchestrator{recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}}
	s, _, pool := testService(t, Options{})
	s.orch = orch
	ownerID := owner(t, s, pool, "owner-stream")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orch.pods = []orchestrator.PodInfo{
		{Name: "web-1", Namespace: a.Namespace},
		{Name: "victim-db-1", Namespace: "yacht-someone-else"},
	}

	if _, err := s.LogStream(ctx, ownerID, "web", LogRequest{Pod: "victim-db-1"}); err != ErrNotFound {
		t.Fatalf("streaming another app's pod = %v, want ErrNotFound", err)
	}
	if orch.last.Pod == "victim-db-1" {
		t.Fatal("another app's pod was streamed")
	}
}

// The namespace comes from the app row on this path too.
func TestLogStreamReadsFromTheAppsOwnNamespace(t *testing.T) {
	ctx := context.Background()
	orch := &loggingOrchestrator{recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}}
	s, _, pool := testService(t, Options{})
	s.orch = orch
	ownerID := owner(t, s, pool, "owner-stream-ns")

	a, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orch.pods = []orchestrator.PodInfo{{Name: "web-1", Namespace: a.Namespace}}

	seq, err := s.LogStream(ctx, ownerID, "web", LogRequest{})
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}
	for range seq { //nolint:revive // draining is the point
	}

	if orch.last.Namespace != a.Namespace {
		t.Errorf("streamed from namespace %q, want %q", orch.last.Namespace, a.Namespace)
	}
	if orch.last.Previous {
		t.Error("a followed stream asked for the previous container, which can never produce a line")
	}
}

// accessLogOrchestrator is a cluster whose ingress access log can be switched.
//
// It counts the switch rather than only recording it, because "on by default"
// and "turned on at every boot" are the same on the first run and different
// forever after: the second restarts the ingress controller each time the
// control plane restarts.
type accessLogOrchestrator struct {
	*recordingOrchestrator
	on        bool
	enables   int
	supported bool
}

func (a *accessLogOrchestrator) HTTPLogs(
	context.Context, orchestrator.HTTPLogOptions,
) (orchestrator.HTTPLogs, error) {
	return orchestrator.HTTPLogs{}, nil
}

func (a *accessLogOrchestrator) HTTPLogHint() string { return "kind: HelmChartConfig" }

func (a *accessLogOrchestrator) HTTPLogsEnabled(context.Context) bool { return a.on }

func (a *accessLogOrchestrator) EnableHTTPLogs(context.Context) error {
	a.enables++
	if !a.supported {
		return orchestrator.ErrNotSupported
	}
	a.on = true
	return nil
}

// Request logging is on by default rather than something to find and switch on.
//
// An app whose traffic is not recorded is an app nobody can debug, and the
// previous behaviour put that behind a button on one app's page that restarted
// the whole cluster's ingress controller — a cluster-wide cost presented as a
// per-app choice.
func TestRequestLoggingIsTurnedOnWithoutBeingAsked(t *testing.T) {
	orch := &accessLogOrchestrator{
		recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()},
		supported:             true,
	}
	s := &Service{orch: orch, log: testLogger()}

	s.EnsureHTTPLogs(context.Background())

	if !orch.on {
		t.Fatal("request logging is still off after startup")
	}
}

// And on again at the next boot, which would restart the ingress controller
// every time the control plane restarted.
func TestRequestLoggingIsNotTurnedOnTwice(t *testing.T) {
	orch := &accessLogOrchestrator{
		recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()},
		supported:             true,
		on:                    true,
	}
	s := &Service{orch: orch, log: testLogger()}

	s.EnsureHTTPLogs(context.Background())

	if orch.enables != 0 {
		t.Errorf("reconfigured an already-logging controller %d times, "+
			"restarting every app in the cluster for nothing", orch.enables)
	}
}

// A cluster whose ingress controller Yacht did not install is not a failure to
// boot. It keeps running and the HTTP tab hands over the configuration.
func TestAControllerYachtCannotConfigureIsNotFatal(t *testing.T) {
	orch := &accessLogOrchestrator{
		recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()},
	}
	s := &Service{orch: orch, log: testLogger()}

	s.EnsureHTTPLogs(context.Background())

	if orch.on {
		t.Error("reported logging as on where it could not be configured")
	}
}
