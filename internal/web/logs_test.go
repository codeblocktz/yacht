package web

import (
	"context"
	"iter"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// testDeployID is the deployment recordingLogs answers for.
var testDeployID = uuid.MustParse("11111111-2222-3333-4444-555555555555")

// recordingLogs reports whether container output was actually fetched.
//
// The distinction the empty views turn on is not what renders — it is whether
// the cluster was read at all.
type recordingLogs struct {
	readLogs bool
	stream   []string
	build    *app.Build
	http     *app.HTTPLogs
	enabled  bool
}

func (l *recordingLogs) Logs(
	context.Context, string, string, app.LogRequest,
) (app.Logs, error) {
	l.readLogs = true
	return app.Logs{}, nil
}

func (l *recordingLogs) DeploymentLogs(
	context.Context, string, string, uuid.UUID, app.LogRequest,
) (app.DeployLogs, error) {
	l.readLogs = true
	return app.DeployLogs{
		Deployment: app.Deployment{ID: testDeployID, Status: app.DeployActive},
		Live:       true,
	}, nil
}

// BuildForDeployment answers with the build this fake was given, so a test can
// drive the Build logs tab without a builder.
func (l *recordingLogs) BuildForDeployment(
	context.Context, string, uuid.UUID,
) (app.Build, error) {
	if l.build == nil {
		return app.Build{}, app.ErrNotFound
	}
	return *l.build, nil
}

// DeploymentHTTPLogs answers with whatever the test set, so the HTTP tab can
// be driven without an ingress controller.
func (l *recordingLogs) DeploymentHTTPLogs(
	context.Context, string, string,
) (app.HTTPLogs, error) {
	if l.http == nil {
		return app.HTTPLogs{}, nil
	}
	return *l.http, nil
}

// EnableHTTPLogs records that the cluster-wide switch was thrown.
func (l *recordingLogs) EnableHTTPLogs(context.Context) error {
	l.enabled = true
	return nil
}

func (l *recordingLogs) Deployment(
	context.Context, string, string, uuid.UUID,
) (app.Deployment, error) {
	return app.Deployment{ID: testDeployID, Status: app.DeployActive}, nil
}

// liveDeploy is a running deployment with output, the case every view is
// measured against.
func liveDeploy() DeployLogsData {
	return DeployLogsData{
		App:  "web",
		View: viewDeploy,
		Deploy: app.DeployLogs{
			Deployment: app.Deployment{ID: testDeployID, Status: app.DeployActive},
			Live:       true,
			Logs:       app.Logs{Lines: []orchestrator.LogLine{{Text: "listening"}}},
		},
	}
}

// renderPanel returns the HTML one deployment's log sheet produces.
func renderPanel(t *testing.T, d DeployLogsData) string {
	t.Helper()
	var b strings.Builder
	if err := DeployLogPanel(d).Render(context.Background(), &b); err != nil {
		t.Fatalf("render deploy log panel: %v", err)
	}
	return b.String()
}

// The sheet offers a log per thing that could have written one.
//
// Deploy, build and HTTP are three different failures — never started, never
// built, started and serving errors — and an empty pane is only useful if it
// says which of those it is answering for.
func TestTheSheetOffersEachLog(t *testing.T) {
	html := renderPanel(t, liveDeploy())

	for _, want := range []string{"Deploy logs", "Build logs", "HTTP logs"} {
		if !strings.Contains(html, want) {
			t.Errorf("no %q tab", want)
		}
	}
	for _, href := range []string{
		"/logs?view=deploy", "/logs?view=build", "/logs?view=http",
	} {
		if !strings.Contains(html, href) {
			t.Errorf("no way to reach %s", href)
		}
	}
}

// An empty log says why it is empty.
//
// A blank pane under a tab reads as a failure to load. These two are empty
// because nothing wrote them, which is a different thing and worth saying:
// there is no build to log, and no access log to read.
func TestAnEmptyLogSaysWhyItIsEmpty(t *testing.T) {
	for _, tc := range []struct{ view, want string }{
		{viewBuild, "Nothing was built"},
		{viewHTTP, "No requests recorded"},
	} {
		d := liveDeploy()
		d.View = tc.view
		html := renderPanel(t, d)

		if !strings.Contains(html, tc.want) {
			t.Errorf("the %s log does not explain itself", tc.view)
		}
		// And it does not print the deploy log under another log's heading.
		if strings.Contains(html, "listening") {
			t.Errorf("the %s log shows the container's output", tc.view)
		}
	}
}

// The build and HTTP views do not read the cluster.
//
// There is nothing there for them. A version that fetched and threw the result
// away would render identically, so the check is on the call, not the output.
func TestTheEmptyLogsDoNotReadTheCluster(t *testing.T) {
	logs := &recordingLogs{}
	h := testServer(t, Options{Logs: logs})

	for _, view := range []string{viewBuild, viewHTTP} {
		logs.readLogs = false
		w := get(t, h,
			"/apps/web/deployments/"+testDeployID.String()+"/logs?view="+view)

		if w.Code != http.StatusOK {
			t.Fatalf("%s log: status %d", view, w.Code)
		}
		if logs.readLogs {
			t.Errorf("the %s log read container output", view)
		}
	}
}

// The count is written once.
//
// plural already carries the number, so pairing it with a separate count
// rendered "61 61 lines" — read as two figures rather than one repeated.
func TestTheLineCountIsNotWrittenTwice(t *testing.T) {
	d := liveDeploy()
	d.Deploy.Logs.Lines = []orchestrator.LogLine{
		{Text: "one"}, {Text: "two"}, {Text: "three"},
	}
	html := renderPanel(t, d)

	if strings.Contains(html, "3 3 lines") {
		t.Error("the line count is rendered twice")
	}
	if !strings.Contains(html, "3 lines") {
		t.Error("the line count is missing")
	}
}

// A build's log survives the deployment that produced it.
//
// This is the tab's whole reason for existing: container output dies with the
// container, but a build is bounded and stored, so a deployment replaced weeks
// ago can still answer what compiled it.
func TestTheBuildLogIsShownForAReplacedDeployment(t *testing.T) {
	logs := &recordingLogs{build: &app.Build{
		Status: app.BuildSucceeded, RepoURL: "https://github.com/you/app.git",
		RepoRef: "main", CommitSHA: "4bec5f8b07ffc4d16eb6354f7dc5ee6d56122cc4",
		Log: "Cloning\nStep 1/3\nPushed\n",
	}}
	h := testServer(t, Options{Logs: logs})

	w := get(t, h, "/apps/web/deployments/"+testDeployID.String()+"/logs?view=build")
	body := w.Body.String()

	if !strings.Contains(body, "Step 1/3") {
		t.Error("the stored build log is not shown")
	}
	if !strings.Contains(body, "4bec5f8") {
		t.Error("the commit that was built is not shown")
	}
	// And it did not read the cluster to produce any of that.
	if logs.readLogs {
		t.Error("the build view read container output")
	}
}

// An app nobody built says so rather than showing an empty pane.
func TestAnAppWithNoBuildSaysSo(t *testing.T) {
	h := testServer(t, Options{Logs: &recordingLogs{}})

	body := get(t, h, "/apps/web/deployments/"+testDeployID.String()+"/logs?view=build").
		Body.String()
	if !strings.Contains(body, "Nothing was built") {
		t.Error("an image-sourced deployment does not explain the empty build tab")
	}
}

// A failed build shows why on the tab, not only in the log.
func TestAFailedBuildShowsItsReason(t *testing.T) {
	h := testServer(t, Options{Logs: &recordingLogs{build: &app.Build{
		Status:  app.BuildFailed,
		Message: "the dockerfile step exited 1",
		Log:     "ERROR: failed to solve\n",
	}}})

	body := get(t, h, "/apps/web/deployments/"+testDeployID.String()+"/logs?view=build").
		Body.String()
	if !strings.Contains(body, "the dockerfile step exited 1") {
		t.Error("the failure reason is not shown")
	}
	if !strings.Contains(body, "failed") {
		t.Error("the build is not marked failed")
	}
}

// The HTTP tab shows real requests when the controller recorded any.
func TestTheHTTPTabShowsRecordedRequests(t *testing.T) {
	h := testServer(t, Options{Logs: &recordingLogs{http: &app.HTTPLogs{
		Hosts: []string{"web.apps.example.com"},
		Lines: []orchestrator.HTTPLogLine{
			{Method: "GET", Path: "/health", Status: 200, Duration: 2 * time.Millisecond},
			{Method: "POST", Path: "/orders", Status: 500, Duration: 41 * time.Millisecond},
		},
	}}})

	body := get(t, h, "/apps/web/deployments/"+testDeployID.String()+"/logs?view=http").
		Body.String()

	for _, want := range []string{"/health", "/orders", "200", "500", "web.apps.example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("the request log does not show %q", want)
		}
	}
}

