package web

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
)

func TestDeploymentsRenderThePointerRatherThanTheNewestAttemptAsActive(t *testing.T) {
	releaseID := uuid.New()
	body := renderToString(t, AppDeploymentsBody(AppDetailData{
		App: app.App{Name: "web", ActiveReleaseID: &releaseID},
		Deployments: []app.Deployment{
			{Image: "new-failed-image", Revision: "new-failed", Status: app.DeployFailed, StartedAt: time.Now()},
			{Revision: "old-live", Status: app.DeploySucceeded, IsActive: true, StartedAt: time.Now()},
		},
	}))
	for _, want := range []string{"active release", "old-live", "new-failed-image", "History"} {
		if !strings.Contains(body, want) {
			t.Errorf("deployment body missing %q", want)
		}
	}
	if strings.Index(body, "old-live") > strings.Index(body, "new-failed-image") {
		t.Error("the newest failed attempt was rendered as the active panel")
	}
}

func TestDeploymentsExplainBaselineStatesWithoutInventingAnActiveAttempt(t *testing.T) {
	releaseID := uuid.New()
	baseline := renderToString(t, AppDeploymentsBody(AppDetailData{
		App: app.App{Name: "legacy", ActiveReleaseID: &releaseID},
		Deployments: []app.Deployment{
			{Revision: "legacy", Status: app.DeploySucceeded, StartedAt: time.Now()},
		},
	}))
	if !strings.Contains(baseline, "Active baseline release") ||
		strings.Contains(baseline, "active release</span>") {
		t.Fatalf("baseline without linked history was misrepresented: %s", baseline)
	}

	missing := renderToString(t, AppDeploymentsBody(AppDetailData{
		App: app.App{Name: "missing", BaselineError: "manifest no longer exists"},
	}))
	for _, want := range []string{"No baseline release yet", "manifest no longer exists"} {
		if !strings.Contains(missing, want) {
			t.Errorf("missing-baseline body missing %q", want)
		}
	}
}
