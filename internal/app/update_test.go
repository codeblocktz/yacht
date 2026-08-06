package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// fakeImages is the smallest registry a Git app needs to exist.
//
// Nothing here builds — these tests are about what an update may change, and a
// Git app is only reachable at all on an install that could build. Without it
// Create refuses the source before any of that can be exercised.
type fakeImages struct{}

func (fakeImages) ImageFor(_ context.Context, ownerID, app, revision string) (string, error) {
	return "registry.test/" + ownerID + "/" + app + ":" + revision, nil
}
func (fakeImages) Configured(context.Context) bool              { return true }
func (fakeImages) Insecure(context.Context) bool                { return false }
func (fakeImages) DockerConfig(context.Context) ([]byte, error) { return []byte("{}"), nil }

// fakeBuilder never builds. CanBuild wants a builder as well as a registry —
// a repository source is only offered on an install that has both — and these
// tests need the source to exist, not to run.
type fakeBuilder struct{}

func (fakeBuilder) Build(context.Context, orchestrator.BuildRequest) (orchestrator.BuildResult, error) {
	return orchestrator.BuildResult{}, errors.New("not built in this test")
}
func (fakeBuilder) BuildJobName(orchestrator.BuildRequest) string { return "build-test" }
func (fakeBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{}, nil
}
func (fakeBuilder) CancelBuild(context.Context, string) error { return nil }

// gitService is a service that can offer the repository source.
func gitService(t *testing.T, name string) (*Service, *recordingOrchestrator, string) {
	t.Helper()
	s, orch, pool := testService(t, Options{Images: fakeImages{}, Builder: fakeBuilder{}})
	return s, orch, owner(t, s, pool, name)
}

// The gap this closes. Before Update existed, an app's image was decided at
// create and frozen — so shipping a new tag meant deleting the app and starting
// again, taking its domains and its deployment history with it.
func TestAnImageTagCanBeChanged(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-update-image")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := s.Update(ctx, id, "web", UpdateInput{
		Image: "nginx:1.28", Port: 8080,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Image != "nginx:1.28" {
		t.Fatalf("image = %q, want the new tag", updated.Image)
	}

	// And it reached the cluster rather than only the row.
	wantPinned := "nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if got := orch.lastAppSpec().Image; got != wantPinned {
		t.Errorf("applied image = %q, want immutable %q", got, wantPinned)
	}

	// Recorded as a deployment, naming what changed. A history of rows all
	// saying "update" answers nothing three weeks later.
	deps, err := s.Deployments(ctx, id, updated.ID, 10)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deps) < 2 {
		t.Fatalf("deployments = %d, want the update recorded", len(deps))
	}
	if !strings.Contains(deps[0].Revision, "nginx:1.28") {
		t.Errorf("revision = %q, want it to name the change", deps[0].Revision)
	}
}

// A port change has to reach the Service and the Ingress, not only the
// container. One that rewrote the container's port alone would leave the
// Service routing to nothing.
func TestChangingThePortReachesTheCluster(t *testing.T) {
	ctx := context.Background()
	// A domain of its own. The host index is globally unique, so two tests
	// issuing web.apps.example.com collide when their packages run together.
	s, orch, pool := testService(t, Options{AppDomain: "apps.update-port.test"})
	id := owner(t, s, pool, "svc-update-port")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: "nginx:alpine", Port: 3000,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	spec := orch.lastAppSpec()
	if spec.Port != 3000 {
		t.Errorf("applied port = %d, want 3000", spec.Port)
	}
	if len(spec.Hosts) == 0 {
		t.Error("the app lost its hostname on a port change")
	}
}

