package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/secret"
)

func TestReferencesAreFoundOnceEach(t *testing.T) {
	got := References("postgres://${{db.USER}}:${{ db.PASSWORD }}@${{db.PRIVATE_HOST}}/${{db.USER}} ${{Not.valid}} ${{db}}")
	want := []Reference{{"db", "USER"}, {"db", "PASSWORD"}, {"db", "PRIVATE_HOST"}}
	if len(got) != len(want) {
		t.Fatalf("References = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("References[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if HasReference("postgres://localhost/app") {
		t.Error("a plain value is a reference")
	}
}

func referenceService(t *testing.T, team string) (*Service, *recordingOrchestrator, string) {
	t.Helper()
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("r"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	s, orch, pool := testService(t, Options{Keeper: keeper})
	return s, orch, owner(t, s, pool, team)
}

// A reference resolves when its app deploys, from the other app's current
// variables, secrets opened — and reaches the cluster as a secret, never
// the plain environment a release keeps for good.
func TestAReferenceResolvesAtDeployAsASecret(t *testing.T) {
	ctx := context.Background()
	s, orch, id := referenceService(t, "svc-references")

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "db", Image: "postgres:16-alpine", Replicas: 1, Port: 5432,
	}); err != nil {
		t.Fatalf("Create db: %v", err)
	}
	// Internal, so it is reached on its own port rather than the public one.
	if _, err := s.pool.Exec(ctx, `UPDATE apps SET internal = true WHERE owner_id = $1 AND name = 'db'`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(ctx, id, "db", VariableInput{
		Key: "DATABASE_URL", Value: "postgres://app:hunter2@db/app", Secret: true,
	}); err != nil {
		t.Fatalf("set the database's URL: %v", err)
	}
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create web: %v", err)
	}
	for key, value := range map[string]string{
		"DATABASE_URL": "${{db.DATABASE_URL}}",
		"DB_ADDR":      "${{db.PRIVATE_HOST}}:${{db.PRIVATE_PORT}}",
	} {
		// The second save finds the first one's deploy running; it is saved
		// all the same, and said to be.
		if err := s.SetVariable(ctx, id, "web", VariableInput{Key: key, Value: value}); err != nil &&
			!errors.Is(err, ErrSavedWhileDeploying) {
			t.Fatalf("set %s: %v", key, err)
		}
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("deploy web: %v", err)
	}

	spec := orch.lastAppSpec()
	if spec.Secrets["DATABASE_URL"] != "postgres://app:hunter2@db/app" {
		t.Errorf("DATABASE_URL reached the cluster as %q", spec.Secrets["DATABASE_URL"])
	}
	db, err := s.Get(ctx, id, "db")
	if err != nil {
		t.Fatal(err)
	}
	if want := "db." + db.Namespace + ".svc.cluster.local:5432"; spec.Secrets["DB_ADDR"] != want {
		t.Errorf("DB_ADDR = %q, want %q", spec.Secrets["DB_ADDR"], want)
	}
	for key, value := range spec.Env {
		if strings.Contains(value, "hunter2") || HasReference(value) {
			t.Errorf("the plain environment carries %s = %q", key, value)
		}
	}

	// The page sees the reference as typed, not what it resolves to.
	web, err := s.Get(ctx, id, "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range web.Variables {
		if v.Key == "DATABASE_URL" && (!v.Reference || v.Value != "${{db.DATABASE_URL}}") {
			t.Errorf("the page is given %+v", v)
		}
	}

	// And the canvas draws an edge for it.
	links, err := s.Links(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range links {
		found = found || (l.From == "web" && l.To == "db" && l.Via == "DATABASE_URL")
	}
	if !found {
		t.Errorf("links = %+v, want web → db via DATABASE_URL", links)
	}
}

// A reference to something that does not exist is refused when it is typed,
// not at a deploy that fails later.
func TestAReferenceToNothingIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _, id := referenceService(t, "svc-references-refused")
	for _, name := range []string{"db", "web"} {
		if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
			Name: name, Image: "nginx:alpine", Replicas: 1, Port: 8080,
		}); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	for value, says := range map[string]string{
		"${{nope.DATABASE_URL}}": "no app called that",
		"${{db.DATABASE_URL}}":   "does not have",
		"${{web.PRIVATE_HOST}}":  "this app itself",
	} {
		err := s.SetVariable(ctx, id, "web", VariableInput{Key: "DATABASE_URL", Value: value})
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: err = %v, want it to say %q", value, err, says)
		}
	}
}

// Deleting an app another one refers to breaks the other at its next
// deploy, so the delete has to be able to say which variables those are:
// a reference by service name, a staged one, and none from the app itself.
func TestReferrersNameTheVariablesThatReferToAnApp(t *testing.T) {
	ctx := context.Background()
	s, _, id := referenceService(t, "svc-referrers")

	for _, in := range []CreateInput{
		{Name: "db", Image: "postgres:16-alpine", Replicas: 1, Port: 5432},
		{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080},
		{Name: "api", Image: "nginx:alpine", Replicas: 1, Port: 8080},
	} {
		if _, err := createAndDeploy(t, s, ctx, id, in); err != nil {
			t.Fatalf("Create %s: %v", in.Name, err)
		}
	}
	set := func(name, key, value string) {
		t.Helper()
		if err := s.SetVariable(ctx, id, name, VariableInput{Key: key, Value: value}); err != nil &&
			!errors.Is(err, ErrSavedWhileDeploying) {
			t.Fatalf("set %s on %s: %v", key, name, err)
		}
	}
	set("db", "DATABASE_URL", "postgres://db/app")
	set("web", "DATABASE_URL", "${{db.DATABASE_URL}}")
	set("web", "DB_ADDR", "${{db.PRIVATE_HOST}}:${{db.PRIVATE_PORT}}")
	set("web", "PLAIN", "postgres://db/app")
	set("api", "WEB", "${{web.PRIVATE_HOST}}")

	got, err := s.Referrers(ctx, id, "db")
	if err != nil {
		t.Fatalf("Referrers: %v", err)
	}
	want := []Referrer{{App: "web", Key: "DATABASE_URL"}, {App: "web", Key: "DB_ADDR"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Referrers(db) = %+v, want %+v", got, want)
	}
	if got, err := s.Referrers(ctx, id, "api"); err != nil || len(got) != 0 {
		t.Errorf("Referrers(api) = %+v, %v; nothing refers to it", got, err)
	}
	if _, err := s.Referrers(ctx, id, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Referrers(gone) = %v, want ErrNotFound", err)
	}
}
