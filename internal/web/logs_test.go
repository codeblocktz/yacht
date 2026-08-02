package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
type recordingLogs struct{ readLogs bool }

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
		{viewHTTP, "Requests are not recorded"},
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
