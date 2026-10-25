package web

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A build that failed says so, rather than "building…" for ever; one that
// built names its image.
func TestDeploymentImageLabel(t *testing.T) {
	for _, tc := range []struct {
		dep  app.Deployment
		want string
	}{
		{app.Deployment{Image: app.PendingImage, Status: app.DeployFailed}, "build failed"},
		{app.Deployment{Image: app.PendingImage, Status: app.DeployRunning}, "building…"},
		{app.Deployment{Image: app.PendingImage, Status: app.DeploySuperseded}, "not built"},
		{app.Deployment{Image: "registry/apps/web:abc", Status: app.DeployFailed}, "registry/apps/web:abc"},
	} {
		if got := deploymentImageLabel(tc.dep); got != tc.want {
			t.Errorf("deploymentImageLabel(%+v) = %q, want %q", tc.dep, got, tc.want)
		}
	}
}

// An app built from GitHub was deployed from GitHub, its first deploy too.
func TestDeploymentSource(t *testing.T) {
	gh := app.App{Source: app.SourceGit, Repo: app.Repo{URL: "https://github.com/acme/web.git"}}
	git := app.App{Source: app.SourceGit, Repo: app.Repo{URL: "https://gitlab.com/acme/web.git"}}
	image := app.App{Source: app.SourceImage}
	first := app.Deployment{Revision: "initial"}
	push := app.Deployment{Revision: app.PushTriggerPrefix + "abc"}
	for _, tc := range []struct {
		a    app.App
		dep  app.Deployment
		want string
	}{
		{gh, first, "GitHub"}, {git, first, "Git"}, {image, first, "image"}, {gh, push, "push"},
	} {
		if got := deploymentSource(tc.a, tc.dep); got != tc.want {
			t.Errorf("deploymentSource(%s, %s) = %q, want %q", tc.a.Repo.URL, tc.dep.Revision, got, tc.want)
		}
	}
}

// A pod whose container cannot start is shown by why, in red, not as Running.
func TestPodStatusShowsAFailingContainer(t *testing.T) {
	failing := orchestrator.PodInfo{Phase: "Running", Ready: 0, Total: 1, Reason: "RunContainerError"}
	if got := podStatusLabel(failing); got != "RunContainerError" {
		t.Errorf("label = %q", got)
	}
	if got := podPhaseClass(failing); got != "status-err" {
		t.Errorf("class = %q", got)
	}
	starting := orchestrator.PodInfo{Phase: "Running", Ready: 0, Total: 1, Reason: "ContainerCreating"}
	if podPhaseClass(starting) != "status-warn" {
		t.Error("a container on its way is shown as failing")
	}
	healthy := orchestrator.PodInfo{Phase: "Running", Ready: 1, Total: 1}
	if podStatusLabel(healthy) != "Running" || podPhaseClass(healthy) != "status-ok" {
		t.Error("a healthy pod is not Running")
	}
}

// An app that never ran says why its last deploy failed, on its own page.
func TestAFailedDeploySaysWhyOnTheAppPage(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.StatusKnown, a.Status = true, orchestrator.AppStatus{Phase: orchestrator.PhaseFailing, Desired: 1}
	apps := newFakeApps(a)
	apps.deploys = []app.Deployment{{
		ID: uuid.New(), AppID: a.ID, Image: "registry/apps/web:abc", Revision: "redeploy",
		Status: app.DeployFailed, StartedAt: time.Now(),
		Message: `app: rollout failed: RunContainerError: exec: "/app/bin/server": permission denied`,
	}}
	h := testServer(t, Options{Apps: apps})
	body := get(t, h, "/apps/web").Body.String()
	for _, want := range []string{"The last deploy failed", "RunContainerError: exec: &#34;/app/bin/server&#34;: permission denied", "failing"} {
		if !strings.Contains(body, want) {
			t.Errorf("the app page does not say %q", want)
		}
	}
	if strings.Contains(body, "app: rollout failed") {
		t.Error("the reason keeps the service's prefixes")
	}
}
