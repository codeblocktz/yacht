package app

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// aliasesOf is a network's aliases as name → host.
func aliasesOf(n orchestrator.PrivateNetwork) map[string]string {
	out := map[string]string{}
	for _, a := range n.Aliases {
		out[a.Name] = a.Host
	}
	return out
}

// Inside a project and environment each service reaches the others by name;
// staging's names are staging's; another project's or team's services have
// none; and an app in another project that refers to one is let in.
func TestPrivateNetworksAreAProjectsAndEnvironments(t *testing.T) {
	ctx := context.Background()
	s, orch, id := stagingService(t, "svc-privnet", Options{})
	_, _, other := stagingService(t, "svc-privnet-other", Options{})

	shop, err := s.CreateProject(ctx, id, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	blog, err := s.CreateProject(ctx, id, "Blog")
	if err != nil {
		t.Fatal(err)
	}
	deploy := func(owner string, in CreateInput) App {
		t.Helper()
		// No replicas: capacity is counted across the install.
		in.Image, in.Replicas = "nginx:alpine", 0
		a, err := createAndDeploy(t, s, ctx, owner, in)
		if err != nil {
			t.Fatalf("create %s: %v", in.Name, err)
		}
		return a
	}
	web := deploy(id, CreateInput{Name: "web", Port: 8080, ProjectID: shop.ID})
	db := deploy(id, CreateInput{Name: "db", Port: 5432, ProjectID: shop.ID})
	worker := deploy(id, CreateInput{Name: "worker", ProjectID: shop.ID}) // no port
	api := deploy(id, CreateInput{Name: "api", Port: 3000, ProjectID: blog.ID})
	theirs := deploy(other, CreateInput{Name: "cache", Port: 6379})
	if _, _, err := s.CreateEnvironment(ctx, id, shop.Slug, "Staging", DefaultEnvironment); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	// Blog's api uses Shop's database, by reference: the link that lets it in.
	if err := s.SetVariable(ctx, id, "api", VariableInput{Key: "DB_HOST", Value: "${{db.PRIVATE_HOST}}"}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}

	networks, err := s.PrivateNetworks(ctx)
	if err != nil {
		t.Fatalf("PrivateNetworks: %v", err)
	}
	byNS := map[string]orchestrator.PrivateNetwork{}
	for _, n := range networks {
		byNS[n.Namespace] = n
	}
	webStaging, err := s.Get(ctx, id, "web-staging")
	if err != nil {
		t.Fatal(err)
	}
	dbStaging, err := s.Get(ctx, id, "db-staging")
	if err != nil {
		t.Fatal(err)
	}

	check := func(a App, project string, aliases map[string]string, peers []string) {
		t.Helper()
		n, ok := byNS[a.Namespace]
		if !ok {
			t.Fatalf("%s has no private network", a.Name)
		}
		if n.Owner != orchestrator.OwnerID(a.OwnerID) || n.Project != project {
			t.Errorf("%s: owner %s project %q, want %s %q", a.Name, n.Owner, n.Project, a.OwnerID, project)
		}
		if got := aliasesOf(n); !maps.Equal(got, aliases) {
			t.Errorf("%s's aliases = %v, want %v", a.Name, got, aliases)
		}
		if !slices.Equal(n.Peers, peers) {
			t.Errorf("%s's peers = %v, want %v", a.Name, n.Peers, peers)
		}
	}
	// web is its own Service's name, so it has no alias of its own; worker
	// listens on nothing, so it has none in anybody's namespace.
	check(web, shop.ID.String(), map[string]string{"db": privateHost(db)}, nil)
	check(db, shop.ID.String(), map[string]string{"web": privateHost(web)}, []string{api.Namespace})
	check(worker, shop.ID.String(), map[string]string{"web": privateHost(web), "db": privateHost(db)}, nil)
	// Staging's web reaches staging's db as db, and itself as web.
	check(webStaging, shop.ID.String(), map[string]string{
		"web": privateHost(webStaging), "db": privateHost(dbStaging),
	}, nil)
	check(api, blog.ID.String(), map[string]string{}, nil)
	// Another team's app is in its own default project, with names of its own.
	if n := byNS[theirs.Namespace]; n.Owner != orchestrator.OwnerID(other) || len(n.Aliases) != 0 {
		t.Errorf("the other team's network = %+v", n)
	}

	// A pass writes what was deployed, and a deploy labelled it first.
	if err := s.SyncPrivateNetworks(ctx); err != nil {
		t.Fatalf("SyncPrivateNetworks: %v", err)
	}
	got, ok := orch.PrivateNetwork(db.Namespace)
	if !ok || !maps.Equal(aliasesOf(got), map[string]string{"web": privateHost(web)}) ||
		!slices.Equal(got.Peers, []string{api.Namespace}) {
		t.Errorf("db's synced network = %+v, %v", got, ok)
	}
	if spec, _ := orch.Namespace(web.Namespace); spec.Project != shop.ID.String() {
		t.Errorf("web's namespace was ensured with project %q, want %s", spec.Project, shop.ID)
	}
}

// An app on no project's canvas, in a team that has never had its default
// project made, is grouped with the team's other such apps, and with no
// project's: they reach each other by name.
func TestAppsOnNoProjectAreGroupedTogether(t *testing.T) {
	ctx := context.Background()
	s, orch, id := stagingService(t, "svc-privnet-default", Options{})
	solo, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "solo", Image: "nginx:alpine", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	pal, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "pal", Image: "nginx:alpine", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	if spec, _ := orch.Namespace(solo.Namespace); spec.Project != noProject {
		t.Errorf("namespace project = %q, want %q", spec.Project, noProject)
	}
	if err := s.SyncPrivateNetworks(ctx); err != nil {
		t.Fatalf("SyncPrivateNetworks: %v", err)
	}
	n, _ := orch.PrivateNetwork(solo.Namespace)
	if n.Project != noProject || !maps.Equal(aliasesOf(n), map[string]string{"pal": privateHost(pal)}) {
		t.Errorf("solo's network = %+v, want pal's name, on no project", n)
	}
}
