package app

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestExposedPortReadsTheImagesStage(t *testing.T) {
	for _, tc := range []struct {
		name, dockerfile string
		want             int32
	}{
		{"one stage", "FROM node:22\nEXPOSE 3000\nCMD [\"node\", \"server.js\"]\n", 3000},
		{"with a protocol", "FROM nginx\nEXPOSE 8080/tcp 9090\n", 8080},
		{"the last stage's", "FROM node AS assets\nEXPOSE 5173\nFROM debian AS runtime\nENV PORT=4000\nEXPOSE 4000\n", 4000},
		{"an earlier stage's is not the image's", "FROM node AS build\nEXPOSE 3000\nFROM nginx\n", 0},
		{"lowercase, continued", "from alpine\nexpose \\\n  4321\n", 4321},
		{"a variable is not a number", "FROM alpine\nEXPOSE ${PORT}\n", 0},
		{"none", "FROM alpine\nCMD [\"sh\"]\n", 0},
		{"a comment", "FROM alpine\n# EXPOSE 1234\nEXPOSE 5000\n", 5000},
		{"out of range", "FROM alpine\nEXPOSE 70000\n", 0},
	} {
		if got := ExposedPort([]byte(tc.dockerfile)); got != tc.want {
			t.Errorf("%s: ExposedPort = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRepoName(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/acme/web.git":            "web",
		"https://github.com/acme/My_Service.git":     "my-service",
		"https://github.com/acme/" + string(make([]byte, 0)) + "---.git": "app",
		"https://github.com/acme/a-very-long-repository-name-that-goes-on-and-on.git": "a-very-long-repository-name-that-goes-on",
	} {
		if got := repoName(url); got != want {
			t.Errorf("repoName(%q) = %q, want %q", url, got, want)
		}
	}
}

// One click: the app is named for the repository, sent traffic on the port
// its Dockerfile exposes, built from GitHub, in the project — and a second
// click is a second app rather than a refusal.
func TestDeployFromGitHub(t *testing.T) {
	ctx := context.Background()
	s, fake := githubFixture(t, nil)
	team := owner(t, s, s.pool, "gh-oneclick")
	connect(t, s, team, 7)
	fake.mu.Lock()
	fake.files["acme/web/Dockerfile"] = "FROM node AS assets\nEXPOSE 5173\nFROM debian\nEXPOSE 4000\n"
	fake.mu.Unlock()
	project, err := s.CreateProject(ctx, team, "dominion")
	if err != nil {
		t.Fatal(err)
	}

	a, err := s.DeployFromGitHub(ctx, team, GitHubRef{InstallationID: 7, RepoID: webRepo.ID}, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "web" || a.Port != 4000 || a.ProjectID != project.ID || a.Source != SourceGit ||
		a.Repo.URL != "https://github.com/acme/web.git" || a.Replicas != 1 {
		t.Errorf("app = name %q port %d project %s source %s repo %q replicas %d",
			a.Name, a.Port, a.ProjectID, a.Source, a.Repo.URL, a.Replicas)
	}

	again, err := s.DeployFromGitHub(ctx, team, GitHubRef{InstallationID: 7, RepoID: webRepo.ID}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != "web-2" {
		t.Errorf("a second deploy of the same repository is %q, want web-2", again.Name)
	}

	// No Dockerfile: the default port, which the app is given as PORT.
	api, err := s.DeployFromGitHub(ctx, team, GitHubRef{InstallationID: 7, RepoID: otherRepo.ID}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if api.Name != "api" || api.Port != DefaultPort {
		t.Errorf("without a Dockerfile: name %q port %d", api.Name, api.Port)
	}

	// A repository the team's connection does not reach is refused.
	if _, err := s.DeployFromGitHub(ctx, team, GitHubRef{InstallationID: 7, RepoID: 999}, uuid.Nil); err == nil {
		t.Error("a repository the connection does not reach was deployed")
	}
	if _, err := s.DeployFromGitHub(ctx, team, GitHubRef{}, uuid.Nil); err == nil {
		t.Error("no repository was deployed")
	}
}

// Moving an app changes its canvas, and nothing it runs on.
func TestMoveToProject(t *testing.T) {
	ctx := context.Background()
	s, _ := githubFixture(t, nil)
	team := owner(t, s, s.pool, "move-project")
	a, err := s.Create(ctx, team, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, team, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveToProject(ctx, team, "web", p.Slug); err != nil {
		t.Fatal(err)
	}
	moved, err := s.Get(ctx, team, "web")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ProjectID != p.ID || moved.Namespace != a.Namespace {
		t.Errorf("after the move: project %s namespace %s", moved.ProjectID, moved.Namespace)
	}
	if _, err := s.MoveToProject(ctx, team, "web", "nowhere"); err == nil {
		t.Error("moved into a project that does not exist")
	}
	if _, err := s.MoveToProject(ctx, team, "gone", p.Slug); err == nil {
		t.Error("moved an app that does not exist")
	}
}
