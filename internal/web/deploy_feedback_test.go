package web

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
)

// The placeholder image is for the cluster, not for a person.
//
// A git app has no image until its first build finishes, and until then every
// surface that showed one showed the literal string
// yacht.invalid/not-built-yet:pending. The constant's own comment anticipates
// it reaching a cluster; it was reaching the app list, the canvas card and the
// deployment rows.
func TestThePendingImagePlaceholderIsNeverShown(t *testing.T) {
	if got := imageLabel(app.PendingImage); got != "building…" {
		t.Errorf("imageLabel(pending) = %q", got)
	}
	if strings.Contains(imageLabel(app.PendingImage), "yacht.invalid") {
		t.Error("the placeholder leaks to the page")
	}
	if !imageIsPending(app.PendingImage) {
		t.Error("a pending image is not recognised as pending")
	}

	// A real image is untouched.
	if got := imageLabel("nginx:alpine"); got != "nginx:alpine" {
		t.Errorf("imageLabel(real) = %q", got)
	}
	if imageIsPending("nginx:alpine") {
		t.Error("a real image is treated as pending")
	}
}

// What decides whether the deployments panel keeps watching. A build keeps its
// deployment running for as long as it takes, which is the case the polling
// exists for.
func TestDeploymentsInFlight(t *testing.T) {
	running := AppDetailData{Deployments: []app.Deployment{
		{Status: app.DeployRunning}, {Status: app.DeployActive},
	}}
	if !deploymentsInFlight(running) {
		t.Error("a running deployment is not treated as in flight")
	}

	settled := AppDetailData{Deployments: []app.Deployment{
		{Status: app.DeployActive}, {Status: app.DeployFailed},
		{Status: app.DeploySuperseded},
	}}
	if deploymentsInFlight(settled) {
		t.Error("a finished history is still being watched")
	}

	// Failed is finished. A page that kept polling a failed deploy would never
	// stop, because nothing is going to change it.
	if deploymentsInFlight(AppDetailData{Deployments: []app.Deployment{{Status: app.DeployFailed}}}) {
		t.Error("a failed deployment is treated as in flight")
	}

	if deploymentsInFlight(AppDetailData{}) {
		t.Error("an app with no deployments is being watched")
	}
}

// The build pane addresses itself, not the sheet around it. Swapping the sheet
// every two seconds would replace the tab strip and the search box above it,
// including the one somebody is typing into while the build runs.
func TestTheBuildPaneFragmentAddressesOnlyItself(t *testing.T) {
	href := buildFragmentHref(DeployLogsData{
		App: "web", DeployID: "11111111-2222-3333-4444-555555555555",
	})

	for _, want := range []string{
		"/apps/web/deployments/11111111-2222-3333-4444-555555555555/logs",
		"view=build",
		"pane=1",
	} {
		if !strings.Contains(href, want) {
			t.Errorf("build fragment href %q is missing %q", href, want)
		}
	}
}

// A running build is watched; a finished one is not. The pane carries its own
// trigger and is replaced whole, so the render that finds the build over comes
// back without one.
func TestTheBuildPaneStopsPollingWhenTheBuildEnds(t *testing.T) {
	for _, tc := range []struct {
		status    string
		wantWatch bool
	}{
		{app.BuildRunning, true},
		{app.BuildSucceeded, false},
		{app.BuildFailed, false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			d := DeployLogsData{
				App: "web", DeployID: uuid.New().String(), HasBuild: true,
				Build: app.Build{
					Status: tc.status, RepoURL: "https://github.com/example/app",
					RepoRef: "main", StartedAt: time.Now(),
				},
			}
			var sb strings.Builder
			if err := buildLogView(d).Render(t.Context(), &sb); err != nil {
				t.Fatalf("render: %v", err)
			}

			polling := strings.Contains(sb.String(), `hx-trigger="every 2s"`)
			if polling != tc.wantWatch {
				t.Errorf("a %q build polling = %v, want %v", tc.status, polling, tc.wantWatch)
			}
		})
	}
}

// A failed deploy leaves the previous workload running, so the app's live
// status stays green and the list said nothing about the change that never
// took. That is exactly the deploy somebody needs to find.
func TestTheAppListMarksAFailedDeploy(t *testing.T) {
	failed := sampleApp("owner-1", "web")
	failed.LastDeploy = app.DeployFailed
	fine := sampleApp("owner-1", "api")
	fine.LastDeploy = app.DeployActive

	apps := newFakeApps(failed, fine)
	body := get(t, testServer(t, Options{Apps: apps}), "/apps").Body.String()

	if !strings.Contains(body, "deploy failed") {
		t.Error("a failed deploy is invisible in the app list")
	}
	// The workload itself is still healthy, and the list must keep saying so —
	// the two are different questions.
	if !strings.Contains(body, "running") {
		t.Error("the workload's own status was replaced by the deploy's")
	}
	// Counted on the marker rather than the words: they also appear in the
	// title attribute, so the phrase alone is two matches for one app.
	if n := strings.Count(body, "status-err shrink-0"); n != 1 {
		t.Errorf("marked %d apps, want only the one that failed", n)
	}
}
