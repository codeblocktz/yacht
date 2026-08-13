package app

import (
	"context"
	"testing"
)

func TestSlugifyBuildsAnAddressFromAName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Billing", "billing"},
		{"  Billing   Service ", "billing-service"},
		{"Café & Co.", "caf-co"},
		{"---", ""},
		{"...", ""},
	} {
		if got := Slugify(tc.in); got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A name with nothing to build an address from is refused rather than given a
// generated one. A URL with no relationship to what somebody typed is a URL
// they will not recognise as theirs.
func TestAProjectNeedsSomethingToAddressItBy(t *testing.T) {
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-name")

	if _, err := s.CreateProject(context.Background(), ownerID, "!!!"); err == nil {
		t.Fatal("a project named entirely of punctuation was accepted")
	}
}

// The default project is created on demand and adopts anything unassigned.
//
// This is what lets an install that predates projects open the page and see its
// apps, rather than an empty canvas with no way to explain where they went.
func TestTheDefaultProjectAdoptsAppsThatHaveNone(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-adopt")

	if _, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "orphan", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Written straight to the column, because Create has no way to make an app
	// that predates the projects table — which is precisely the case at issue.
	if _, err := pool.Exec(ctx,
		`UPDATE apps SET project_id = NULL WHERE owner_id = $1`, ownerID); err != nil {
		t.Fatalf("unassign: %v", err)
	}

	projects, err := s.Projects(ctx, ownerID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 || projects[0].Slug != DefaultProjectSlug {
		t.Fatalf("projects = %+v, want one default project", projects)
	}
	if projects[0].Apps != 1 {
		t.Fatalf("default project holds %d apps, want the orphan", projects[0].Apps)
	}

	apps, err := s.ListInProject(ctx, ownerID, projects[0].ID)
	if err != nil {
		t.Fatalf("list apps in project: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "orphan" {
		t.Fatalf("apps in default project = %+v, want the orphan", apps)
	}
}

// A position survives the round trip, and clearing gives it back to the layout.
//
// The distinction that matters is nil versus zero: (0,0) is a corner somebody
// can drag a card to, and treating it as "never moved" would re-lay-out a card
// that was deliberately pinned there.
func TestAPositionIsStoredAndCanBeGivenBack(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-project-pos")

	if _, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	project, err := s.DefaultProject(ctx, ownerID)
	if err != nil {
		t.Fatalf("default project: %v", err)
	}

	if err := s.SetPosition(ctx, ownerID, "web", 0, 0); err != nil {
		t.Fatalf("set position: %v", err)
	}
	a, err := s.Get(ctx, ownerID, "web")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.X == nil || a.Y == nil {
		t.Fatal("a card pinned to the origin came back as never having been moved")
	}
	if *a.X != 0 || *a.Y != 0 {
		t.Fatalf("position = (%d,%d), want (0,0)", *a.X, *a.Y)
	}

	if err := s.ClearPositions(ctx, ownerID, project.ID); err != nil {
		t.Fatalf("clear positions: %v", err)
	}
	if a, err = s.Get(ctx, ownerID, "web"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.X != nil || a.Y != nil {
		t.Fatalf("position survived a clear: (%v,%v)", a.X, a.Y)
	}
}

// Positions are scoped by owner, like everything else.
func TestAnotherTeamCannotMoveYourCards(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	mine := owner(t, s, pool, "owner-project-mine")
	theirs := owner(t, s, pool, "owner-project-theirs")

	if _, err := createAndDeploy(t, s, ctx, mine, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	if err := s.SetPosition(ctx, theirs, "web", 10, 20); err == nil {
		t.Fatal("another team moved a card on a canvas that is not theirs")
	}
}
