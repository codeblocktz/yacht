package app

import (
	"context"
	"errors"
	"testing"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// An app is told which environment it runs in, and the ids Railway gives
// beside the names: one image can then tell staging from production, and
// the service in staging is told it is the same service, in staging.
func TestThePlatformVariablesNameTheEnvironment(t *testing.T) {
	ctx := context.Background()
	s, _, id := stagingService(t, "svc-platform-env", Options{PlatformVariablePrefix: "DATAHURU_"})
	p, err := s.CreateProject(ctx, id, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, id, CreateInput{
		// No replicas: capacity is counted across the install, and the room
		// these took would be room another test expects to find.
		Name: "web", Image: "nginx:alpine", Replicas: 0, Port: 8080, ProjectID: p.ID,
	}); err != nil {
		t.Fatal(err)
	}
	env, _, err := s.CreateEnvironment(ctx, id, p.Slug, "Staging", DefaultEnvironment)
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	stagingID, err := s.q.GetEnvironment(ctx, dbgen.GetEnvironmentParams{OwnerID: id, ProjectID: p.ID, Slug: env.Slug})
	if err != nil {
		t.Fatal(err)
	}

	listed := func(name string) (App, map[string]string) {
		t.Helper()
		a, err := s.Get(ctx, id, name)
		if err != nil {
			t.Fatal(err)
		}
		vars, err := s.PlatformVariables(ctx, id, name)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, v := range vars {
			out[v.Key] = v.Value
		}
		return a, out
	}

	web, prod := listed("web")
	webStaging, staging := listed("web-staging")
	for _, c := range []struct {
		vars       map[string]string
		key, value string
	}{
		{prod, "DATAHURU_ENVIRONMENT_NAME", "production"},
		{prod, "DATAHURU_ENVIRONMENT_ID", productionEnvironmentID(p.ID).String()},
		{prod, "DATAHURU_SERVICE_NAME", "web"},
		{prod, "DATAHURU_SERVICE_ID", web.ID.String()},
		{prod, "DATAHURU_PROJECT_NAME", "Shop"},
		{prod, "DATAHURU_PROJECT_ID", p.ID.String()},
		{staging, "DATAHURU_ENVIRONMENT_NAME", "staging"},
		{staging, "DATAHURU_ENVIRONMENT_ID", stagingID.ID.String()},
		{staging, "DATAHURU_SERVICE_NAME", "web"},
		{staging, "DATAHURU_SERVICE_ID", webStaging.ID.String()},
		{staging, "DATAHURU_PROJECT_ID", p.ID.String()},
		{staging, "DATAHURU_PRIVATE_DOMAIN", "web-staging." + webStaging.Namespace + ".svc.cluster.local"},
	} {
		if got := c.vars[c.key]; got != c.value {
			t.Errorf("%s = %q, want %q", c.key, got, c.value)
		}
	}
	if prod["DATAHURU_ENVIRONMENT_ID"] == staging["DATAHURU_ENVIRONMENT_ID"] {
		t.Error("production and staging have the same environment id")
	}
	// Another project's production is another environment.
	other, err := s.CreateProject(ctx, id, "Blog")
	if err != nil {
		t.Fatal(err)
	}
	if productionEnvironmentID(other.ID) == productionEnvironmentID(p.ID) {
		t.Error("two projects' production share an id")
	}
	// Each is a name a reference may use, like the rest of the platform's.
	if !s.isPlatformKey("DATAHURU_ENVIRONMENT_NAME") || !s.isPlatformKey("DATAHURU_PROJECT_ID") {
		t.Error("the new names are not platform keys")
	}
}

// A Raw Editor text refused for one line says which, whether the line does
// not parse or names something that is not there.
func TestARawEditRefusalNamesItsLine(t *testing.T) {
	ctx := context.Background()
	s, _, id := stagingService(t, "svc-raw-lines", Options{})
	if _, err := s.Create(ctx, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		raw  string
		line int
	}{
		{"A=1\n\n# a comment\nB=2\nthis is not a valid line\n", 5},
		{"A=1\nB=\"two\nlines\"\nREF=${{nowhere.KEY}}\n", 4},
		{"A=1\nQ='never closed\nC=3\n", 2},
	} {
		err := s.SetRawVariables(ctx, id, "web", c.raw)
		var le *EnvLineError
		if !errors.As(err, &le) || le.Line != c.line {
			t.Errorf("SetRawVariables(%q) = %v, want a refusal of line %d", c.raw, err, c.line)
		}
	}
}
