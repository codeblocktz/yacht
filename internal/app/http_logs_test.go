package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// controllerLog is an ingress controller whose current log file began at
// from: everything before was rotated away, as the kubelet does every 10 MiB.
type controllerLog struct {
	*recordingOrchestrator
	lines []orchestrator.HTTPLogLine
	from  time.Time
	asked int
}

func (c *controllerLog) HTTPLogs(
	context.Context, orchestrator.HTTPLogOptions,
) (orchestrator.HTTPLogs, error) {
	c.asked++
	return orchestrator.HTTPLogs{Lines: c.lines, From: c.from}, nil
}

func (c *controllerLog) HTTPLogHint() string                  { return "" }
func (c *controllerLog) HTTPLogsEnabled(context.Context) bool { return true }
func (c *controllerLog) EnableHTTPLogs(context.Context) error { return nil }

// storedRequests is the log store's copy of the access log.
type storedRequests struct {
	lines      []orchestrator.HTTPLogLine
	more       bool
	err        error
	hosts      []string
	start, end time.Time
}

func (s *storedRequests) Requests(
	_ context.Context, hosts []string, start, end time.Time, _ int,
) ([]orchestrator.HTTPLogLine, bool, error) {
	s.hosts, s.start, s.end = hosts, start, end
	return s.lines, s.more, s.err
}

func httpLogsService(t *testing.T) (*Service, *controllerLog, string, App) {
	t.Helper()
	s, orch, pool := testService(t, Options{AppDomain: "apps.httplog.test", WildcardTLS: true})
	id := owner(t, s, pool, "owner-httplog")
	if _, err := createAndDeploy(t, s, context.Background(), id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	a := publicApp(t, s, id, "web")
	ctl := &controllerLog{recordingOrchestrator: orch}
	s.orch = ctl
	return s, ctl, id, a
}

// QA OBSERVE-1: the controller's log had been rotated at 19:40:16, so the
// 140 requests before were gone, and the page counted the 10 after as if
// they were all. Whatever is read, the page is told where it begins.
func TestHTTPLogsFromTheControllerSayWhereTheyBegin(t *testing.T) {
	s, ctl, id, a := httpLogsService(t)
	rotated := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
	ctl.from = rotated
	ctl.lines = []orchestrator.HTTPLogLine{{At: rotated.Add(time.Second), Host: a.Host, Method: "GET", Path: "/", Status: 200}}

	got, err := s.DeploymentHTTPLogs(context.Background(), id, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(rotated) {
		t.Errorf("Since = %v, want %v, where the controller's log begins", got.Since, rotated)
	}
	if !strings.Contains(got.Coverage, rotated.Format("15:04:05")+" UTC") {
		t.Errorf("Coverage = %q, does not say the requests begin at %s", got.Coverage, rotated.Format("15:04:05"))
	}
}

// With a log store, an app's requests are read from it, for the app's own
// hosts, back a day or the store's retention, whichever is shorter.
func TestHTTPLogsAreReadFromTheLogStoreWhenThereIsOne(t *testing.T) {
	s, ctl, id, a := httpLogsService(t)
	old := time.Now().UTC().Add(-2 * time.Hour)
	store := &storedRequests{lines: []orchestrator.HTTPLogLine{
		{At: old, Host: a.Host, Method: "GET", Path: "/before-the-rotation", Status: 500},
	}}
	s.UseRequestLog(store, 6*time.Hour)

	got, err := s.DeploymentHTTPLogs(context.Background(), id, "web")
	if err != nil {
		t.Fatal(err)
	}
	if ctl.asked != 0 {
		t.Error("the controller's pod log was read beside the store")
	}
	if len(got.Lines) != 1 || got.Lines[0].Path != "/before-the-rotation" {
		t.Errorf("Lines = %+v, want the store's", got.Lines)
	}
	if !slices.Equal(store.hosts, []string{a.Host}) {
		t.Errorf("the store was asked for hosts %v, want only the app's %q", store.hosts, a.Host)
	}
	if span := store.end.Sub(store.start); span != 6*time.Hour {
		t.Errorf("the store was asked for %v, want the store's retention of 6h", span)
	}
	if got.Coverage != "Every request in the last 6 hours." {
		t.Errorf("Coverage = %q", got.Coverage)
	}

	// More than a page holds: the page says where the newest begin.
	store.more = true
	got, err = s.DeploymentHTTPLogs(context.Background(), id, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(old) || !strings.Contains(got.Coverage, "The newest 1 requests, since ") {
		t.Errorf("Since %v, Coverage %q; want the oldest line's time", got.Since, got.Coverage)
	}
}

// A store that cannot be read now is covered by what the controller still
// has, said as such, rather than by an error page.
func TestHTTPLogsFallBackToTheControllerWhenTheStoreFails(t *testing.T) {
	s, ctl, id, a := httpLogsService(t)
	ctl.from = time.Now().UTC().Add(-time.Minute)
	ctl.lines = []orchestrator.HTTPLogLine{{At: time.Now().UTC(), Host: a.Host, Method: "GET", Path: "/", Status: 200}}
	s.UseRequestLog(&storedRequests{err: errors.New("connection refused")}, 0)

	got, err := s.DeploymentHTTPLogs(context.Background(), id, "web")
	if err != nil {
		t.Fatal(err)
	}
	if ctl.asked != 1 || len(got.Lines) != 1 || !strings.Contains(got.Coverage, "controller's log begins") {
		t.Errorf("asked the controller %d times; got %+v", ctl.asked, got)
	}
}
