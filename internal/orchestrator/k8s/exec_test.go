package k8s

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// consolePod is a running pod of one of the engine's apps, labelled the way
// applyDeployment labels its pod template.
func consolePod(ns, app, owner, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Labels: map[string]string{
				orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
				orchestrator.LabelApp:       app,
				orchestrator.LabelOwner:     owner,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: app}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  app,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

// execCall is what reached the stand-in for the apiserver's exec stream.
type execCall struct {
	namespace, pod string
	opts           *corev1.PodExecOptions
	streams        remotecommand.StreamOptions
}

// execHarness is an orchestrator over the fake clientset whose exec stream is
// recorded rather than dialled.
func execHarness(t *testing.T, pods ...*corev1.Pod) (*Orchestrator, *[]execCall) {
	t.Helper()
	client := fake.NewClientset()
	for _, p := range pods {
		if _, err := client.CoreV1().Pods(p.Namespace).Create(t.Context(), p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod %s: %v", p.Name, err)
		}
	}
	o := NewWithClient(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls []execCall
	o.execStream = func(
		_ context.Context, ns, pod string,
		opts *corev1.PodExecOptions, streams remotecommand.StreamOptions,
	) error {
		calls = append(calls, execCall{namespace: ns, pod: pod, opts: opts, streams: streams})
		return nil
	}
	return o, &calls
}

var webRef = orchestrator.Ref{Owner: "owner-1", Namespace: "yacht-owner-1-web", Name: "web"}

func TestExecOpensAShellInTheAppsOwnContainer(t *testing.T) {
	o, calls := execHarness(t, consolePod(webRef.Namespace, "web", "owner-1", "web-abc12"))

	err := o.Exec(t.Context(), orchestrator.ExecOptions{
		Ref: webRef, Pod: "web-abc12",
		Stdin: strings.NewReader(""), Stdout: io.Discard,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("%d exec streams opened, want 1", len(*calls))
	}
	c := (*calls)[0]
	if c.namespace != webRef.Namespace || c.pod != "web-abc12" {
		t.Errorf("exec in %s/%s, want %s/web-abc12", c.namespace, c.pod, webRef.Namespace)
	}
	if c.opts.Container != "web" {
		t.Errorf("container = %q, want the app's own", c.opts.Container)
	}
	if !slices.Equal(c.opts.Command, orchestrator.ShellCommand) {
		t.Errorf("command = %q, want the shell fallback %q", c.opts.Command, orchestrator.ShellCommand)
	}
	// A terminal: input, one merged output, and a TTY so line editing and job
	// control work. Stderr alongside a TTY is refused by the kubelet.
	if !c.opts.Stdin || !c.opts.Stdout || c.opts.Stderr || !c.opts.TTY {
		t.Errorf("exec options = %+v, want stdin, stdout and a TTY without stderr", c.opts)
	}
	if !c.streams.Tty || c.streams.Stdin == nil || c.streams.Stdout == nil {
		t.Errorf("streams = %+v, want a TTY with both ends attached", c.streams)
	}
}

// The pod name arrives from a request. Every way of pointing it somewhere other
// than one of this app's running pods must be refused before a stream opens.
func TestExecRefusesAPodThatIsNotTheApps(t *testing.T) {
	sibling := consolePod(webRef.Namespace, "worker", "owner-1", "worker-1")
	stranger := consolePod(webRef.Namespace, "web", "owner-2", "web-impostor")
	unmanaged := consolePod(webRef.Namespace, "web", "owner-1", "web-handmade")
	delete(unmanaged.Labels, orchestrator.LabelManagedBy)
	elsewhere := consolePod("yacht-owner-2-db", "web", "owner-1", "web-elsewhere")
	build := consolePod(webRef.Namespace, "web", "owner-1", "build-1")
	build.Labels = map[string]string{
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		orchestrator.LabelOwner:     "owner-1",
		"yacht/build":               "true",
	}

	o, calls := execHarness(t, sibling, stranger, unmanaged, elsewhere, build)

	for _, tc := range []struct{ why, pod string }{
		{"another app's pod in the same namespace", "worker-1"},
		{"a pod labelled for another owner", "web-impostor"},
		{"a pod the engine did not create", "web-handmade"},
		{"a pod of this app's name in another namespace", "web-elsewhere"},
		{"a build pod", "build-1"},
		{"a pod that does not exist", "web-nope"},
		{"a name that is a path", "../../kube-system/pods/etcd"},
		{"no name at all", ""},
	} {
		t.Run(tc.why, func(t *testing.T) {
			err := o.Exec(t.Context(), orchestrator.ExecOptions{Ref: webRef, Pod: tc.pod})
			if !errors.Is(err, orchestrator.ErrNotFound) {
				t.Errorf("Exec(%q) = %v, want ErrNotFound", tc.pod, err)
			}
		})
	}
	if len(*calls) != 0 {
		t.Fatalf("an exec stream was opened into %s/%s", (*calls)[0].namespace, (*calls)[0].pod)
	}
}

// A container that is not running has nothing to exec into. Said as "not
// started" rather than as a failure, so the console can say what to do.
func TestExecRefusesAContainerThatIsNotRunning(t *testing.T) {
	pending := consolePod(webRef.Namespace, "web", "owner-1", "web-pending")
	pending.Status = corev1.PodStatus{Phase: corev1.PodPending}
	crashing := consolePod(webRef.Namespace, "web", "owner-1", "web-crashing")
	crashing.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
	}

	o, calls := execHarness(t, pending, crashing)
	for _, pod := range []string{"web-pending", "web-crashing"} {
		err := o.Exec(t.Context(), orchestrator.ExecOptions{Ref: webRef, Pod: pod})
		if !errors.Is(err, orchestrator.ErrNotStarted) {
			t.Errorf("Exec(%s) = %v, want ErrNotStarted", pod, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatal("an exec stream was opened into a container that is not running")
	}
}

// Typing exit 1 is how a shell ends, not a failure of the console.
func TestAShellExitingNonZeroIsAnOrdinaryEnd(t *testing.T) {
	o, _ := execHarness(t, consolePod(webRef.Namespace, "web", "owner-1", "web-abc12"))
	o.execStream = func(context.Context, string, string, *corev1.PodExecOptions, remotecommand.StreamOptions) error {
		return utilexec.CodeExitError{Err: errors.New("command terminated with exit code 130"), Code: 130}
	}
	if err := o.Exec(t.Context(), orchestrator.ExecOptions{Ref: webRef, Pod: "web-abc12"}); err != nil {
		t.Fatalf("Exec after exit 130 = %v, want nil", err)
	}
}

// The fake clientset has nothing to stream from, and the real stream needs the
// connection New keeps. Without one, Exec says it is unsupported rather than
// dereferencing a missing config.
func TestExecWithoutAStreamingConnectionIsUnsupported(t *testing.T) {
	client := fake.NewClientset(consolePod(webRef.Namespace, "web", "owner-1", "web-abc12"))
	o := NewWithClient(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := o.Exec(t.Context(), orchestrator.ExecOptions{Ref: webRef, Pod: "web-abc12"})
	if !errors.Is(err, orchestrator.ErrNotSupported) {
		t.Fatalf("Exec = %v, want ErrNotSupported", err)
	}
}

// Sizes reach the executor in order, and the queue ends — rather than blocking
// forever — once the terminal has gone.
func TestTheSizeQueueEndsWithTheTerminal(t *testing.T) {
	sizes := make(chan orchestrator.TerminalSize, 2)
	sizes <- orchestrator.TerminalSize{Cols: 80, Rows: 24}
	sizes <- orchestrator.TerminalSize{Cols: 120, Rows: 40}
	close(sizes)

	q := sizeQueue{ctx: t.Context(), sizes: sizes}
	if got := q.Next(); got == nil || got.Width != 80 || got.Height != 24 {
		t.Fatalf("first size = %+v, want 80x24", got)
	}
	if got := q.Next(); got == nil || got.Width != 120 || got.Height != 40 {
		t.Fatalf("second size = %+v, want 120x40", got)
	}
	if got := q.Next(); got != nil {
		t.Fatalf("after the channel closed, Next = %+v, want nil", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := (sizeQueue{ctx: ctx, sizes: make(chan orchestrator.TerminalSize)}).Next(); got != nil {
		t.Fatalf("after the context ended, Next = %+v, want nil", got)
	}
}
