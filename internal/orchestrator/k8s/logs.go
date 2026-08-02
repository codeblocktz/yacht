package k8s

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"iter"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// maxTail is the most lines this will fetch however many are asked for.
//
// A cap rather than a default: without one, a pod up for a month sends its
// whole history across the wire before anything renders, and the page that
// asked for "all of it" is the page nobody can use.
const maxTail = 2000

// Logs reads a container's output.
//
// Timestamps are requested from the API and split off each line, so the page
// can format them rather than printing whatever the container happened to
// prefix its own output with.
func (o *Orchestrator) Logs(
	ctx context.Context, opts orchestrator.LogOptions,
) ([]orchestrator.LogLine, error) {
	if opts.Namespace == "" || opts.Pod == "" {
		// A wiring mistake rather than a state: every caller resolves an app
		// first, and an empty namespace would read from whatever the client's
		// default happens to be.
		return nil, fmt.Errorf("k8s: logs need a namespace and a pod")
	}

	tail := opts.Tail
	if tail <= 0 || tail > maxTail {
		tail = maxTail
	}

	req := o.client.CoreV1().Pods(opts.Namespace).GetLogs(opts.Pod, &corev1.PodLogOptions{
		Timestamps: true,
		TailLines:  &tail,
		Previous:   opts.Previous,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("k8s: no pod %s/%s: %w",
				opts.Namespace, opts.Pod, orchestrator.ErrNotFound)
		case opts.Previous && apierrors.IsBadRequest(err):
			// "previous terminated container not found" is the ordinary answer
			// for a pod that has never restarted. Empty rather than an error:
			// there is nothing wrong, there is simply nothing to show.
			return nil, nil
		case apierrors.IsBadRequest(err):
			// The other BadRequest here is "is waiting to start", which is a
			// container that could not be created — a bad image reference, a
			// security context the image cannot satisfy, a missing secret. The
			// pod already says which; this must not answer "could not read the
			// logs", because that sends somebody looking for a broken log.
			return nil, fmt.Errorf("%w: %s", orchestrator.ErrNotStarted, err)
		}
		return nil, fmt.Errorf("k8s: read logs %s/%s: %w", opts.Namespace, opts.Pod, err)
	}
	defer stream.Close() //nolint:errcheck // reading is done

	var out []orchestrator.LogLine
	sc := bufio.NewScanner(stream)
	// A container can emit a line longer than the scanner's default, and the
	// default is to stop reading entirely — which would silently truncate the
	// log at whatever line happened to be long.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		out = append(out, parseLogLine(sc.Text()))
	}
	if err := sc.Err(); err != nil {
		// What was read is returned alongside the error. A log that fails
		// halfway is still worth showing up to the point it failed.
		return out, fmt.Errorf("k8s: reading logs %s/%s: %w", opts.Namespace, opts.Pod, err)
	}
	return out, nil
}

// LogStream follows a container's output until ctx is done.
//
// A separate method rather than a Follow flag on Logs: the batch call returns
// []LogLine, and a flag that makes it never return would leave that signature
// telling the truth for one value of one field and lying for the other.
//
// The stream stays open until the caller stops reading or ctx is cancelled, so
// every caller must do one of the two — an abandoned iterator holds a
// connection to the apiserver open.
func (o *Orchestrator) LogStream(
	ctx context.Context, opts orchestrator.LogOptions,
) (iter.Seq2[orchestrator.LogLine, error], error) {
	if opts.Namespace == "" || opts.Pod == "" {
		return nil, fmt.Errorf("k8s: logs need a namespace and a pod")
	}

	tail := opts.Tail
	if tail <= 0 || tail > maxTail {
		tail = maxTail
	}

	// Previous is not offered: a container that has already died writes nothing
	// more, so following it would hold a connection open on a stream that can
	// never produce another line. That view keeps the batch read.
	req := o.client.CoreV1().Pods(opts.Namespace).GetLogs(opts.Pod, &corev1.PodLogOptions{
		Timestamps: true,
		TailLines:  &tail,
		Follow:     true,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("k8s: no pod %s/%s: %w",
				opts.Namespace, opts.Pod, orchestrator.ErrNotFound)
		case apierrors.IsBadRequest(err):
			return nil, fmt.Errorf("%w: %s", orchestrator.ErrNotStarted, err)
		}
		return nil, fmt.Errorf("k8s: stream logs %s/%s: %w", opts.Namespace, opts.Pod, err)
	}

	lines := streamLines(ctx, stream)
	return func(yield func(orchestrator.LogLine, error) bool) {
		// Closed here rather than by the caller: the iterator is the only thing
		// that knows when reading stopped, whether that was EOF, an error, or a
		// consumer that broke out of its range.
		defer stream.Close() //nolint:errcheck // reading is done
		for line, err := range lines {
			if !yield(line, err) {
				return
			}
		}
	}, nil
}

// streamLines yields each line of a log stream as it arrives.
//
// Separate from the reading of the stream so the part with the interesting
// behaviour — yielding as lines arrive, stopping when the caller stops, and a
// stream that breaks midway — can be tested against an ordinary reader. The
// fake clientset answers with a canned string and cannot follow.
func streamLines(ctx context.Context, r io.Reader) iter.Seq2[orchestrator.LogLine, error] {
	return func(yield func(orchestrator.LogLine, error) bool) {
		sc := bufio.NewScanner(r)
		// Same bound as the batch read: a container can emit a line longer than
		// the scanner's default, and the default is to give up reading entirely.
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for sc.Scan() {
			// Checked per line rather than only at the end. A followed stream
			// has no end, so a cancelled reader that only noticed on EOF would
			// never notice at all.
			if ctx.Err() != nil {
				return
			}
			if !yield(parseLogLine(sc.Text()), nil) {
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			// Reported rather than swallowed, and only when the caller did not
			// cause it: a cancelled stream fails on read, and surfacing that as
			// an error would put "connection closed" on the page every time
			// somebody navigates away.
			yield(orchestrator.LogLine{}, err)
		}
	}
}

// parseLogLine splits the RFC3339 timestamp the API prefixes onto each line.
//
// A line whose timestamp will not parse keeps its text whole rather than losing
// its first word: the prefix is only there because it was asked for, and
// guessing wrong should not eat the output.
func parseLogLine(raw string) orchestrator.LogLine {
	stamp, text, found := strings.Cut(raw, " ")
	if !found {
		return orchestrator.LogLine{Text: raw}
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return orchestrator.LogLine{Text: raw}
	}
	return orchestrator.LogLine{At: at, Text: text}
}
