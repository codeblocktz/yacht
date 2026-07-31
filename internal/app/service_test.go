package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store"
)

func testService(t *testing.T, opts Options) (*Service, *recordingOrchestrator, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run app service tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	orch := &recordingOrchestrator{Noop: orchestrator.NewNoop()}
	return NewService(pool, orch, log, opts), orch, pool
}

// recordingOrchestrator keeps the last spec it was asked to apply, so a test
// can assert on what crossed the seam rather than only on what was stored.
type recordingOrchestrator struct {
	*orchestrator.Noop

	mu   sync.Mutex
	last orchestrator.AppSpec
}

func (r *recordingOrchestrator) ApplyApp(ctx context.Context, spec orchestrator.AppSpec) error {
	r.mu.Lock()
	r.last = spec
	r.mu.Unlock()
	return r.Noop.ApplyApp(ctx, spec)
}

func (r *recordingOrchestrator) lastAppSpec() orchestrator.AppSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// owner registers an owner and removes it (and its apps, by cascade) after.
func owner(t *testing.T, s *Service, pool *pgxpool.Pool, id string) string {
	t.Helper()
	ctx := context.Background()

	purge := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM owners WHERE id = $1`, id); err != nil {
			t.Errorf("purge owner: %v", err)
		}
	}
	purge()
	t.Cleanup(purge)

	if err := s.EnsureOwner(ctx, id, "Test", ""); err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	return id
}

func TestCreateAppliesToClusterAndStores(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-create")

	a, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 2, Port: 8080,
		Env: map[string]string{"LOG_LEVEL": "info"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if a.Namespace != Namespace(id, "web") {
		t.Errorf("namespace = %q, want a derived one", a.Namespace)
	}
	if a.Env["LOG_LEVEL"] != "info" {
		t.Errorf("env round-trip failed: %v", a.Env)
	}

	// The workload reached the cluster, not just the database.
	if got := orch.Apps(); len(got) != 1 {
		t.Fatalf("orchestrator has %d apps, want 1", len(got))
	}
	if got := orch.Namespaces(); len(got) != 1 {
		t.Errorf("orchestrator has %d namespaces, want 1", len(got))
	}

	// And it is readable back with live status attached.
	fetched, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !fetched.StatusKnown || fetched.Status.Phase != orchestrator.PhaseRunning {
		t.Errorf("status = %+v, want a known running status", fetched.Status)
	}
}

// If the cluster rejects the workload, the database row must not survive —
// otherwise the app appears in the UI while nothing runs, and nothing ever
// retries it.
func TestCreateRollsBackWhenApplyFails(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-rollback")

	s.orch = failingOrchestrator{Noop: orchestrator.NewNoop()}

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1,
	}); err == nil {
		t.Fatal("expected Create to fail when the cluster rejects the workload")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM apps WHERE owner_id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("%d rows survived a failed apply, want 0", count)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-dupe")

	in := CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1}
	if _, err := s.Create(ctx, id, in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.Create(ctx, id, in)
	if !errors.Is(err, ErrNameTaken) {
		t.Errorf("err = %v, want ErrNameTaken", err)
	}
}

// Two owners may each have an app called "web". This is the property that
// makes the schema extensible to more than one owner later.
func TestNamesAreScopedByOwner(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	a := owner(t, s, pool, "svc-owner-a")
	b := owner(t, s, pool, "svc-owner-b")

	in := CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1}
	if _, err := s.Create(ctx, a, in); err != nil {
		t.Fatalf("owner a: %v", err)
	}
	if _, err := s.Create(ctx, b, in); err != nil {
		t.Errorf("owner b must be able to use the same app name: %v", err)
	}

	// And neither can see the other's.
	list, err := s.List(ctx, a)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("owner a sees %d apps, want 1", len(list))
	}
	if _, err := s.Get(ctx, a, "web"); err != nil {
		t.Errorf("owner a should see their own app: %v", err)
	}
}

func TestGetUnknownAppIsNotFound(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-missing")

	if _, err := s.Get(ctx, id, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestScaleAndDelete(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-scale")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	scaled, err := s.Scale(ctx, id, "web", 4)
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if scaled.Replicas != 4 {
		t.Errorf("replicas = %d, want 4", scaled.Replicas)
	}

	if err := s.Delete(ctx, id, "web"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(orch.Apps()) != 0 {
		t.Error("workload should be gone from the cluster")
	}
	if _, err := s.Get(ctx, id, "web"); !errors.Is(err, ErrNotFound) {
		t.Error("record should be gone from the database")
	}
}

// A cluster that cannot be reached must not turn a list into an error. The
// records are still real and the operator still needs to see them.
func TestListToleratesUnreachableCluster(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-degraded")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.orch = failingOrchestrator{Noop: orchestrator.NewNoop()}

	list, err := s.List(ctx, id)
	if err != nil {
		t.Fatalf("List should not fail when the cluster is down: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d apps, want 1", len(list))
	}
	if list[0].StatusKnown {
		t.Error("status must be reported as unknown rather than as stopped")
	}
}

func TestCreateIssuesAManagedHostname(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{AppDomain: "apps.example.com", WildcardTLS: true})
	id := owner(t, s, pool, "svc-host")

	created, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.Host != "web.apps.example.com" {
		t.Fatalf("Host = %q, want web.apps.example.com", created.Host)
	}

	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Host != "web.apps.example.com" {
		t.Errorf("Host on read = %q, want web.apps.example.com", got.Host)
	}
	if got.URLScheme() != "https" {
		t.Errorf("URLScheme = %q, want https when wildcard TLS is on", got.URLScheme())
	}
}

func TestCreateWithoutAnAppDomainIssuesNoHostname(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-no-host")

	created, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Host != "" {
		t.Fatalf("Host = %q, want empty when the feature is off", created.Host)
	}
}

// Changing the app domain moves each app's URL the next time it is applied,
// rather than all at once at startup. The managed column is what makes that
// safe: the reconcile rewrites only rows Yacht issued.
func TestApplyReissuesWhenTheAppDomainChanges(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{AppDomain: "apps.example.com"})
	id := owner(t, s, pool, "svc-reissue")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.opts.AppDomain = "apps.acme.com"

	if err := s.Redeploy(ctx, id, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}

	got, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Host != "web.apps.acme.com" {
		t.Fatalf("Host = %q, want it reissued to web.apps.acme.com", got.Host)
	}
}

func TestApplyPassesHostsToTheOrchestrator(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.example.com", WildcardTLS: true})
	id := owner(t, s, pool, "svc-host-spec")

	if _, err := s.Create(ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spec := orch.lastAppSpec()
	if len(spec.Hosts) != 1 || spec.Hosts[0] != "web.apps.example.com" {
		t.Fatalf("spec.Hosts = %v, want [web.apps.example.com]", spec.Hosts)
	}
	if !spec.TLS {
		t.Fatal("spec.TLS = false, want true when wildcard TLS is on")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		in   CreateInput
		ok   bool
	}{
		{"valid", CreateInput{Name: "web", Image: "nginx", Replicas: 1}, true},
		{"no name", CreateInput{Image: "nginx"}, false},
		{"uppercase", CreateInput{Name: "Web", Image: "nginx"}, false},
		{"underscore", CreateInput{Name: "my_app", Image: "nginx"}, false},
		{"leading dash", CreateInput{Name: "-web", Image: "nginx"}, false},
		{"path traversal", CreateInput{Name: "../etc", Image: "nginx"}, false},
		{"no image", CreateInput{Name: "web"}, false},
		{"negative replicas", CreateInput{Name: "web", Image: "nginx", Replicas: -1}, false},
		{"too many replicas", CreateInput{Name: "web", Image: "nginx", Replicas: 500}, false},
		{"bad port", CreateInput{Name: "web", Image: "nginx", Port: 70000}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.ok && err != nil {
				t.Errorf("expected valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

// Namespaces must be deterministic, owner-scoped, and immune to the
// truncation collision that a slug-and-trim scheme suffers from.
func TestNamespaceDerivation(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		if Namespace("o", "web") != Namespace("o", "web") {
			t.Error("must be stable for the same inputs")
		}
	})

	t.Run("differs by owner", func(t *testing.T) {
		if Namespace("a", "web") == Namespace("b", "web") {
			t.Error("two owners must not share a namespace")
		}
	})

	t.Run("long names do not collide", func(t *testing.T) {
		long1 := "a-very-long-application-name-that-would-be-truncated-aaaa"
		long2 := "a-very-long-application-name-that-would-be-truncated-bbbb"
		if Namespace("o", long1) == Namespace("o", long2) {
			t.Error("names that differ only past the truncation point must not collide")
		}
	})

	t.Run("is a legal kubernetes label", func(t *testing.T) {
		ns := Namespace("owner", "web")
		if len(ns) > 63 {
			t.Errorf("namespace %q is %d chars, over the 63 limit", ns, len(ns))
		}
		if err := orchestrator.ValidateDNSLabel("namespace", ns); err != nil {
			t.Errorf("namespace is not a valid DNS label: %v", err)
		}
	})
}

// failingOrchestrator rejects every mutation, standing in for an unreachable
// or hostile cluster.
type failingOrchestrator struct{ *orchestrator.Noop }

var errCluster = errors.New("cluster refused the request")

func (failingOrchestrator) EnsureNamespace(context.Context, orchestrator.NamespaceSpec) error {
	return errCluster
}

func (failingOrchestrator) ApplyApp(context.Context, orchestrator.AppSpec) error {
	return errCluster
}

func (failingOrchestrator) AppStatus(
	context.Context, orchestrator.Ref,
) (orchestrator.AppStatus, error) {
	return orchestrator.AppStatus{}, errCluster
}
