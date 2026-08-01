package k8s

import (
	"bufio"
	"context"
	"fmt"
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
