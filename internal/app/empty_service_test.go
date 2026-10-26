package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// uploadBuilder builds whatever it is asked to, and keeps the requests.
type uploadBuilder struct {
	mu   sync.Mutex
	reqs []orchestrator.BuildRequest
}

func (b *uploadBuilder) Build(_ context.Context, req orchestrator.BuildRequest) (orchestrator.BuildResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reqs = append(b.reqs, req)
	return orchestrator.BuildResult{CommitSHA: "upload", RunAsUser: 1000}, nil
}
func (*uploadBuilder) BuildJobName(orchestrator.BuildRequest) string { return "build-upload" }
func (*uploadBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{}, nil
}
func (*uploadBuilder) CancelBuild(context.Context, string) error { return nil }

func (b *uploadBuilder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reqs)
}

// sourceArchive is what the CLI's up sends: a gzipped tar of a directory.
func sourceArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Railway's Empty Service: created with nothing to run and nothing deployed,
// refusing a build of nothing, and built from the first upload — after
// which a redeploy rolls out what that upload built rather than cloning a
// repository it never had.
func TestAnEmptyServiceWaitsForItsSource(t *testing.T) {
	ctx := context.Background()
	b := &uploadBuilder{}
	s, _, pool := testService(t, Options{
		Images: fakeImages{}, Builder: b, SourceFetchURL: "https://yacht.example.test",
	})
	team := owner(t, s, pool, "empty-service")

	a, err := s.Create(ctx, team, CreateInput{Name: "api", Empty: true, Port: 8080, Replicas: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Source != SourceGit || a.Repo.Set() || a.Image != PendingImage {
		t.Fatalf("created %s from %q with image %q — want a source build with nothing yet",
			a.Source, a.Repo.URL, a.Image)
	}
	if deps, err := s.Deployments(ctx, team, a.ID, 10); err != nil || len(deps) != 0 {
		t.Fatalf("deployments = %+v, %v — want none until something arrives", deps, err)
	}

	if err := s.Redeploy(ctx, team, "api"); !errors.Is(err, ErrNothingToBuild) {
		t.Fatalf("redeploy with nothing given: err = %v, want ErrNothingToBuild", err)
	}

	if _, _, err := s.DeployUpload(ctx, team, "api",
		sourceArchive(t, map[string]string{"Dockerfile": "FROM busybox\n"})); err != nil {
		t.Fatalf("DeployUpload: %v", err)
	}
	if err := executeNamedOperation(s, ctx, team, "api"); err != nil {
		t.Fatalf("the upload's deploy: %v", err)
	}
	if b.count() != 1 || b.reqs[0].SourceURL == "" || b.reqs[0].RepoURL != "" {
		t.Fatalf("builds %+v — want one, fetching the upload", b.reqs)
	}
	built, err := s.Get(ctx, team, "api")
	if err != nil || built.Image == PendingImage {
		t.Fatalf("after its build the image is %q (%v)", built.Image, err)
	}

	if err := s.Redeploy(ctx, team, "api"); err != nil {
		t.Fatalf("redeploy after an upload: %v", err)
	}
	if err := executeNamedOperation(s, ctx, team, "api"); err != nil {
		t.Fatalf("the redeploy: %v", err)
	}
	if b.count() != 1 {
		t.Errorf("the redeploy built again (%d builds) — it has no repository to build", b.count())
	}
}

// A service built from uploads is copied into a new environment as an empty
// service: what was uploaded to production is not the copy's to run.
func TestAnEmptyServiceIsCopiedEmpty(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{
		Images: fakeImages{}, Builder: &uploadBuilder{}, SourceFetchURL: "https://yacht.example.test",
	})
	team := owner(t, s, pool, "empty-service-env")
	p, err := s.CreateProject(ctx, team, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, team, CreateInput{
		Name: "api", Empty: true, Port: 8080, Replicas: 1, ProjectID: p.ID,
		Variables: []VariableInput{{Key: "LOG_LEVEL", Value: "info"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, copies, err := s.CreateEnvironment(ctx, team, p.Slug, "staging", DefaultEnvironment)
	if err != nil || len(copies) != 1 || copies[0].Err != nil {
		t.Fatalf("CreateEnvironment: %+v, %v", copies, err)
	}
	if !strings.Contains(copies[0].Note, "uploaded") {
		t.Errorf("the copy's notes %q do not say it waits for an upload", copies[0].Note)
	}
	c, err := s.Get(ctx, team, "api-staging")
	if err != nil || c.Repo.Set() || c.Image != PendingImage {
		t.Fatalf("api-staging = %+v, %v — want an empty service", c, err)
	}
	if v, err := s.variableValue(ctx, team, c.ID, "LOG_LEVEL"); err != nil || v != "info" {
		t.Errorf("LOG_LEVEL in the copy = %q, %v", v, err)
	}
}