// When the access log is switched off, the page says so and hands over the
// configuration rather than leaving somebody to work out why it is empty.
func TestAnUnrecordedIngressHandsOverTheConfiguration(t *testing.T) {
	h := testServer(t, Options{Logs: &recordingLogs{http: &app.HTTPLogs{
		Hosts: []string{"web.apps.example.com"},
		Note:  "The ingress controller is running but is not writing an access log",
		Hint:  "kind: HelmChartConfig",
	}}})

	body := get(t, h, "/apps/web/deployments/"+testDeployID.String()+"/logs?view=http").
		Body.String()

	if !strings.Contains(body, "not writing an access log") {
		t.Error("nothing says why there are no requests")
	}
	if !strings.Contains(body, "HelmChartConfig") {
		t.Error("the configuration that would fix it is not offered")
	}
}

// The panel offers to turn logging on where Yacht can do it.
func TestTheHTTPTabOffersToTurnLoggingOn(t *testing.T) {
	logs := &recordingLogs{http: &app.HTTPLogs{
		Hosts:     []string{"web.apps.example.com"},
		Note:      "not writing an access log",
		Hint:      "kind: HelmChartConfig",
		CanEnable: true,
	}}
	h := testServer(t, Options{Logs: logs})

	body := get(t, h, "/apps/web/deployments/"+testDeployID.String()+"/logs?view=http").
		Body.String()
	if !strings.Contains(body, "Turn on request logging") {
		t.Fatal("no way to turn it on")
	}
	// The restart is the cost, and it is cluster-wide. Somebody clicking this
	// from one app's page must not learn that afterwards.
	if !strings.Contains(body, "restarts the ingress controller") {
		t.Error("the restart is not mentioned")
	}
	if !strings.Contains(body, "whole cluster") {
		t.Error("the blast radius is not mentioned")
	}
}

