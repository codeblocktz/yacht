package app

import (
	"context"
	"fmt"
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

// A pull request against a repository a project builds gets an environment:
// production copied, the repository's services building the pull request's
// branch. Closing it takes the environment down; a fork's is never built.
func TestAPullRequestGetsAnEnvironment(t *testing.T) {
	ctx := context.Background()
	s, _ := githubFixture(t, nil)
	team := owner(t, s, s.pool, "pr-environments")
	connect(t, s, team, 7)
	p, err := s.CreateProject(ctx, team, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeployFromGitHub(ctx, team, GitHubRef{InstallationID: 7, RepoID: webRepo.ID}, p.ID); err != nil {
		t.Fatalf("DeployFromGitHub: %v", err)
	}
	event := func(action string, headRepo int64) []byte {
		return []byte(fmt.Sprintf(`{"action":%q,"number":7,
			"pull_request":{"head":{"ref":"feature/cart","repo":{"id":%d}},"base":{"ref":"main","repo":{"id":%d}}},
			"repository":{"id":%d,"full_name":"acme/web"},"installation":{"id":7}}`,
			action, headRepo, webRepo.ID, webRepo.ID))
	}

	// Off until the project turns it on.
	if got, _ := s.gitHubPullRequest(ctx, event("opened", webRepo.ID)); !strings.Contains(got, "No project") {
		t.Errorf("with PR environments off: %q", got)
	}
	if err := s.SetPREnvironments(ctx, team, p.Slug, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.gitHubPullRequest(ctx, event("opened", 999)); !strings.Contains(got, "fork") {
		t.Errorf("a fork's pull request: %q", got)
	}
	if got, err := s.gitHubPullRequest(ctx, event("opened", webRepo.ID)); err != nil || !strings.Contains(got, "pr-7 made") {
		t.Fatalf("opened: %q, %v", got, err)
	}
	copy, err := s.Get(ctx, team, "web-pr-7")
	if err != nil {
		t.Fatalf("the pull request's copy: %v", err)
	}
	if copy.Environment() != "pr-7" || copy.Repo.Branch != "feature/cart" {
		t.Errorf("the copy is in %s building %q", copy.Environment(), copy.Repo.Branch)
	}
	if got, _ := s.gitHubPullRequest(ctx, event("closed", webRepo.ID)); !strings.Contains(got, "taken down") {
		t.Errorf("closed: %q", got)
	}
	if _, err := s.Get(ctx, team, "web-pr-7"); err == nil {
		t.Error("the pull request's copy survived it closing")
	}
}

// A copy's app is named <service>-<env>, but it is still the service: its
// generated domain is <service>-<env>, not the env appended twice, and it is
// told the service's name, the same in every environment (CONFIG-12).
func TestACopyIsNamedForItsService(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := testService(t, Options{AppDomain: "apps.copies.test"})
	id := owner(t, s, pool, "svc-environments-copy-names")
	p, err := s.CreateProject(ctx, id, "Echo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "echo-a", Image: "nginx:alpine", Replicas: 1, Port: 8080, ProjectID: p.ID,
	}); err != nil {
		t.Fatalf("create echo-a: %v", err)
	}
	if prod := publicApp(t, s, id, "echo-a"); prod.Host != "echo-a-production.up.apps.copies.test" {
		t.Fatalf("production's host = %q", prod.Host)
	}

	_, copies, err := s.CreateEnvironment(ctx, id, p.Slug, "Staging", DefaultEnvironment)
	if err != nil || len(copies) != 1 || copies[0].Err != nil {
		t.Fatalf("CreateEnvironment: %+v, %v", copies, err)
	}
	const want = "echo-a-staging.up.apps.copies.test"
	if !strings.Contains(copies[0].Note, "reachable at "+want) {
		t.Errorf("the copy's note = %q, want it reachable at %s", copies[0].Note, want)
	}
	if err := executeNamedOperation(s, ctx, id, "echo-a-staging"); err != nil {
		t.Fatalf("deploy echo-a-staging: %v", err)
	}
	env := orch.lastAppSpec().Env
	if got := env[DefaultPlatformVariablePrefix+PlatformServiceName]; got != "echo-a" {
		t.Errorf("the copy's service name = %q, want echo-a", got)
	}
	if got := env[DefaultPlatformVariablePrefix+PlatformPublicDomain]; got != want {
		t.Errorf("the copy's public domain = %q, want %s", got, want)
	}
}
