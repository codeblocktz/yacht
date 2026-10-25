package k8s

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Compile-time check that the cluster offers a console.
var _ orchestrator.Execer = (*Orchestrator)(nil)

// streamFunc runs one exec against the apiserver.
//
// A seam for tests rather than an option anybody configures: the fake
// clientset has no streaming endpoint, so without this the only part of Exec
// a unit test could reach is the part before the interesting one.
type streamFunc func(
	ctx context.Context, namespace, pod string,
	opts *corev1.PodExecOptions, streams remotecommand.StreamOptions,
) error

// Exec opens an interactive shell in one of an app's pods.
//
// The pod is looked up in the app's own namespace and must carry the app's
// labels — managed by the engine, this app's name, this owner. A name alone is
// never enough: it arrived from a request, and it is all anybody needs to reach
// a container that is not theirs. A pod that fails the check is reported as
// not found, the same as one that does not exist, so the answer does not
// confirm what is running in somebody else's namespace.
func (o *Orchestrator) Exec(ctx context.Context, opts orchestrator.ExecOptions) error {
	pod, err := o.execTarget(ctx, opts.Ref, opts.Pod)
	if err != nil {
		return err
	}

	execOpts := &corev1.PodExecOptions{
		// The app's own container, named after the app. Sidecars a wrapping
		// application might add are not the app, and not what somebody opening
		// its console expects to land in.
		Container: opts.Ref.Name,
		Command:   orchestrator.ShellCommand,
		Stdin:     true,
		Stdout:    true,
		// A TTY has no separate error stream: the terminal merges them, and
		// asking for one alongside a TTY is refused by the kubelet.
		Stderr: false,
		TTY:    true,
	}
	streams := remotecommand.StreamOptions{
		Stdin:  opts.Stdin,
		Stdout: opts.Stdout,
		Tty:    true,
	}
	if opts.Resize != nil {
		streams.TerminalSizeQueue = sizeQueue{ctx: ctx, sizes: opts.Resize}
	}

	run := o.execStream
	if run == nil {
		run = o.streamExec
	}
	err = run(ctx, pod.Namespace, pod.Name, execOpts, streams)

	// A shell that exits non-zero is still a shell that ended the way shells
	// do. `exit 130` after a Ctrl-C at the prompt is the ordinary case, and
	// reporting it as a failure would put an error in front of somebody who
	// did nothing wrong.
	var exit utilexec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("k8s: exec in %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// execTarget resolves a pod a console may open, and is the whole of the check.
func (o *Orchestrator) execTarget(
	ctx context.Context, ref orchestrator.Ref, name string,
) (*corev1.Pod, error) {
	if err := ref.Validate(); err != nil {
		return nil, fmt.Errorf("k8s: exec: %w", err)
	}
	// Checked before it reaches a URL. A name with a slash in it is not a pod
	// name, and the apiserver's path is not where to find that out.
	if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		return nil, fmt.Errorf("k8s: exec: no pod %q: %w", name, orchestrator.ErrNotFound)
	}

	pod, err := o.client.CoreV1().Pods(ref.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("k8s: exec: no pod %s/%s: %w",
				ref.Namespace, name, orchestrator.ErrNotFound)
		}
		return nil, fmt.Errorf("k8s: exec: read pod %s/%s: %w", ref.Namespace, name, err)
	}

	// The namespace is the app's, but a namespace is not the boundary on its
	// own: a build, or anything an operator put there by hand, can share it.
	// The labels are what say this pod is this app, run for this owner.
	l := pod.Labels
	if pod.Namespace != ref.Namespace ||
		l[orchestrator.LabelManagedBy] != orchestrator.ManagedByValue ||
		l[orchestrator.LabelApp] != ref.Name ||
		l[orchestrator.LabelOwner] != string(ref.Owner) {
		return nil, fmt.Errorf("k8s: exec: no pod %s/%s for %s: %w",
			ref.Namespace, name, ref.Name, orchestrator.ErrNotFound)
	}

	if pod.Status.Phase != corev1.PodRunning {
		return nil, fmt.Errorf("%w: pod %s is %s", orchestrator.ErrNotStarted,
			name, cmpPhase(pod.Status.Phase))
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != ref.Name {
			continue
		}
		if cs.State.Running == nil {
			return nil, fmt.Errorf("%w: the %s container in %s is not running",
				orchestrator.ErrNotStarted, ref.Name, name)
		}
		return pod, nil
	}
	return nil, fmt.Errorf("%w: %s has no %s container running",
		orchestrator.ErrNotStarted, name, ref.Name)
}

// cmpPhase names a pod phase for a message, including the empty one a pod has
// before the kubelet has reported anything.
func cmpPhase(p corev1.PodPhase) string {
	if p == "" {
		return "not reported yet"
	}
	return string(p)
}

// streamExec is the real stream: WebSocket first, SPDY where that fails.
//
// The same pairing kubectl makes. Kubernetes is moving exec to WebSockets and
// newer apiservers prefer it, but an older one — or a proxy in between that
// does not pass the upgrade — only speaks SPDY, and an install that worked
// last month must not lose its console to a protocol preference.
func (o *Orchestrator) streamExec(
	ctx context.Context, namespace, pod string,
	opts *corev1.PodExecOptions, streams remotecommand.StreamOptions,
) error {
	if o.restConfig == nil {
		return fmt.Errorf("%w: this orchestrator was built without a cluster "+
			"connection that can stream", orchestrator.ErrNotSupported)
	}

	u := o.client.CoreV1().RESTClient().Post().
		Namespace(namespace).
		Resource("pods").
		Name(pod).
		SubResource("exec").
		VersionedParams(opts, scheme.ParameterCodec).
		URL()

	ws, err := remotecommand.NewWebSocketExecutor(o.restConfig, http.MethodGet, u.String())
	if err != nil {
		return fmt.Errorf("websocket executor: %w", err)
	}
	spdy, err := remotecommand.NewSPDYExecutor(o.restConfig, http.MethodPost, u)
	if err != nil {
		return fmt.Errorf("spdy executor: %w", err)
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return fmt.Errorf("fallback executor: %w", err)
	}
	return exec.StreamWithContext(ctx, streams)
}

// sizeQueue hands terminal sizes to the executor as they arrive.
//
// Next blocks, which is what the executor expects: it calls it in a loop for
// the life of the stream. Returning nil is how it is told to stop, so a
// cancelled context and a closed channel both end the loop rather than leaving
// a goroutine waiting on a terminal that has gone.
type sizeQueue struct {
	ctx   context.Context
	sizes <-chan orchestrator.TerminalSize
}

func (q sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case <-q.ctx.Done():
		return nil
	case s, ok := <-q.sizes:
		if !ok {
			return nil
		}
		return &remotecommand.TerminalSize{Width: s.Cols, Height: s.Rows}
	}
}