// Where it cannot, it hands over the configuration instead.
func TestWhereYachtCannotConfigureItSaysSo(t *testing.T) {
	h := testServer(t, Options{Logs: &recordingLogs{http: &app.HTTPLogs{
		Hosts: []string{"web.apps.example.com"},
		Note:  "not writing an access log",
		Hint:  "kind: HelmChartConfig",
	}}})

	body := get(t, h, "/apps/web/deployments/"+testDeployID.String()+"/logs?view=http").
		Body.String()
	if strings.Contains(body, "Turn on request logging") {
		t.Error("it offers to do something it cannot do")
	}
	if !strings.Contains(body, "HelmChartConfig") {
		t.Error("the configuration is not handed over")
	}
}

func (l *recordingLogs) LogStream(
	context.Context, string, string, app.LogRequest,
) (iter.Seq2[orchestrator.LogLine, error], error) {
	l.readLogs = true
	return func(yield func(orchestrator.LogLine, error) bool) {
		for _, text := range l.stream {
			if !yield(orchestrator.LogLine{At: time.Unix(0, 0), Text: text}, nil) {
				return
			}
		}
	}, nil
}

// The stream is an event stream, not a page. A proxy or a browser that reads it
// as anything else buffers it, and a buffered live log is a slow batch one.
func TestLogStreamAnnouncesItselfAsAnEventStream(t *testing.T) {
	logs := &recordingLogs{stream: []string{"first", "second"}}
	h := testServer(t, Options{Logs: logs})

	rec := get(t, h, "/apps/web/logs/stream")

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	// nginx buffers proxied responses by default, which would hold every line
	// until the stream ended — and a followed stream does not end.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
}

func TestLogStreamSendsEachLineAsAnEvent(t *testing.T) {
	logs := &recordingLogs{stream: []string{"first", "second"}}
	h := testServer(t, Options{Logs: logs})

	body := get(t, h, "/apps/web/logs/stream").Body.String()

	for _, want := range []string{"first", "second"} {
		if !strings.Contains(body, "data: ") || !strings.Contains(body, want) {
			t.Errorf("stream body %q missing an event for %q", body, want)
		}
	}
	if strings.Count(body, "data: ") != 2 {
		t.Errorf("got %d events, want 2 — one per line", strings.Count(body, "data: "))
	}
}

// A line with a newline in it would otherwise end the event early and the rest
// would arrive as a field the client does not understand, silently losing it.
func TestLogStreamEscapesNewlinesWithinALine(t *testing.T) {
	logs := &recordingLogs{stream: []string{"panic:\nstack trace"}}
	h := testServer(t, Options{Logs: logs})

	body := get(t, h, "/apps/web/logs/stream").Body.String()

	// One event, not two: a blank line is what ends an event, and the log line
	// contains none.
	if n := strings.Count(body, "id: "); n != 1 {
		t.Errorf("one log line became %d events: %q", n, body)
	}
	// The real failure this guards is a fragment arriving as a bare line. SSE
	// reads such a line as a field name it does not know and drops it, so the
	// second half of a stack trace would vanish with nothing reporting it.
	for _, l := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n") {
		if l == "" || strings.HasPrefix(l, "data: ") || strings.HasPrefix(l, "id: ") {
			continue
		}
		t.Errorf("line %q is not a field, so a client would drop it", l)
	}
	if !strings.Contains(body, "panic:") || !strings.Contains(body, "stack trace") {
		t.Errorf("event lost part of the line: %q", body)
	}
}
