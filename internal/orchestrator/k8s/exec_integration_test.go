package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// syncBuffer is a bytes.Buffer the exec stream can write to while the test
// reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Opt-in like the rollout test: the fake clientset has no exec stream, so the
// only proof that a shell opens — over WebSocket or SPDY, as the restricted
// user, resized — is a real apiserver. Point it at a disposable K3s cluster
// with YACHT_TEST_K3S=1.
func TestK3sOpensAShellInAnAppsContainer(t *testing.T) {
	if os.Getenv("YACHT_TEST_K3S") == "" {
		t.Skip("set YACHT_TEST_K3S=1 to run against the current disposable K3s cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	o, err := New(ctx, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect to K3s: %v", err)
	}
	namespace := fmt.Sprintf("yacht-console-%d", time.Now().UnixNano())
	ref := orchestrator.Ref{Owner: "console-test", Namespace: namespace, Name: "web"}
	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: ref.Owner, Name: namespace}); err != nil {
		t.Fatalf("ensure namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := o.DeleteNamespace(cleanup, namespace); err != nil {
			t.Errorf("delete integration namespace: %v", err)
		}
	})

	if err := o.ApplyApp(ctx, orchestrator.AppSpec{
		Ref: ref, Image: "nginxinc/nginx-unprivileged:1.27-alpine",
		Replicas: 1, Port: 8080, RunAsUser: 101,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	waitForStatus(t, ctx, o, ref, func(s orchestrator.AppStatus) bool {
		return s.Available == 1
	})
	pods, err := o.Pods(ctx, orchestrator.PodListOptions{Namespace: namespace, Owner: ref.Owner})
	if err != nil || len(pods) != 1 {
		t.Fatalf("pods = %v, %v; want one", pods, err)
	}
	pod := pods[0].Name

	// The shell is fed a script and its output read back. Resizing first, so
	// the size the shell reports is the one the terminal asked for.
	stdin, typed := io.Pipe()
	var out syncBuffer
	sizes := make(chan orchestrator.TerminalSize, 1)
	sizes <- orchestrator.TerminalSize{Cols: 101, Rows: 37}
	done := make(chan error, 1)
	go func() {
		done <- o.Exec(ctx, orchestrator.ExecOptions{
			Ref: ref, Pod: pod, Stdin: stdin, Stdout: &out, Resize: sizes,
		})
	}()

	waitFor := func(what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !strings.Contains(out.String(), what) {
			if time.Now().After(deadline) {
				t.Fatalf("waited for %q; the terminal shows:\n%s", what, out.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// A marker the shell has to compute, so the echo of the typed line cannot
	// satisfy the wait on its own.
	fmt.Fprint(typed, "echo uid=$(id -u) size=$(stty size) marker-$((40+2))\n")
	waitFor("marker-42")
	if !strings.Contains(out.String(), "uid=101") {
		t.Errorf("the shell is not the container's own user:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "size=37 101") {
		t.Errorf("the terminal size did not reach the shell:\n%s", out.String())
	}
	// Exec does not escalate: the restricted profile forbids it, and the
	// shell has no way back to root.
	fmt.Fprint(typed, "grep NoNewPrivs /proc/self/status | tr -s '\\t ' '='; echo nnp-done\n")
	waitFor("nnp-done")
	if !strings.Contains(out.String(), "NoNewPrivs:=1") {
		t.Errorf("the shell may gain privileges:\n%s", out.String())
	}

	fmt.Fprint(typed, "exit 3\n")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a shell exiting non-zero = %v, want nil", err)
		}
	case <-ctx.Done():
		t.Fatal("the shell never exited")
	}

	// A pod of this name in another namespace, or a name that is not this
	// app's, is refused against a real apiserver too.
	other := orchestrator.Ref{Owner: ref.Owner, Namespace: "kube-system", Name: "web"}
	if err := o.Exec(ctx, orchestrator.ExecOptions{Ref: other, Pod: pod}); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Errorf("exec with another namespace = %v, want ErrNotFound", err)
	}
	if err := o.Exec(ctx, orchestrator.ExecOptions{Ref: ref, Pod: "coredns"}); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Errorf("exec into a pod that is not the app's = %v, want ErrNotFound", err)
	}
}
