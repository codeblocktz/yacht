package app

import (
	"context"
	"strings"
	"testing"
)

// A staging environment copied from production is every service again,
// wired to its own copies: a database's connection string becomes a
// reference to staging's database, an address of a production service is
// rewritten to its copy's, and nothing in staging reaches production.
func TestAnEnvironmentIsACopyWiredToItself(t *testing.T) {
	ctx := context.Background()
	s, orch, id := referenceService(t, "svc-environments")
	p, err := s.CreateProject(ctx, id, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Source: SourcePostgres, Name: "db", ProjectID: p.ID,
	}); err != nil {
		t.Fatalf("create db: %v", err)
	}
	db, err := s.Get(ctx, id, "db")
	if err != nil {
		t.Fatal(err)
	}
	dbURL, err := s.variableValue(ctx, id, db.ID, "DATABASE_URL")
	if err != nil || dbURL == "" {
		t.Fatalf("db's URL = %q, %v", dbURL, err)
	}
	// The way dominion was wired before references: a copy of the URL.
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080, ProjectID: p.ID,
		Variables: []VariableInput{
			{Key: "DATABASE_URL", Value: dbURL, Secret: true},
			{Key: "DB_HOST", Value: "db." + db.Namespace + ".svc.cluster.local"},
			{Key: "CACHE", Value: "${{db.PRIVATE_HOST}}"},
			{Key: "LOG_LEVEL", Value: "info"},
		},
	}); err != nil {
		t.Fatalf("create web: %v", err)
	}

	env, copies, err := s.CreateEnvironment(ctx, id, p.Slug, "Staging", DefaultEnvironment)
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.Slug != "staging" || len(copies) != 2 {
		t.Fatalf("environment %+v, copies %+v", env, copies)
	}
	for _, c := range copies {
		if c.Err != nil {
			t.Errorf("copying %s: %v", c.Service, c.Err)
		}
	}

	staging, err := s.ListInEnvironment(ctx, id, p, "staging")
	if err != nil || len(staging) != 2 {
		t.Fatalf("staging = %+v, %v", staging, err)
	}
	web, err := s.Get(ctx, id, "web-staging")
	if err != nil || web.ServiceName() != "web" || web.Environment() != "staging" {
		t.Fatalf("web-staging = %+v, %v", web, err)
	}
	dbStaging, err := s.Get(ctx, id, "db-staging")
	if err != nil {
		t.Fatal(err)
	}

	if err := executeNamedOperation(s, ctx, id, "web-staging"); err != nil {
		t.Fatalf("deploy web-staging: %v", err)
	}
	spec := orch.lastAppSpec()
	stagingHost := "db-staging." + dbStaging.Namespace
	for key, want := range map[string]string{"DATABASE_URL": stagingHost, "CACHE": stagingHost} {
		if got := spec.Secrets[key]; !strings.Contains(got, stagingHost) || strings.Contains(got, "db."+db.Namespace) {
			t.Errorf("%s in staging = %q, want staging's db (%s)", key, got, want)
		}
	}
	if got := spec.Env["DB_HOST"]; !strings.Contains(got, stagingHost) {
		t.Errorf("DB_HOST in staging = %q, want it rewritten to %s", got, stagingHost)
	}
	if spec.Env["LOG_LEVEL"] != "info" {
		t.Errorf("LOG_LEVEL = %q, want it copied", spec.Env["LOG_LEVEL"])
	}
	if strings.Contains(spec.Secrets["DATABASE_URL"], dbURL) {
		t.Error("staging was given production's database URL")
	}

	// Production is untouched, and both are listed.
	if prod, _ := s.ListInEnvironment(ctx, id, p, DefaultEnvironment); len(prod) != 2 {
		t.Errorf("production has %d apps, want 2", len(prod))
	}
	envs, err := s.Environments(ctx, id, p.Slug)
	if err != nil || len(envs) != 2 || !envs[0].Production || envs[1].Slug != "staging" || envs[1].Apps != 2 {
		t.Errorf("environments = %+v, %v", envs, err)
	}

	// A copy is moved with its service, not alone.
	if _, err := s.MoveToProject(ctx, id, "web-staging", DefaultProjectSlug); err == nil {
		t.Error("a staging copy was moved out of its environment")
	}

	if err := s.DeleteEnvironment(ctx, id, p.Slug, DefaultEnvironment); err == nil {
		t.Error("production was deleted")
	}
	if err := s.DeleteEnvironment(ctx, id, p.Slug, "staging"); err != nil {
		t.Fatalf("DeleteEnvironment: %v", err)
	}
	if _, err := s.Get(ctx, id, "web-staging"); err == nil {
		t.Error("staging's web survived its environment")
	}
	if _, err := s.Get(ctx, id, "web"); err != nil {
		t.Errorf("production's web went with staging: %v", err)
	}
}

// An empty environment is just the environment; its name has to be one.
func TestAnEnvironmentNeedsAName(t *testing.T) {
	ctx := context.Background()
	s, _, id := referenceService(t, "svc-environments-names")
	p, err := s.CreateProject(ctx, id, "Blog")
	if err != nil {
		t.Fatal(err)
	}
	if _, copies, err := s.CreateEnvironment(ctx, id, p.Slug, "Preview", ""); err != nil || len(copies) != 0 {
		t.Fatalf("empty environment: %v, %v", copies, err)
	}
	for _, bad := range []string{"", "production", "Preview"} {
		if _, _, err := s.CreateEnvironment(ctx, id, p.Slug, bad, ""); err == nil {
			t.Errorf("CreateEnvironment(%q) was accepted", bad)
		}
	}
	if _, _, err := s.CreateEnvironment(ctx, id, p.Slug, "qa", "nowhere"); err == nil {
		t.Error("copied from an environment that does not exist")
	}
}
