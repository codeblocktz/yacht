package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/cluster"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/registry"
	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

func TestEveryAppSpecFieldHasOneReleaseOwner(t *testing.T) {
	releaseOwned := map[string]bool{
		"Image": true, "Replicas": true, "Port": true, "Env": true,
		"CPURequest": true, "CPULimit": true,
		"MemoryRequest": true, "MemoryLimit": true,
		"WritableRootFilesystem": true, "Internal": true,
		"RunAsUser": true, "FSGroup": true, "ScratchPaths": true,
		"HealthPath": true, "Liveness": true,
	}
	overlayOwned := map[string]bool{
		"Ref": true, "ReleaseID": true, "ConfigVersion": true,
		"Hosts": true, "Secrets": true, "Volumes": true,
		"TLSHosts": true, "CNAMETarget": true, "HTTPSOnly": true,
		"RegistryAuth": true,
	}

	typ := reflect.TypeOf(orchestrator.AppSpec{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		owners := 0
		if releaseOwned[name] {
			owners++
		}
		if overlayOwned[name] {
			owners++
		}
		if owners != 1 {
			t.Errorf("AppSpec.%s has %d owners; every field needs exactly one", name, owners)
		}
	}
	if got, want := len(releaseOwned)+len(overlayOwned), typ.NumField(); got != want {
		t.Fatalf("ownership table has %d fields, AppSpec has %d", got, want)
	}
}

func TestAReleaseReconstructsTheWholeAppSpec(t *testing.T) {
	ref := orchestrator.Ref{Owner: "owner", Namespace: "ns", Name: "web"}
	release := Release{
		ID:          uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		ImageRef:    "registry.test/team/web:v7",
		ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Replicas:    3, Port: 8080, Env: map[string]string{"PLAIN": "value"},
		CPURequest: "100m", CPULimit: "500m",
		MemoryRequest: "128Mi", MemoryLimit: "512Mi",
		WritableRootFilesystem: true, Internal: true,
		RunAsUser: 1001, FSGroup: 1002, ScratchPaths: []string{"/tmp/run"},
		HealthPath: "/ready", Liveness: true,
	}
	overlays := ReleaseOverlays{
		Ref: ref, ConfigVersion: 7, RegistryAuth: []byte("auth"),
		Secrets: map[string]string{"SECRET": "current"},
		Volumes: []orchestrator.VolumeSpec{{Name: "data", MountPath: "/data", SizeBytes: 42}},
		Hosts:   []string{"web.example.test"}, TLSHosts: []string{"web.example.test"},
		HTTPSOnly: true, CNAMETarget: "edge.example.test",
	}
	want := orchestrator.AppSpec{
		Ref: ref, ReleaseID: release.ID.String(), ConfigVersion: 7,
		Image:    "registry.test/team/web@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Replicas: 3, Port: 8080, Env: map[string]string{"PLAIN": "value"},
		CPURequest: "100m", CPULimit: "500m",
		MemoryRequest: "128Mi", MemoryLimit: "512Mi",
		WritableRootFilesystem: true, Internal: true,
		RunAsUser: 1001, FSGroup: 1002, ScratchPaths: []string{"/tmp/run"},
		HealthPath: "/ready", Liveness: true,
		RegistryAuth: []byte("auth"), Secrets: map[string]string{"SECRET": "current"},
		Volumes: []orchestrator.VolumeSpec{{Name: "data", MountPath: "/data", SizeBytes: 42}},
		Hosts:   []string{"web.example.test"}, TLSHosts: []string{"web.example.test"},
		HTTPSOnly: true, CNAMETarget: "edge.example.test",
	}
	if got := release.AppSpec(overlays); !reflect.DeepEqual(got, want) {
		t.Fatalf("AppSpec mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestConfigVersionAndReleaseClassification(t *testing.T) {
	ctx := context.Background()
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, _, pool := testService(t, Options{Keeper: keeper})
	ownerID := owner(t, s, pool, "release-classification")
	created, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertState := func(wantVersion int64, wantReleases int) App {
		t.Helper()
		a, err := s.Get(ctx, ownerID, created.Name)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if a.ConfigVersion != wantVersion {
			t.Fatalf("config_version = %d, want %d", a.ConfigVersion, wantVersion)
		}
		releases, err := s.Releases(ctx, ownerID, a.ID, 100)
		if err != nil {
			t.Fatalf("Releases: %v", err)
		}
		if len(releases) != wantReleases {
			t.Fatalf("releases = %d, want %d", len(releases), wantReleases)
		}
		return a
	}

	assertState(1, 1)
	if _, err := s.Scale(ctx, ownerID, created.Name, 1); err != nil {
		t.Fatalf("no-op Scale: %v", err)
	}
	assertState(1, 1)
	if _, err := s.Scale(ctx, ownerID, created.Name, 0); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	assertState(2, 2)
	if err := s.SetVariable(ctx, ownerID, created.Name, VariableInput{Key: "PLAIN", Value: "one"}); err != nil {
		t.Fatalf("SetVariable plain: %v", err)
	}
	assertState(3, 3)
	if err := s.SetVariable(ctx, ownerID, created.Name, VariableInput{Key: "PLAIN", Value: "one"}); err != nil {
		t.Fatalf("no-op SetVariable: %v", err)
	}
	assertState(3, 3)
	if err := s.SetVariable(ctx, ownerID, created.Name, VariableInput{Key: "SECRET", Value: "now", Secret: true}); err != nil {
		t.Fatalf("SetVariable secret: %v", err)
	}
	assertState(4, 3)
	if err := s.SetVariable(ctx, ownerID, created.Name, VariableInput{Key: "SECRET", Value: "now", Secret: true}); err != nil {
		t.Fatalf("no-op secret: %v", err)
	}
	assertState(4, 3)
	if _, err := s.Scale(ctx, ownerID, created.Name, 1); err != nil {
		t.Fatalf("Scale before volume: %v", err)
	}
	assertState(5, 4)
	releases, err := s.Releases(ctx, ownerID, created.ID, 1)
	if err != nil {
		t.Fatalf("Releases after secret: %v", err)
	}
	if got := releases[0].SecretKeys; !reflect.DeepEqual(got, []string{"SECRET"}) {
		t.Fatalf("release secret keys = %v, want names only", got)
	}
	if _, leaked := releases[0].Env["SECRET"]; leaked {
		t.Fatal("a secret value entered immutable release environment")
	}
	if _, err := s.AttachVolume(ctx, ownerID, created.Name, VolumeInput{
		Name: "data", MountPath: "/data", SizeBytes: 1024,
	}); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	assertState(6, 4)
	if err := s.SetNetworking(ctx, ownerID, created.Name, true, true); err != nil {
		t.Fatalf("no-op networking: %v", err)
	}
	assertState(6, 4)
	if err := s.SetNetworking(ctx, ownerID, created.Name, false, false); err != nil {
		t.Fatalf("SetNetworking: %v", err)
	}
	assertState(7, 4)
	if err := s.DeleteVolume(ctx, ownerID, created.Name, "data", true); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	assertState(8, 4)
	if err := s.DeleteVariable(ctx, ownerID, created.Name, "PLAIN"); err != nil {
		t.Fatalf("DeleteVariable plain: %v", err)
	}
	assertState(9, 5)
	if err := s.DeleteVariable(ctx, ownerID, created.Name, "SECRET"); err != nil {
		t.Fatalf("DeleteVariable secret: %v", err)
	}
	final := assertState(10, 5)
	if final.ActiveReleaseID == nil {
		t.Fatal("successful release deployment did not write active_release_id")
	}
}

func TestConcurrentVariableWritesDoNotLoseConfigVersions(t *testing.T) {
	ctx := context.Background()
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("c"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, _, pool := testService(t, Options{Keeper: keeper})
	ownerID := owner(t, s, pool, "release-counter-race")
	created, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const writes = 8
	var wg sync.WaitGroup
	errCh := make(chan error, writes)
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- s.SetVariable(ctx, ownerID, created.Name, VariableInput{
				Key: "SECRET_" + string(rune('A'+i)), Value: "value", Secret: true,
			})
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent SetVariable: %v", err)
		}
	}
	a, err := s.Get(ctx, ownerID, created.Name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if want := int64(1 + writes); a.ConfigVersion != want {
		t.Fatalf("config_version = %d, want %d", a.ConfigVersion, want)
	}
}

type failingManifests struct{ err error }

func (f failingManifests) ResolveDigest(context.Context, string) (string, error) {
	return "", f.err
}

type countingOrchestrator struct {
	*orchestrator.Noop
	calls atomic.Int32
}

func (c *countingOrchestrator) EnsureNamespace(context.Context, orchestrator.NamespaceSpec) error {
	c.calls.Add(1)
	return nil
}

func (c *countingOrchestrator) ApplyApp(context.Context, orchestrator.AppSpec) error {
	c.calls.Add(1)
	return nil
}

func TestAnUnresolvableImageFailsBeforeAClusterCall(t *testing.T) {
	ctx := context.Background()
	base, _, pool := testService(t, Options{})
	ownerID := owner(t, base, pool, "release-resolution-first")
	created, err := base.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	wantErr := errors.New("registry unavailable")
	orch := &countingOrchestrator{Noop: orchestrator.NewNoop()}
	s := NewService(pool, orch, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Manifests: failingManifests{err: wantErr},
	})
	_, err = s.Update(ctx, ownerID, created.Name, UpdateInput{
		Image: "nginx:1.28", Port: 8080,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Update error = %v, want resolver error", err)
	}
	if got := orch.calls.Load(); got != 0 {
		t.Fatalf("cluster calls = %d, want 0", got)
	}
	releases, err := base.Releases(ctx, ownerID, created.ID, 100)
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("releases after failed resolution = %d, want initial only", len(releases))
	}
}

func TestReleaseRowsAreImmutableAndCannotBeCrossLinked(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "release-immutability")
	one, err := s.Create(ctx, ownerID, CreateInput{
		Name: "one", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create one: %v", err)
	}
	two, err := s.Create(ctx, ownerID, CreateInput{
		Name: "two", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create two: %v", err)
	}
	releases, err := s.Releases(ctx, ownerID, one.ID, 1)
	if err != nil || len(releases) != 1 {
		t.Fatalf("Releases: %v (%d rows)", err, len(releases))
	}
	if _, err := pool.Exec(ctx,
		`UPDATE app_releases SET replicas = replicas + 1 WHERE id = $1`, releases[0].ID); err == nil {
		t.Fatal("an immutable release accepted an update")
	}
	n, err := s.q.SetActiveRelease(ctx, dbgen.SetActiveReleaseParams{
		OwnerID: ownerID, AppID: two.ID, ReleaseID: pgUUID(releases[0].ID),
	})
	if err != nil {
		t.Fatalf("SetActiveRelease cross-link: %v", err)
	}
	if n != 0 {
		t.Fatal("an app accepted another app's release as active")
	}
}

func TestInstallWideOverlaysVersionOnlyAffectedApps(t *testing.T) {
	ctx := context.Background()
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("g"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, _, pool := testService(t, Options{
		Keeper: keeper, Images: fakeImages{}, Builder: fakeBuilder{},
	})
	ownerID := owner(t, s, pool, "release-global-overlays")
	gitApp, err := s.Create(ctx, ownerID, CreateInput{
		Name: "git", Source: SourceGit, Replicas: 1, Port: 8080,
		Repo: Repo{URL: "https://example.test/repo.git", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Create Git app: %v", err)
	}
	imageApp, err := s.Create(ctx, ownerID, CreateInput{
		Name: "image", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create image app: %v", err)
	}
	version := func(name string) int64 {
		t.Helper()
		a, err := s.Get(ctx, ownerID, name)
		if err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
		return a.ConfigVersion
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registries := registry.New(pool, keeper, log)
	password := gitApp.ID.String()
	if err := registries.Set(ctx, uuid.Nil, "registry.test", "acme", "bot", password, false); err != nil {
		t.Fatalf("Set registry: %v", err)
	}
	if got := version(gitApp.Name); got != 2 {
		t.Fatalf("Git config version after registry = %d, want 2", got)
	}
	if got := version(imageApp.Name); got != 1 {
		t.Fatalf("image config version after registry = %d, want 1", got)
	}
	if err := registries.Set(ctx, uuid.Nil, "registry.test", "acme", "bot", password, false); err != nil {
		t.Fatalf("no-op registry: %v", err)
	}
	if got := version(gitApp.Name); got != 2 {
		t.Fatalf("Git config version after no-op registry = %d, want 2", got)
	}

	dns := cluster.New(pool, keeper, log)
	target := "edge-" + gitApp.ID.String()[:8] + ".example.test"
	if err := dns.SetDNS(ctx, target, cluster.DefaultTXTPrefix); err != nil {
		t.Fatalf("SetDNS: %v", err)
	}
	if got := version(gitApp.Name); got != 3 {
		t.Fatalf("Git config version after CNAME target = %d, want 3", got)
	}
	if got := version(imageApp.Name); got != 2 {
		t.Fatalf("image config version after CNAME target = %d, want 2", got)
	}
	if err := dns.SetDNS(ctx, target, "changed-"); err != nil {
		t.Fatalf("SetDNS prefix only: %v", err)
	}
	if got := version(imageApp.Name); got != 2 {
		t.Fatalf("config version after non-rendered DNS prefix = %d, want 2", got)
	}
}
