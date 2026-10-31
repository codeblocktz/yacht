package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// servicePillRE is the open panel's heading pill, as rendered: the first
// status in the sheet's title.
var servicePillRE = regexp.MustCompile(`id="` + canvasSheetTitle + `"[\s\S]*?<span class="status ([a-z-]+)"[^>]*>([^<]*)</span>`)

// The panel's heading pill said "unknown" while a first build ran and after
// it failed, and "pending" after a deploy whose container crashed, while the
// card beside it said Building, Failed, Crashed. It says what the card says.
func TestThePanelHeadingSaysWhatItsCardSays(t *testing.T) {
	dep := uuid.New()
	repo := app.Repo{URL: "https://github.com/acme/api.git"}
	for _, tc := range []struct {
		name, label, class string
		a                  app.App
	}{
		{"building", "Building", "status-info", app.App{Source: app.SourceGit, Repo: repo, Image: app.PendingImage,
			LastDeploy: app.DeployRunning, LastDeployID: dep}},
		{"build failed", "Failed", "status-err", app.App{Source: app.SourceGit, Repo: repo, Image: app.PendingImage,
			LastDeploy: app.DeployFailed, LastDeployID: dep}},
		{"deploying", "Deploying", "status-info", app.App{Source: app.SourceImage, Image: "nginx:1.27",
			LastDeploy: app.DeployRunning, LastDeployID: dep}},
		{"crashing after a failed deploy", "Crashed", "status-err", app.App{Source: app.SourceImage, Image: "nginx:1.27",
			StatusKnown: true, Status: orchestrator.AppStatus{Phase: orchestrator.PhasePending},
			LastDeploy: app.DeployFailed, LastDeployID: dep}},
		{"crashed", "Crashed", "status-err", app.App{Source: app.SourceImage, Image: "nginx:1.27",
			StatusKnown: true, Status: orchestrator.AppStatus{Phase: orchestrator.PhaseFailing}}},
		{"online", "Online", "status-ok", app.App{Source: app.SourceImage, Image: "nginx:1.27",
			StatusKnown: true, Status: orchestrator.AppStatus{Phase: orchestrator.PhaseRunning},
			LastDeploy: app.DeploySucceeded, LastDeployID: dep}},
		{"sleeping", "Sleeping", "status-sleep", app.App{Source: app.SourceImage, Image: "nginx:1.27",
			Sleep: app.Sleep{State: app.SleepAsleep}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := sampleApp("owner-1", "api")
			a.Source, a.Repo, a.Image, a.Sleep = tc.a.Source, tc.a.Repo, tc.a.Image, tc.a.Sleep
			a.StatusKnown, a.Status = tc.a.StatusKnown, tc.a.Status
			a.LastDeploy, a.LastDeployID = tc.a.LastDeploy, tc.a.LastDeployID

			body := get(t, testServer(t, Options{Apps: newFakeApps(a)}), "/apps/api").Body.String()
			m := servicePillRE.FindStringSubmatch(body)
			if m == nil {
				t.Fatal("the panel heading has no status pill")
			}
			if m[2] != tc.label || m[1] != tc.class {
				t.Errorf("heading pill = %q (%s), want %q (%s)", m[2], m[1], tc.label, tc.class)
			}
			if card, _ := cardStatus(a); card != m[2] {
				t.Errorf("heading pill %q, card %q: the two disagree", m[2], card)
			}
			if !strings.Contains(m[0], `data-service-status="api"`) {
				t.Error("the heading pill is not marked for the canvas's poll to update")
			}
		})
	}
}

// The canvas's poll carries the pill's colour with each card's status, so
// the open heading follows a deploy rather than keeping what it was drawn
// with.
func TestTheCanvasPollCarriesTheHeadingPill(t *testing.T) {
	a := sampleApp("owner-1", "api")
	a.StatusKnown = false
	a.Source, a.Repo, a.Image = app.SourceGit, app.Repo{URL: "https://github.com/acme/api.git"}, app.PendingImage
	a.LastDeploy, a.LastDeployID = app.DeployFailed, uuid.New()
	body := get(t, testServer(t, Options{Apps: newFakeApps(a)}), "/projects/default/status").Body.String()
	if !strings.Contains(body, `"label":"Failed"`) || !strings.Contains(body, `"pill":"status-err"`) {
		t.Errorf("status poll = %s, want the Failed label and its pill", body)
	}
}
