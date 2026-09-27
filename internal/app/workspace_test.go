package app

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// An app's state is read off its records in a fixed order: what somebody is
// waiting on first, then what somebody chose, then how the last attempt went.
func TestAppStateIsReadOffTheRecords(t *testing.T) {
	for _, c := range []struct {
		name       string
		deploying  bool
		replicas   int32
		serving    bool
		lastDeploy string
		want       AppState
	}{
		{"a deploy in flight wins", true, 0, true, DeployFailed, AppDeploying},
		{"scaled to zero is stopped whatever happened before", false, 0, true, DeploySucceeded, AppStopped},
		{"a failed change on a serving app", false, 1, true, DeployFailed, AppDeployFailed},
		{"failed and never served", false, 1, false, DeployFailed, AppFailed},
		{"serving", false, 2, true, DeploySucceeded, AppServing},
		{"a legacy active row is serving", false, 1, true, DeployActive, AppServing},
		{"never deployed", false, 1, false, "", AppIdle},
	} {
		if got := appStateOf(c.deploying, c.replicas, c.serving, c.lastDeploy); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Apps hang off their own project, in the order the query returned them, and
// an app whose project is not in the list is kept rather than dropped.
func TestAWorkspacePlacesEveryApp(t *testing.T) {
	shop, blog := uuid.New(), uuid.New()
	w := assembleWorkspace(
		[]dbgen.WorkspaceProjectsRow{{ID: blog, Slug: "blog", Name: "Blog"}, {ID: shop, Slug: "shop", Name: "Shop"}},
		[]dbgen.WorkspaceAppsRow{
			{Name: "api", ProjectID: pgtype.UUID{Bytes: shop, Valid: true}, Replicas: 1, Serving: true},
			{Name: "cms", ProjectID: pgtype.UUID{Bytes: blog, Valid: true}, Replicas: 1, Deploying: true},
			{Name: "old", Replicas: 1},
			{Name: "web", ProjectID: pgtype.UUID{Bytes: shop, Valid: true}, Replicas: 0},
		},
		dbgen.WorkspaceFactsRow{LiveDeploys: 1, HasApps: true},
	)

	if len(w.Projects) != 2 || w.Projects[0].Slug != "blog" || w.Projects[1].Slug != "shop" {
		t.Fatalf("projects = %+v", w.Projects)
	}
	if got := w.Projects[1].Apps; len(got) != 2 || got[0].Name != "api" || got[1].Name != "web" ||
		got[1].State != AppStopped {
		t.Errorf("shop apps = %+v", got)
	}
	if got := w.Projects[0].Apps; len(got) != 1 || got[0].State != AppDeploying {
		t.Errorf("blog apps = %+v", got)
	}
	if len(w.Unplaced) != 1 || w.Unplaced[0].Name != "old" {
		t.Errorf("unplaced = %+v, want the app with no project", w.Unplaced)
	}
	if w.LiveDeploys != 1 || !w.Onboarding.HasApps {
		t.Errorf("facts not carried: %+v", w)
	}
}

// Against the database: what a team has is what its workspace says, and
// nothing of another team's leaks into it.
func TestTheWorkspaceIsTheTeamsOwn(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	mine := owner(t, s, pool, "owner-workspace-mine")
	theirs := owner(t, s, pool, "owner-workspace-theirs")

	// A fresh team has nothing, and reading that must not create anything —
	// a default project conjured by the sidebar would be a write on a GET.
	empty, err := s.Workspace(ctx, mine)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if len(empty.Projects) != 0 || empty.LiveDeploys != 0 || empty.Onboarding != (Onboarding{}) {
		t.Fatalf("a fresh team's workspace is not empty: %+v", empty)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE owner_id = $1`, mine).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reading the workspace created %d projects (err %v)", n, err)
	}

	shop, err := s.CreateProject(ctx, mine, "Shop")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// One deployed and serving, one only queued: the queued one is the deploy
	// in flight the badge counts.
	if _, err := createAndDeploy(t, s, ctx, mine, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80, ProjectID: shop.ID,
	}); err != nil {
		t.Fatalf("create web: %v", err)
	}
	if _, err := s.Create(ctx, mine, CreateInput{
		Name: "api", Image: "nginx:alpine", Replicas: 1, Port: 80, ProjectID: shop.ID,
	}); err != nil {
		t.Fatalf("create api: %v", err)
	}
	// The other team deploys too, and has a routed custom domain.
	other, err := createAndDeploy(t, s, ctx, theirs, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("create theirs: %v", err)
	}
	if _, err := s.Create(ctx, theirs, CreateInput{
		Name: "queued", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("create theirs queued: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO domains (owner_id, app_id, host, tls, state, managed)
		VALUES ($1, $2, 'theirs.example.com', true, 'routed', false)`, theirs, other.ID); err != nil {
		t.Fatalf("insert their domain: %v", err)
	}

	w, err := s.Workspace(ctx, mine)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if len(w.Projects) != 1 || w.Projects[0].Slug != shop.Slug {
		t.Fatalf("projects = %+v, want only Shop", w.Projects)
	}
	apps := w.Projects[0].Apps
	if len(apps) != 2 || apps[0].Name != "api" || apps[1].Name != "web" {
		t.Fatalf("apps = %+v, want api and web", apps)
	}
	if apps[0].State != AppDeploying || apps[1].State != AppServing {
		t.Errorf("states = %q, %q; want deploying, serving", apps[0].State, apps[1].State)
	}
	if w.LiveDeploys != 1 {
		t.Errorf("live deploys = %d, want 1 — the other team's queued deploy leaked in or ours was missed", w.LiveDeploys)
	}
	want := Onboarding{HasApps: true, HasSucceededDeploy: true}
	if w.Onboarding != want {
		t.Errorf("onboarding = %+v, want %+v — their custom domain is not ours", w.Onboarding, want)
	}

	theirsW, err := s.Workspace(ctx, theirs)
	if err != nil {
		t.Fatalf("Workspace theirs: %v", err)
	}
	if !theirsW.Onboarding.HasCustomDomain {
		t.Error("a routed custom domain was not counted")
	}
}

// Dismissing is the team's, it survives, and it works for an owner with no
// team row yet — a single-owner install that has not deployed anything.
func TestDismissingTheChecklistIsRemembered(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	mine := owner(t, s, pool, "owner-onboarding-dismiss")
	other := owner(t, s, pool, "owner-onboarding-other")

	if err := s.DismissOnboarding(ctx, mine); err != nil {
		t.Fatalf("DismissOnboarding: %v", err)
	}
	// Twice is fine: a double-submitted form is not an error.
	if err := s.DismissOnboarding(ctx, mine); err != nil {
		t.Fatalf("DismissOnboarding again: %v", err)
	}
	w, err := s.Workspace(ctx, mine)
	if err != nil || !w.Onboarding.Dismissed {
		t.Fatalf("dismissal not remembered: %+v (err %v)", w.Onboarding, err)
	}
	if w, _ := s.Workspace(ctx, other); w.Onboarding.Dismissed {
		t.Error("one team dismissing the checklist dismissed another's")
	}

	const fresh = "owner-onboarding-no-row"
	if _, err := pool.Exec(ctx, `DELETE FROM teams WHERE id = $1`, fresh); err != nil {
		t.Fatalf("purge: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM teams WHERE id = $1`, fresh) })
	if err := s.DismissOnboarding(ctx, fresh); err != nil {
		t.Fatalf("dismiss with no team row: %v", err)
	}
	if w, _ := s.Workspace(ctx, fresh); !w.Onboarding.Dismissed {
		t.Error("dismissal with no team row was lost")
	}
}
