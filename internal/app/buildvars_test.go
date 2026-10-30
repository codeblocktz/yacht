package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func TestArgsUseAny(t *testing.T) {
	cases := []struct {
		name       string
		args, keys []string
		want       bool
	}{
		{"a build that did not say", nil, []string{"ANY"}, true},
		{"a buildpacks build", []string{orchestrator.BuildArgsAll}, []string{"ANY"}, true},
		{"a declared one changed", []string{"APP_HOSTS", "MIX_ENV"}, []string{"DATABASE_URL", "APP_HOSTS"}, true},
		{"none declared changed", []string{"APP_HOSTS"}, []string{"DATABASE_URL"}, false},
		{"a Dockerfile that declares nothing", []string{}, []string{"DATABASE_URL"}, false},
	}
	for _, c := range cases {
		if got := argsUseAny(c.args, c.keys); got != c.want {
			t.Errorf("%s: %t, want %t", c.name, got, c.want)
		}
	}
}

// argsBuilder builds every time, says its Dockerfile declares args, and
// keeps what it was asked.
type argsBuilder struct {
	args []string

	mu   sync.Mutex
	reqs []orchestrator.BuildRequest
}

func (b *argsBuilder) Build(_ context.Context, req orchestrator.BuildRequest) (orchestrator.BuildResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reqs = append(b.reqs, req)
	return orchestrator.BuildResult{CommitSHA: "c0ffee", RunAsUser: 1000, BuildArgs: b.args}, nil
}
func (*argsBuilder) BuildJobName(orchestrator.BuildRequest) string { return "build-args" }
func (*argsBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{}, nil
}
func (*argsBuilder) CancelBuild(context.Context, string) error { return nil }

func (b *argsBuilder) built() []orchestrator.BuildRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.reqs)
}

