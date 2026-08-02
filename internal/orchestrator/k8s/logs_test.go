package k8s

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// erroringReader serves some lines and then fails, standing in for a log stream
// the apiserver drops partway through.
type erroringReader struct {
	rest string
	err  error
}

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.rest == "" {
		return 0, r.err
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

// Streaming is tested against a reader rather than the fake clientset. The fake
// answers GetLogs with a canned string and cannot follow, so it can say nothing
// about the part that is actually new here: yielding lines as they arrive,
// stopping when the caller stops, and surviving a stream that breaks midway.

func TestStreamLinesYieldsLinesInOrder(t *testing.T) {
	r := strings.NewReader(
		"2026-08-02T10:00:00.000000000Z first\n" +
			"2026-08-02T10:00:01.000000000Z second\n")

	var got []orchestrator.LogLine
	for line, err := range streamLines(t.Context(), r) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, line)
	}

	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}
	if got[0].Text != "first" || got[1].Text != "second" {
		t.Errorf("got %q then %q, want \"first\" then \"second\"", got[0].Text, got[1].Text)
	}
	if !got[0].At.Before(got[1].At) {
		t.Errorf("timestamps not parsed in order: %v then %v", got[0].At, got[1].At)
	}
}

func TestStreamLinesStopsWhenConsumerStops(t *testing.T) {
	r := strings.NewReader(
		"2026-08-02T10:00:00.000000000Z first\n" +
			"2026-08-02T10:00:01.000000000Z second\n" +
			"2026-08-02T10:00:02.000000000Z third\n")

	seen := 0
	for range streamLines(t.Context(), r) {
		seen++
		break
	}

	if seen != 1 {
		t.Errorf("consumer broke after one line but iterator produced %d", seen)
	}
}

func TestStreamLinesStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := strings.NewReader(
		"2026-08-02T10:00:00.000000000Z first\n" +
			"2026-08-02T10:00:01.000000000Z second\n" +
			"2026-08-02T10:00:02.000000000Z third\n")

	seen := 0
	for range streamLines(ctx, r) {
		seen++
		cancel() // a reader who navigated away after the first line
	}

	if seen != 1 {
		t.Errorf("context cancelled after one line but iterator produced %d", seen)
	}
}

func TestStreamLinesSurfacesAMidStreamFailure(t *testing.T) {
	boom := errors.New("connection reset")
	r := &erroringReader{rest: "2026-08-02T10:00:00.000000000Z first\n", err: boom}

	var lines []orchestrator.LogLine
	var gotErr error
	for line, err := range streamLines(t.Context(), r) {
		if err != nil {
			gotErr = err
			continue
		}
		lines = append(lines, line)
	}

	if len(lines) != 1 {
		t.Errorf("got %d lines before the failure, want 1", len(lines))
	}
	if !errors.Is(gotErr, boom) {
		t.Errorf("got error %v, want it to wrap %v", gotErr, boom)
	}
}

func TestStreamLinesStaysQuietWhenCancellationCausedTheFailure(t *testing.T) {
	// A cancelled stream fails on read. Reporting that would put "connection
	// closed" on the page every time somebody navigates away from a log.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &erroringReader{rest: "", err: io.ErrUnexpectedEOF}

	for _, err := range streamLines(ctx, r) {
		if err != nil {
			t.Fatalf("reported %v for a stream the caller cancelled", err)
		}
	}
}

func TestLogStreamNeedsANamespaceAndPod(t *testing.T) {
	o, _ := testOrchestrator(t)

	if _, err := o.LogStream(t.Context(), orchestrator.LogOptions{Pod: "web-1"}); err == nil {
		t.Error("streamed with no namespace — a pod name alone reads any tenant's output")
	}
	if _, err := o.LogStream(t.Context(), orchestrator.LogOptions{Namespace: "ns"}); err == nil {
		t.Error("streamed with no pod")
	}
}

func TestLogStreamReadsTheContainer(t *testing.T) {
	o, _ := testOrchestrator(t)

	seq, err := o.LogStream(t.Context(), orchestrator.LogOptions{Namespace: "ns", Pod: "web-1"})
	if err != nil {
		t.Fatalf("LogStream: %v", err)
	}

	var lines []orchestrator.LogLine
	for line, err := range seq {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		t.Fatal("stream produced nothing from a pod the fake serves output for")
	}
}
