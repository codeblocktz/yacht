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
// built names its image. A deploy refused for a reference never built, and
// says why instead (DEPLOY-9).
func TestDeploymentImageLabel(t *testing.T) {
	for _, tc := range []struct {
		dep  app.Deployment
		want string
	}{
		{app.Deployment{Image: app.PendingImage, Status: app.DeployFailed}, "build failed"},
		{app.Deployment{Image: app.PendingImage, Status: app.DeployFailed,
			Message: "app: MONGO_URL: ${{mongo.MONGO_URL}} refers to mongo, which no longer exists"}, "invalid reference"},
		{app.Deployment{Image: app.PendingImage, Status: app.DeployFailed,
			Message: "app: A refers through more than 4 variables; is there a loop?"}, "invalid reference"},
		{app.Deployment{Image: app.PendingImage, Status: app.DeployFailed, Message: "build: exit status 1"}, "build failed"},
		{app.Deployment{Image: app.PendingImage, Status: app.DeployRunning}, "building…"},
		{app.Deployment{Image: app.PendingImage, Status: app.DeploySuperseded}, "not built"},
		{app.Deployment{Image: "registry/apps/web:abc", Status: app.DeployFailed}, "registry/apps/web:abc"},
	} {
		if got := deploymentImageLabel(tc.dep); got != tc.want {
			t.Errorf("deploymentImageLabel(%+v) = %q, want %q", tc.dep, got, tc.want)
		}
	}
}

// An app built from GitHub was deployed from GitHub, its first deploy too;
// one whose only source is `up` was deployed from the CLI, a variable's
// deploy on its image too.
func TestDeploymentSource(t *testing.T) {
	gh := app.App{Source: app.SourceGit, Repo: app.Repo{URL: "https://github.com/acme/web.git"}}
	git := app.App{Source: app.SourceGit, Repo: app.Repo{URL: "https://gitlab.com/acme/web.git"}}
	image := app.App{Source: app.SourceImage}
	uploaded := app.App{Source: app.SourceGit}
	first := app.Deployment{Revision: "initial"}
	push := app.Deployment{Revision: app.PushTriggerPrefix + "abc"}
	up := app.Deployment{Revision: app.UploadTriggerPrefix + "abc"}
	changed := app.Deployment{Revision: "changes:FOO"}
	for _, tc := range []struct {
		a    app.App
		dep  app.Deployment
		want string
	}{
		{gh, first, "GitHub"}, {git, first, "Git"}, {image, first, "image"}, {gh, push, "push"},
		{uploaded, up, "CLI"}, {uploaded, changed, "CLI"}, {git, changed, "Git"},
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
	for _, want := range []string{"The last deploy failed", "RunContainerError: exec: &#34;/app/bin/server&#34;: permission denied", ">Crashed<"} {
		if !strings.Contains(body, want) {
			t.Errorf("the app page does not say %q", want)
		}
	}
	if strings.Contains(body, "app: rollout failed") {
		t.Error("the reason keeps the service's prefixes")
	}
}

// A deployment is titled by what caused it, and badged by what became of
// it, in Railway's words.
func TestDeploymentTitlesAndBadges(t *testing.T) {
	for rev, want := range map[string]string{
		"initial": "Initial deploy", "redeploy": "Redeploy", app.RollbackTrigger: "Rollback",
		app.PushTriggerPrefix + "2fdc9db0123": "Push 2fdc9db", "variable:DATABASE_URL": "Set DATABASE_URL",
		"variable-delete:OLD": "Removed OLD", "port:8080 internal:false": "Changed port → 8080",
		"changes:DATABASE_URL": "Deployed DATABASE_URL", "changes:A,B,C": "Deployed 3 changes",
	} {
		if got := deploymentTitle(app.Deployment{Revision: rev}); got != want {
			t.Errorf("deploymentTitle(%q) = %q, want %q", rev, got, want)
		}
	}
	for _, tc := range []struct {
		dep  app.Deployment
		want string
	}{
		{app.Deployment{IsActive: true, Status: app.DeploySucceeded}, "ACTIVE"},
		{app.Deployment{Status: app.DeploySuperseded}, "REMOVED"},
		{app.Deployment{Status: app.DeployFailed}, "FAILED"},
		{app.Deployment{Status: app.DeployRunning, Image: app.PendingImage}, "BUILDING"},
		{app.Deployment{Status: app.DeployRunning, Image: "registry/web:1"}, "DEPLOYING"},
	} {
		if got, _ := deploymentBadge(tc.dep); got != tc.want {
			t.Errorf("deploymentBadge(%+v) = %q, want %q", tc.dep.Status, got, tc.want)
		}
	}
}
