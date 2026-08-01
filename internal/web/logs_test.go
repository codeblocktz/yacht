package web

import (
	"context"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// renderPanel returns the HTML one deployment's log sheet produces.
func renderPanel(t *testing.T, d DeployLogsData) string {
	t.Helper()
	var b strings.Builder
	if err := DeployLogPanel(d).Render(context.Background(), &b); err != nil {
		t.Fatalf("render deploy log panel: %v", err)
	}
	return b.String()
}

// The log sheet has to carry the app's tabs, because it covers them.
//
// This is not a layering nicety. The sheet is a modal dialog, so while it is
// open the rest of the document is inert: a tab left behind it is not merely
// hidden, it cannot be clicked at all. Without these the only way out of the
// logs is to close the sheet first.
func TestTheLogSheetCarriesTheAppsTabs(t *testing.T) {
	html := renderPanel(t, DeployLogsData{
		App: "web",
		Deploy: app.DeployLogs{
			Deployment: app.Deployment{Status: app.DeployActive, Image: "nginx:alpine"},
			Live:       true,
			Logs:       app.Logs{Lines: []orchestrator.LogLine{{Text: "listening"}}},
		},
	})

	for _, href := range []string{
		`href="/apps/web"`,
		`href="/apps/web/variables"`,
		`href="/apps/web/metrics"`,
		`href="/apps/web/storage"`,
		`href="/apps/web/settings"`,
	} {
		if !strings.Contains(html, href) {
			t.Errorf("the log sheet has no way to reach %s", href)
		}
	}
}

// A superseded deployment still has to offer the way out.
//
// The empty state returns early, and an early return that skips the tabs
// leaves the one panel with nothing to read stuck with nowhere to go.
func TestAReplacedDeploymentsSheetStillHasTheTabs(t *testing.T) {
	html := renderPanel(t, DeployLogsData{
		App: "web",
		Deploy: app.DeployLogs{
			Deployment: app.Deployment{Status: app.DeploySuperseded},
			Logs:       app.Logs{Note: "This deployment has been replaced."},
		},
	})

	if !strings.Contains(html, `href="/apps/web/settings"`) {
		t.Error("a replaced deployment's sheet has no tabs")
	}
	if !strings.Contains(html, "This deployment has been replaced.") {
		t.Error("nothing explains the empty pane")
	}
}

// The count is written once.
//
// plural already carries the number, so pairing it with a separate count
// rendered "61 61 lines" — read as two figures rather than one repeated.
func TestTheLineCountIsNotWrittenTwice(t *testing.T) {
	html := renderPanel(t, DeployLogsData{
		App: "web",
		Deploy: app.DeployLogs{
			Deployment: app.Deployment{Status: app.DeployActive},
			Live:       true,
			Logs: app.Logs{Lines: []orchestrator.LogLine{
				{Text: "one"}, {Text: "two"}, {Text: "three"},
			}},
		},
	})

	if strings.Contains(html, "3 3 lines") {
		t.Error("the line count is rendered twice")
	}
	if !strings.Contains(html, "3 lines") {
		t.Error("the line count is missing")
	}
}
