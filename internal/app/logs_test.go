package app

import (
	"context"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

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
