package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/secret"
)

func TestAStackNameHasToLeaveRoomForTheAppNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stack string
		ok    bool
	}{
		{"plain", "shop", true},
		{"dashes", "my-shop", true},
		{"empty", "", false},
		{"uppercase", "Shop", false},
		{"underscore", "my_shop", false},
		{"trailing dash", "shop-", false},
		{"too long", strings.Repeat("a", 31), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStackName(tc.stack)
			if tc.ok && err != nil {
				t.Fatalf("ValidateStackName(%q) = %v, want accepted", tc.stack, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateStackName(%q) was accepted", tc.stack)
			}
		})
	}
}

// A template makes several apps, so it cannot be the source of one. Reaching
// Create with it would produce an app running whatever image happened to be in
// the form.
func TestATemplateIsNotASourceAnAppCanHave(t *testing.T) {
	if _, err := BlueprintFor(SourceTemplate); err == nil {
		t.Fatal("a template was accepted as a blueprint for a single app")
	}
}

// Every template must wire only to apps it creates first. Order is the
// dependency here, and a template naming a later app would deploy looking
// complete while missing the connection that is the point of it.
func TestEveryTemplateWiresOnlyToWhatItCreatesFirst(t *testing.T) {
	for _, tmpl := range Templates() {
		made := map[string]bool{}
		for _, a := range tmpl.Apps {
			if a.NeedsFrom != "" && !made[a.NeedsFrom] {
				t.Errorf("template %s wires %s to %s, which it does not create first",
					tmpl.Slug, a.Name, a.NeedsFrom)
			}
			made[a.Name] = true
		}
		if len(tmpl.Apps) == 0 {
			t.Errorf("template %s creates nothing", tmpl.Slug)
		}
	}
}

// The whole point: the consumer ends up holding the provider's connection
// string, and the canvas shows the edge because of it.
func TestDeployingATemplateWiresTheStackTogether(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	ownerID := owner(t, s, pool, "owner-template")

	project, err := s.DeployTemplate(ctx, ownerID, "postgres-app", "shop")
	if err != nil {
		t.Fatalf("DeployTemplate: %v", err)
	}
	if project.Slug != "shop" {
		t.Fatalf("project slug = %q, want shop", project.Slug)
	}

	apps, err := s.ListInProject(ctx, ownerID, project.ID)
	if err != nil {
		t.Fatalf("ListInProject: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("stack has %d apps, want 2", len(apps))
	}

	// Named after the stack, so a second deploy does not collide and the names
	// say what they belong to.
	names := map[string]bool{}
	for _, a := range apps {
		names[a.Name] = true
	}
	for _, want := range []string{"shop-db", "shop-web"} {
		if !names[want] {
			t.Errorf("stack is missing %s — got %v", want, names)
		}
	}

	// The consumer holds the database's connection string, as a secret.
	web, err := s.Get(ctx, ownerID, "shop-web")
	if err != nil {
		t.Fatalf("get shop-web: %v", err)
	}
	var found bool
	for _, v := range web.Variables {
		if v.Key != "DATABASE_URL" {
			continue
		}
		found = true
		if !v.Secret {
			t.Error("the connection string was stored readable")
		}
		if v.Value != "" {
			t.Error("a secret's value came back out of the variables list")
		}
	}
	if !found {
		t.Fatal("shop-web did not receive DATABASE_URL, which is the point of the template")
	}

	// And it points at the database rather than at nothing: the link exists
	// because the value names the other app's in-cluster address.
	links, err := s.Links(ctx, ownerID)
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	var wired bool
	for _, l := range links {
		if l.From == "shop-web" && l.To == "shop-db" && l.Via == "DATABASE_URL" {
			wired = true
		}
	}
	if !wired {
		t.Fatalf("no edge from shop-web to shop-db — links were %v", links)
	}
}

// The same template twice is the case the naming exists for.
func TestTheSameTemplateCanBeDeployedTwice(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	ownerID := owner(t, s, pool, "owner-template-twice")

	if _, err := s.DeployTemplate(ctx, ownerID, "postgres-app", "shop"); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	if _, err := s.DeployTemplate(ctx, ownerID, "postgres-app", "blog"); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	for _, name := range []string{"shop-db", "shop-web", "blog-db", "blog-web"} {
		if _, err := s.Get(ctx, ownerID, name); err != nil {
			t.Errorf("%s is missing after two deploys: %v", name, err)
		}
	}
}

// Each stack gets its own canvas, so two stacks are two pictures rather than
// one crowded one.
func TestEachStackGetsItsOwnProject(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	ownerID := owner(t, s, pool, "owner-template-projects")

	shop, err := s.DeployTemplate(ctx, ownerID, "postgres-app", "shop")
	if err != nil {
		t.Fatalf("deploy shop: %v", err)
	}
	blog, err := s.DeployTemplate(ctx, ownerID, "postgres-app", "blog")
	if err != nil {
		t.Fatalf("deploy blog: %v", err)
	}
	if shop.ID == blog.ID {
		t.Fatal("both stacks landed on one canvas")
	}

	for _, p := range []Project{shop, blog} {
		apps, err := s.ListInProject(ctx, ownerID, p.ID)
		if err != nil {
			t.Fatalf("ListInProject %s: %v", p.Slug, err)
		}
		if len(apps) != 2 {
			t.Errorf("project %s holds %d apps, want its own 2", p.Slug, len(apps))
		}
	}
}

// A stack whose credentials cannot be sealed is refused before anything is
// created, not left half-deployed.
func TestATemplateNeedingSecretsIsRefusedWithNoKey(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-template-nokey")

	if _, err := s.DeployTemplate(ctx, ownerID, "postgres-app", "shop"); err == nil {
		t.Fatal("a stack with generated credentials deployed with no key to seal them")
	}
	if _, err := s.Get(ctx, ownerID, "shop-web"); err == nil {
		t.Error("the stack was left half-deployed")
	}
}

func TestAnUnknownTemplateIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	ownerID := owner(t, s, pool, "owner-template-unknown")

	if _, err := s.DeployTemplate(ctx, ownerID, "nope", "shop"); err != ErrTemplateNotFound {
		t.Fatalf("error = %v, want ErrTemplateNotFound", err)
	}
}

// testKeeper returns a keeper for tests that need secrets sealed.
func testKeeper(t *testing.T) *secret.Keeper {
	t.Helper()
	k, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	return k
}