// A build is given the variables its deploy will run with — staged edits,
// secrets opened, the platform's own — and a staged change to one the
// Dockerfile declares deploys with a build, while any other deploys on the
// image already there.
func TestStagedVariablesTheImageWasBuiltWithBuildAgain(t *testing.T) {
	ctx := context.Background()
	builder := &argsBuilder{args: []string{"APP_HOSTS", "SECRET_KEY_BASE"}}
	s, _, id := stagingService(t, "svc-build-args", Options{Images: fakeImages{}, Builder: builder})

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Source: SourceGit, Replicas: 1, Port: 4000,
		Repo: Repo{URL: "https://example.test/web.git", Branch: "main"},
		Env:  map[string]string{"APP_HOSTS": "old.example"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if got := builder.built(); len(got) != 1 || got[0].Variables["APP_HOSTS"] != "old.example" {
		t.Fatalf("first build was given %+v", got)
	}

	// Declared: deploying it builds, and the build has the staged value.
	for _, in := range []VariableInput{
		{Key: "APP_HOSTS", Value: "new.example www.new.example"},
		{Key: "SECRET_KEY_BASE", Value: "very-secret-value", Secret: true},
	} {
		if err := s.SetVariable(ctx, id, "web", in); err != nil {
			t.Fatalf("set %s: %v", in.Key, err)
		}
	}
	if err := s.DeployStaged(ctx, id, "web"); err != nil {
		t.Fatalf("DeployStaged: %v", err)
	}
	a, _ := s.Get(ctx, id, "web")
	op, err := s.liveOperation(ctx, a)
	if err != nil || !op.RequiresBuild {
		t.Fatalf("a declared variable's deploy: op %+v, %v — want one that builds", op, err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	reqs := builder.built()
	if len(reqs) != 2 {
		t.Fatalf("%d builds, want 2", len(reqs))
	}
	req := reqs[1]
	if req.Variables["APP_HOSTS"] != "new.example www.new.example" ||
		req.Variables["SECRET_KEY_BASE"] != "very-secret-value" {
		t.Errorf("the rebuild was given %v", req.Variables)
	}
	if req.Variables[DefaultPlatformVariablePrefix+PlatformServiceName] != "web" {
		t.Errorf("the platform's variables are missing: %v", req.Variables)
	}
	if !slices.Contains(req.Redact, "very-secret-value") || slices.Contains(req.Redact, "new.example www.new.example") {
		t.Errorf("redacted %q: want the secret and not the plain value", req.Redact)
	}
	if changes, _ := s.StagedChanges(ctx, id, "web"); len(changes) != 0 {
		t.Errorf("after the rebuild, still staged: %s", changeKeys(changes))
	}

	// Not declared: deployed on the image already there.
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "LOG_LEVEL", Value: "debug"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeployStaged(ctx, id, "web"); err != nil {
		t.Fatalf("DeployStaged: %v", err)
	}
	a, _ = s.Get(ctx, id, "web")
	if op, err := s.liveOperation(ctx, a); err != nil || op.RequiresBuild {
		t.Fatalf("an undeclared variable's deploy: op %+v, %v — want one that does not build", op, err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if n := len(builder.built()); n != 2 {
		t.Errorf("%d builds, want still 2", n)
	}
}

// An image `up` built cannot be built again: its archive is gone once the
// build is over. A staged change to an ARG its Dockerfile declared is
// refused, saying another up is what deploys it, rather than deployed on the
// old image as a success with the old value in it; the change stays staged,
// and the next up builds with it. A variable the Dockerfile did not declare
// deploys on the image as it is.
func TestAnUploadedImagesBuildArgumentNeedsAnotherUp(t *testing.T) {
	ctx := context.Background()
	builder := &argsBuilder{args: []string{"FOO"}}
	s, _, id := stagingService(t, "svc-upload-args", Options{
		Images: fakeImages{}, Builder: builder, SourceFetchURL: "https://yacht.example.test",
	})

	if _, err := s.Create(ctx, id, CreateInput{Name: "argapp", Empty: true, Port: 8080, Replicas: 1}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SetVariable(ctx, id, "argapp", VariableInput{Key: "FOO", Value: "alpha-1"}); err != nil {
		t.Fatal(err)
	}
	archive := sourceArchive(t, map[string]string{"Dockerfile": "FROM busybox\nARG FOO\n"})
	if _, _, err := s.DeployUpload(ctx, id, "argapp", archive); err != nil {
		t.Fatalf("DeployUpload: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "argapp"); err != nil {
		t.Fatalf("the upload's build: %v", err)
	}
	built, _ := s.Get(ctx, id, "argapp")

	if err := s.SetVariable(ctx, id, "argapp", VariableInput{Key: "FOO", Value: "alpha-2"}); err != nil {
		t.Fatal(err)
	}
	err := s.DeployStaged(ctx, id, "argapp")
	var refused UploadRebuildError
	if !errors.As(err, &refused) || !slices.Equal(refused.Keys, []string{"FOO"}) {
		t.Fatalf("DeployStaged = %v — want it refused, naming FOO", err)
	}
	if msg := refused.Explain("datahuru"); !strings.Contains(msg, "This change needs a new `datahuru up`") {
		t.Errorf("the refusal says %q", msg)
	}
	a, _ := s.Get(ctx, id, "argapp")
	if _, err := s.liveOperation(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deploy was admitted all the same: %v", err)
	}
	if a.Image != built.Image {
		t.Errorf("image %q, want still %q", a.Image, built.Image)
	}
	if changes, _ := s.StagedChanges(ctx, id, "argapp"); changeKeys(changes) == "" {
		t.Error("the refused change is no longer staged")
	}

	// The next up builds with the staged value, and deploys it.
	if _, _, err := s.DeployUpload(ctx, id, "argapp", archive); err != nil {
		t.Fatalf("second DeployUpload: %v", err)
	}
	if err := executeNamedOperation(s, ctx, id, "argapp"); err != nil {
		t.Fatalf("the second upload's build: %v", err)
	}
	reqs := builder.built()
	if len(reqs) != 2 || reqs[1].Variables["FOO"] != "alpha-2" {
		t.Fatalf("builds %d, the second given FOO=%q — want alpha-2", len(reqs), reqs[len(reqs)-1].Variables["FOO"])
	}
	if changes, _ := s.StagedChanges(ctx, id, "argapp"); len(changes) != 0 {
		t.Errorf("after the up, still staged: %s", changeKeys(changes))
	}

	// Not declared: deployed on the image as it is, without a build.
	if err := s.SetVariable(ctx, id, "argapp", VariableInput{Key: "LOG_LEVEL", Value: "debug"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeployStaged(ctx, id, "argapp"); err != nil {
		t.Fatalf("an undeclared variable's DeployStaged: %v", err)
	}
	a, _ = s.Get(ctx, id, "argapp")
	if op, err := s.liveOperation(ctx, a); err != nil || op.RequiresBuild {
		t.Fatalf("an undeclared variable's deploy: op %+v, %v — want one that does not build", op, err)
	}
}
