package web

import (
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The states the GitHub App adds. Reaching them by clicking needs an App
// registered on GitHub and an account to install it on.
func githubGalleryPages() []galleryPage {
	now := time.Now()
	conns := []app.GitHubConnection{
		{InstallationID: 101, Account: "example-org", AccountType: "Organization",
			ConnectedBy: "owner@example.test", ConnectedAs: "octo-owner", ConnectedAt: now.Add(-48 * time.Hour)},
		{InstallationID: 102, Account: "octo-owner", AccountType: "User",
			ConnectedBy: "owner@example.test", ConnectedAs: "octo-owner", ConnectedAt: now.Add(-3 * time.Hour),
			Suspended: true},
		{InstallationID: 103, Account: "old-org", AccountType: "Organization",
			ConnectedBy: "admin@example.test", ConnectedAs: "octo-admin", ConnectedAt: now.Add(-30 * 24 * time.Hour),
			Removed: true},
	}

	connected := app.App{
		ID: uuid.New(), OwnerID: "owner-local", Name: "web", Namespace: app.Namespace("owner-local", "web"),
		Image: "registry.example.test/owner-local-web:d0ed801", Replicas: 1, Port: 8080,
		Source: app.SourceGit, StatusKnown: true,
		Status: orchestrator.AppStatus{Phase: orchestrator.PhaseRunning, Desired: 1, Ready: 1},
		Repo: app.Repo{URL: "https://github.com/example-org/web.git", Branch: "main",
			GitHub: app.GitHubRef{InstallationID: 101, RepoID: 9001}},
		CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	paused := connected
	paused.GitHubPaused = app.GitHubPausedRepoRemoved

	repos := []app.GitHubRepo{
		{Ref: app.GitHubRef{InstallationID: 101, RepoID: 9001}, FullName: "example-org/web", Private: true, Account: "example-org"},
		{Ref: app.GitHubRef{InstallationID: 101, RepoID: 9002}, FullName: "example-org/api", Private: true, Account: "example-org"},
		{Ref: app.GitHubRef{InstallationID: 102, RepoID: 9003}, FullName: "octo-owner/site", Account: "octo-owner"},
	}

	settings := func(a app.App, hook app.Hook) AppDetailData {
		d := settingsGallery(a, 2000, 2<<30)
		d.GitHubOn, d.HooksOn, d.Hook = true, true, hook
		return d
	}

	return []galleryPage{
		{
			file: "states-github-team.html", path: "/team",
			crumbs: []Crumb{{Label: "Team"}},
			page: stack(
				section("Nothing connected, as an admin", "what connecting does, and both ways in",
					teamGitHubPanel(TeamGitHubData{CanManage: true})),
				section("Nothing connected, as a member", "who can connect it",
					teamGitHubPanel(TeamGitHubData{})),
				section("Connected", "an organisation, a suspended user installation, and one uninstalled on GitHub",
					teamGitHubPanel(TeamGitHubData{Connections: conns, CanManage: true})),
			),
		},
		{
			file: "states-github-choose.html", path: GitHubCallbackPath,
			crumbs: []Crumb{{Label: "Team", Href: "/team"}, {Label: "Connect GitHub"}},
			page: GitHubChoosePage([]app.GitHubChoice{
				{InstallationID: 101, Account: "example-org", AccountType: "Organization", Token: "x"},
				{InstallationID: 102, Account: "octo-owner", AccountType: "User", Token: "y"},
			}),
		},
		{
			file: "states-github-source.html", path: "/apps/web/settings",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "web"}, {Label: "Settings"}},
			page: stack(
				section("Chosen from GitHub", "the picker, with the URL field for a public repository beside it",
					appRuntimeForm(settings(connected, app.Hook{}))),
				section("Paused", "the installation no longer reaches the repository",
					appRuntimeForm(settings(paused, app.Hook{}))),
				section("Deploy on push, through the App", "no webhook to set up; the last push",
					appPushSection(settings(connected, app.Hook{
						LastDelivery: "Deploying 9e41d05.", LastDeliveryAt: now.Add(-2 * time.Minute),
					}))),
			),
		},
		{
			// The picker once GitHub has answered: what was chosen, what the
			// installations reach, and a choice they no longer do.
			file: "states-github-picker.html", path: "/apps/web/settings",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "web"}, {Label: "Settings"}},
			page: stack(
				section("Loaded", "the team's repositories, the chosen one marked",
					GitHubRepoSelect(GitHubRepoData{Repos: repos, Selected: "101:9001"})),
				section("No longer reachable", "kept chosen, and said",
					GitHubRepoSelect(GitHubRepoData{Repos: repos[1:], Selected: "101:9001",
						SelectedLabel: "example-org/web", Problems: []string{"octo-owner: the GitHub App's installation is suspended"}})),
				section("Nothing connected", "where to connect it",
					GitHubRepoSelect(GitHubRepoData{})),
			),
		},
		{
			file: "states-github-new.html", path: "/apps/new",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "New"}},
			page: NewApp(NewAppData{
				Source: app.SourceGit, GitHubOn: true,
				Blueprint: app.Blueprint{Source: app.SourceGit, Label: "Repository",
					Description: "Build and run a Git repository"},
				Form: NewAppForm{Replicas: "1", Port: "8080"},
			}),
		},
	}
}