// A Git app's image is whatever its last build produced. Accepting one typed by
// hand and then overwriting it on the next build is worse than saying no.
func TestAGitAppRefusesAHandTypedImage(t *testing.T) {
	ctx := context.Background()
	s, _, id := gitService(t, "svc-update-git")

	created, err := s.Create(ctx, id, CreateInput{
		Name: "api", Source: SourceGit, Replicas: 1, Port: 8080,
		Repo: Repo{URL: "https://github.com/example/api", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Image != PendingImage {
		t.Fatalf("image = %q, want the pending placeholder", created.Image)
	}

	_, err = s.Update(ctx, id, "api", UpdateInput{
		Image: "nginx:alpine", Port: 8080,
		Repo: Repo{URL: "https://github.com/example/api", Branch: "main"},
	})
	if err == nil {
		t.Fatal("a hand-typed image was accepted on a built app")
	}
	if !strings.Contains(err.Error(), "comes from its build") {
		t.Errorf("error = %q, want it to explain why", err)
	}

	// And the stored image is untouched.
	after, _ := s.Get(ctx, id, "api")
	if after.Image != PendingImage {
		t.Errorf("image = %q, want the placeholder kept", after.Image)
	}
}

// Leaving the image field alone on a Git app is the ordinary case — the form
// renders it disabled, so nothing is submitted — and must not be refused.
func TestAGitAppUpdatesWithoutTouchingItsImage(t *testing.T) {
	ctx := context.Background()
	s, _, id := gitService(t, "svc-update-git-ok")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "api", Source: SourceGit, Replicas: 1, Port: 8080,
		Repo: Repo{URL: "https://github.com/example/api", Branch: "main"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := s.Update(ctx, id, "api", UpdateInput{
		Port: 9000,
		Repo: Repo{URL: "https://github.com/example/api", Branch: "release"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Image != PendingImage {
		t.Errorf("image = %q, want the build's value kept", updated.Image)
	}
	if updated.Repo.Branch != "release" {
		t.Errorf("branch = %q, want the new one", updated.Repo.Branch)
	}
}

// Changing where an app builds from reaches no cluster: it decides what the
// next build reads. Recording a deployment for it would put a rollout in the
// history that never happened.
func TestARepositoryChangeAloneIsNotARollout(t *testing.T) {
	ctx := context.Background()
	s, _, id := gitService(t, "svc-update-repo")

	created, err := s.Create(ctx, id, CreateInput{
		Name: "api", Source: SourceGit, Replicas: 1, Port: 8080,
		Repo: Repo{URL: "https://github.com/example/api", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, _ := s.Deployments(ctx, id, created.ID, 50)

	if _, err := s.Update(ctx, id, "api", UpdateInput{
		Port: 8080,
		Repo: Repo{URL: "https://github.com/example/other", Branch: "main"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, _ := s.Deployments(ctx, id, created.ID, 50)
	if len(after) != len(before) {
		t.Errorf("deployments went from %d to %d — a repo change is not a rollout",
			len(before), len(after))
	}
}

// Turning an app internal withdraws its hostname rather than hiding it, and
// turning it back issues one again.
func TestInternalWithdrawsAndRestoresTheHostname(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.update-internal.test"})
	id := owner(t, s, pool, "svc-update-internal")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(orch.lastAppSpec().Hosts) == 0 {
		t.Fatal("the app never had a hostname to withdraw")
	}

	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: "nginx:alpine", Port: 8080, Internal: true,
	}); err != nil {
		t.Fatalf("Update to internal: %v", err)
	}
	if hosts := orch.lastAppSpec().Hosts; len(hosts) != 0 {
		t.Errorf("hosts = %v, want none on an internal app", hosts)
	}

	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: "nginx:alpine", Port: 8080, Internal: false,
	}); err != nil {
		t.Fatalf("Update back to public: %v", err)
	}
	if len(orch.lastAppSpec().Hosts) == 0 {
		t.Error("the hostname was not issued again")
	}
}

// An update is held to the same bounds a create is.
func TestUpdateValidatesLikeCreate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     UpdateInput
		source Source
		ok     bool
	}{
		{"a sensible change", UpdateInput{Image: "nginx", Port: 8080}, SourceImage, true},
		{"no port is allowed", UpdateInput{Image: "nginx", Port: 0}, SourceImage, true},
		{"a port above the range", UpdateInput{Image: "nginx", Port: 70000}, SourceImage, false},
		{"a negative port", UpdateInput{Image: "nginx", Port: -1}, SourceImage, false},
		{"an image app needs an image", UpdateInput{Port: 8080}, SourceImage, false},
		// A Git app's image comes from its build, so an empty one is not
		// missing — it is the build's to fill in.
		{"a git app does not", UpdateInput{Port: 8080}, SourceGit, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate(tc.source)
			if (err == nil) != tc.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// Resource limits were on the App and on CreateInput and could never be
// changed after the fact.
func TestResourceLimitsCanBeChanged(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-update-res")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: "nginx:alpine", Port: 8080,
		CPURequest: "250m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	spec := orch.lastAppSpec()
	if spec.CPULimit != "1" || spec.MemoryLimit != "512Mi" {
		t.Errorf("limits = %q/%q, want them applied", spec.CPULimit, spec.MemoryLimit)
	}

	after, _ := s.Get(ctx, id, "web")
	if after.CPURequest != "250m" || after.MemoryRequest != "128Mi" {
		t.Errorf("requests = %q/%q, want them stored", after.CPURequest, after.MemoryRequest)
	}
}

// Scaling keeps its own path, so a settings save cannot silently reset a scale
// somebody chose.
func TestUpdateDoesNotResetReplicas(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-update-replicas")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 3, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := s.Update(ctx, id, "web", UpdateInput{Image: "nginx:1.28", Port: 8080})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Replicas != 3 {
		t.Errorf("replicas = %d, want the 3 that were chosen", updated.Replicas)
	}
}
