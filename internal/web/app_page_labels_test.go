package web

import (
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A build that failed says so, rather than "building…" for ever, and one
// that built says it is deploying.
func TestDeploymentImageLabel(t *testing.T) {
	unbuilt := app.App{Image: app.PendingImage}
	built := app.App{Image: "registry/apps/web:new"}
	for _, tc := range []struct {
		a    app.App
		dep  app.Deployment
		want string
	}{
		{unbuilt, app.Deployment{Image: app.PendingImage, Status: app.DeployFailed}, "build failed"},
		{unbuilt, app.Deployment{Image: app.PendingImage, Status: app.DeployRunning}, "building…"},
		{built, app.Deployment{Image: app.PendingImage, Status: app.DeployRunning}, "deploying registry/apps/web:new"},
		{built, app.Deployment{Image: "registry/apps/web:abc", Status: app.DeployFailed}, "registry/apps/web:abc"},
	} {
		if got := deploymentImageLabel(tc.a, tc.dep); got != tc.want {
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
