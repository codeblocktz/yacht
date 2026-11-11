package k8s

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// imageDigestRE is a manifest digest as the Dockerfile step prints it.
var imageDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// layoutPathRE is every path in a layout the engine will read: the index,
// and a blob named by its sha256. Nothing else in the layout is opened.
var layoutPathRE = regexp.MustCompile(`^(index\.json|blobs/sha256/[0-9a-f]{64})$`)

// pushBuiltImage pushes the image the Dockerfile step built, digest being the
// manifest it printed, and lets the push step end.
//
// The push is the engine's, through req.PushLayout, because the credential
// is: no step of a Dockerfile build holds it. The layout is read out of the
// push step, a file at a time, as data.
func (o *Orchestrator) pushBuiltImage(ctx context.Context, pod string, req orchestrator.BuildRequest, digest string) error {
	if req.PushLayout == nil {
		return errors.New("k8s: the Dockerfile was built, and nothing was given to push it with")
	}
	if err := o.waitForPushStep(ctx, pod); err != nil {
		return err
	}
	req.Log("Pushing " + req.Image + "\n")
	if err := req.PushLayout(ctx, req.Image, digest, podLayout{o: o, pod: pod}, req.Log); err != nil {
		return fmt.Errorf("k8s: push %s: %w", req.Image, err)
	}
	if err := o.execInPush(ctx, pod, []string{"touch", handedOver}, io.Discard); err != nil {
		return fmt.Errorf("k8s: the image was pushed, and the push step could not be told: %w", err)
	}
	return nil
}

// waitForPushStep waits until the push step is running, which is when there is
// a layout to read.
func (o *Orchestrator) waitForPushStep(ctx context.Context, pod string) error {
	var ended error
	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true,
		func(ctx context.Context) (bool, error) {
			p, err := o.client.CoreV1().Pods(BuildNamespace).Get(ctx, pod, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name != pushContainer {
					continue
				}
				if cs.State.Running != nil {
					return true, nil
				}
				if cs.State.Terminated != nil {
					ended = fmt.Errorf("k8s: the push step ended before the image was collected")
					return true, nil
				}
			}
			if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
				ended = fmt.Errorf("k8s: the build ended before the image was collected")
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("k8s: waiting for the push step: %w", err)
	}
	return ended
}

// execInPush runs a command in the push step, its output to stdout.
func (o *Orchestrator) execInPush(ctx context.Context, pod string, command []string, stdout io.Writer) error {
	run := o.execStream
	if run == nil {
		run = o.streamExec
	}
	var stderr cappedBuffer
	err := run(ctx, BuildNamespace, pod, &corev1.PodExecOptions{
		Container: pushContainer,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
	}, remotecommand.StreamOptions{Stdout: stdout, Stderr: &stderr})
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", command[0], err, msg)
		}
		return fmt.Errorf("%s: %w", command[0], err)
	}
	return nil
}

// podLayout is the layout the Dockerfile step left, read through the push
// step with tar.
//
// tar rather than cat, because tar says what a file is: a symlink is
// archived as a symlink, a directory as a directory, a pipe as a pipe, and
// each is refused here rather than followed, listed, or waited on. The push
// step holds nothing a symlink could lead to, but the layout is the
// repository's to have written, and nothing in it is trusted to be what its
// name says.
type podLayout struct {
	o   *Orchestrator
	pod string
}

func (l podLayout) Open(ctx context.Context, path string, limit int64) (io.ReadCloser, int64, error) {
	if !layoutPathRE.MatchString(path) {
		return nil, 0, fmt.Errorf("k8s: %q is not a file of an image layout", path)
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	go func() {
		err := l.o.execInPush(ctx, l.pod, []string{"tar", "-C", imageLayout, "-cf", "-", path}, pw)
		pw.CloseWithError(err) // nil closes it with io.EOF
	}()
	closeAll := func() {
		cancel()
		_ = pr.Close()
	}

	tr := tar.NewReader(pr)
	hdr, err := tr.Next()
	if err != nil {
		closeAll()
		return nil, 0, fmt.Errorf("k8s: read %s: %w", path, err)
	}
	if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA { //nolint:staticcheck // what an older tar writes
		closeAll()
		return nil, 0, fmt.Errorf("k8s: %s is not a plain file", path)
	}
	if strings.TrimPrefix(hdr.Name, "./") != path {
		closeAll()
		return nil, 0, fmt.Errorf("k8s: asked for %s and was given %s", path, hdr.Name)
	}
	if hdr.Size < 0 || hdr.Size > limit {
		closeAll()
		return nil, 0, fmt.Errorf("k8s: %s is %d bytes, more than %d", path, hdr.Size, limit)
	}
	return &tarEntry{Reader: io.LimitReader(tr, hdr.Size), close: closeAll}, hdr.Size, nil
}

// tarEntry is one file of a tar stream, closing the stream with it.
type tarEntry struct {
	io.Reader
	close func()
}

func (t *tarEntry) Close() error {
	t.close()
	return nil
}

// cappedBuffer keeps the first few kilobytes written to it: enough of a
// failing command's stderr to say why.
type cappedBuffer struct{ b []byte }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := 4096 - len(c.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.b = append(c.b, p[:room]...)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return string(c.b) }
