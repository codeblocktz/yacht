package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// consoleOrchestrator lists the pods it is given and records every shell it is
// asked to open, echoing what was typed back to the terminal.
type consoleOrchestrator struct {
	orchestrator.Orchestrator
	pods []orchestrator.PodInfo

	mu    sync.Mutex
	execs []orchestrator.ExecOptions
}

func (c *consoleOrchestrator) Pods(
	_ context.Context, opts orchestrator.PodListOptions,
) ([]orchestrator.PodInfo, error) {
	var out []orchestrator.PodInfo
	for _, p := range c.pods {
		if opts.Namespace == "" || p.Namespace == opts.Namespace {
			out = append(out, p)
		}
	}
	return out, nil
}

func (c *consoleOrchestrator) Exec(_ context.Context, opts orchestrator.ExecOptions) error {
	c.mu.Lock()
	c.execs = append(c.execs, opts)
	c.mu.Unlock()
	if opts.Stdin != nil && opts.Stdout != nil {
		_, err := io.Copy(opts.Stdout, opts.Stdin)
		return err
	}
	return nil
}

func (c *consoleOrchestrator) opened() []orchestrator.ExecOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]orchestrator.ExecOptions(nil), c.execs...)
}

// consoleService is a service over a consoleOrchestrator, with one app
// deployed for ownerID.
func consoleService(t *testing.T, ownerID string) (*Service, *consoleOrchestrator, App) {
	t.Helper()
	s, rec, pool := testService(t, Options{})
	id := owner(t, s, pool, ownerID)
	a, err := createAndDeploy(t, s, context.Background(), id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 2, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orch := &consoleOrchestrator{Orchestrator: rec}
	s.orch = orch
	return s, orch, a
}

func TestAConsoleOpensInTheAppsFirstRunningPod(t *testing.T) {
	ctx := context.Background()
	s, orch, a := consoleService(t, "owner-console")
	orch.pods = []orchestrator.PodInfo{
		{Name: "web-0", Namespace: a.Namespace, App: "web", Phase: "Pending"},
		{Name: "web-1", Namespace: a.Namespace, App: "web", Phase: "Running"},
		{Name: "web-2", Namespace: a.Namespace, App: "web", Phase: "Running"},
	}

	sess, err := s.Console(ctx, a.OwnerID, "web", ConsoleRequest{Actor: "eric@web.test"})
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	if sess.Pod() != "web-1" {
		t.Errorf("pod = %q, want the first running one", sess.Pod())
	}
	if got := strings.Join(sess.Pods(), ","); got != "web-0,web-1,web-2" {
		t.Errorf("pods = %s, want every pod of the app for the picker", got)
	}

	var out bytes.Buffer
	if err := sess.Run(ctx, Terminal{Stdin: strings.NewReader("ls\n"), Stdout: &out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	execs := orch.opened()
	if len(execs) != 1 {
		t.Fatalf("%d shells opened, want 1", len(execs))
	}
	if execs[0].Ref != a.Ref() || execs[0].Pod != "web-1" {
		t.Errorf("shell opened in %v/%s, want %v/web-1", execs[0].Ref, execs[0].Pod, a.Ref())
	}
	if out.String() != "ls\n" {
		t.Errorf("terminal shows %q, want the relayed bytes", out.String())
	}
}

// A pod name from the request is honoured only if it is one of this app's.
// Anything else is a way into another app's container.
func TestAConsoleRefusesAPodThatIsNotTheApps(t *testing.T) {
	ctx := context.Background()
	s, orch, a := consoleService(t, "owner-console-pod")
	orch.pods = []orchestrator.PodInfo{
		{Name: "web-1", Namespace: a.Namespace, App: "web", Phase: "Running"},
		// A sibling sharing the namespace, and somebody else's database.
		{Name: "worker-1", Namespace: a.Namespace, App: "worker", Phase: "Running"},
		{Name: "victim-db-1", Namespace: "yacht-someone-else", App: "db", Phase: "Running"},
	}

	for _, pod := range []string{"worker-1", "victim-db-1", "web-9"} {
		if _, err := s.Console(ctx, a.OwnerID, "web", ConsoleRequest{Pod: pod}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Console(pod=%s) = %v, want ErrNotFound", pod, err)
		}
	}
	if len(orch.opened()) != 0 {
		t.Fatal("a shell was opened in a pod that is not the app's")
	}

	sess, err := s.Console(ctx, a.OwnerID, "web", ConsoleRequest{Pod: "web-1"})
	if err != nil || sess.Pod() != "web-1" {
		t.Fatalf("Console(pod=web-1) = %v, %v; want the app's own pod", sess, err)
	}
}

// Another team's app is not found, before any pod is listed.
func TestAnotherTeamCannotOpenYourConsole(t *testing.T) {
	ctx := context.Background()
	s, orch, a := consoleService(t, "owner-console-mine")
	theirs := owner(t, s, s.pool, "owner-console-theirs")
	orch.pods = []orchestrator.PodInfo{{Name: "web-1", Namespace: a.Namespace, App: "web", Phase: "Running"}}

	if _, err := s.Console(ctx, theirs, "web", ConsoleRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another team's Console = %v, want ErrNotFound", err)
	}
	if _, err := s.Console(ctx, theirs, "web", ConsoleRequest{Pod: "web-1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another team naming the pod = %v, want ErrNotFound", err)
	}
}

func TestAConsoleNeedsARunningPod(t *testing.T) {
	ctx := context.Background()
	s, orch, a := consoleService(t, "owner-console-none")
	orch.pods = []orchestrator.PodInfo{{Name: "web-1", Namespace: a.Namespace, App: "web", Phase: "Pending"}}

	if _, err := s.Console(ctx, a.OwnerID, "web", ConsoleRequest{}); !errors.Is(err, ErrConsoleNoPods) {
		t.Fatalf("Console with nothing running = %v, want ErrConsoleNoPods", err)
	}
}

// A sleeping app runs nothing, and opening a console is not a reason to wake
// it: the person is told to, and nothing is opened.
func TestAConsoleIsRefusedWhileTheAppSleeps(t *testing.T) {
	ctx := context.Background()
	s, sleepy, pool := sleepService(t, Options{Waker: testWaker})
	id := owner(t, s, pool, "owner-console-sleep")
	a := sleepyApp(t, s, id, "sleepy-console")
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	orch := &consoleOrchestrator{Orchestrator: sleepy, pods: []orchestrator.PodInfo{
		// Listed anyway, so the refusal is the sleep and not an empty list.
		{Name: "sleepy-console-1", Namespace: a.Namespace, App: a.Name, Phase: "Running"},
	}}
	s.orch = orch

	if _, err := s.Console(ctx, id, a.Name, ConsoleRequest{}); !errors.Is(err, ErrConsoleAsleep) {
		t.Fatalf("Console while asleep = %v, want ErrConsoleAsleep", err)
	}
	if len(orch.opened()) != 0 {
		t.Fatal("a shell was opened in a sleeping app")
	}
}

// An orchestrator with no exec at all is a console that is not offered.
func TestAConsoleNeedsAnOrchestratorThatCanExec(t *testing.T) {
	ctx := context.Background()
	s, _, a := consoleService(t, "owner-console-noexec")
	// The interface alone, so nothing beneath it can be asserted for.
	s.orch = struct{ orchestrator.Orchestrator }{orchestrator.NewNoop()}

	if _, err := s.Console(ctx, a.OwnerID, "web", ConsoleRequest{}); !errors.Is(err, ErrConsoleUnavailable) {
		t.Fatalf("Console = %v, want ErrConsoleUnavailable", err)
	}
}

// The record says who opened which pod and for how long, and never a byte of
// what crossed the terminal.
func TestAConsoleIsRecordedWithoutWhatWasTyped(t *testing.T) {
	ctx := context.Background()
	s, orch, a := consoleService(t, "owner-console-audit")
	orch.pods = []orchestrator.PodInfo{{Name: "web-1", Namespace: a.Namespace, App: "web", Phase: "Running"}}
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))

	sess, err := s.Console(ctx, a.OwnerID, "web", ConsoleRequest{Actor: "eric@web.test"})
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	const secret = "export TOKEN=hunter2-very-secret"
	if err := sess.Run(ctx, Terminal{Stdin: strings.NewReader(secret + "\n"), Stdout: io.Discard}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := logged.String()
	for _, want := range []string{"console opened", "console closed", "actor=eric@web.test", "app=web", "pod=web-1", "duration="} {
		if !strings.Contains(got, want) {
			t.Errorf("the record is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hunter2") {
		t.Fatalf("what was typed reached the log:\n%s", got)
	}
}
