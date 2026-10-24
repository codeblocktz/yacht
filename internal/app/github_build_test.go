package app

import (
	"context"
	"strings"
	"testing"
)

// The clone's token is minted for the build, for the one repository, and
// travels beside the URL rather than in it.
func TestABuildClonesWithATokenForItsRepositoryAlone(t *testing.T) {
	ctx := context.Background()
	b := &buildRecorder{}
	s, fake := githubFixture(t, b)
	team := owner(t, s, s.pool, "gh-build")
	connect(t, s, team, 7)

	a, err := s.Create(ctx, team, CreateInput{Name: "web", Source: SourceGit, Replicas: 1, Port: 8080,
		Repo: Repo{GitHub: GitHubRef{InstallationID: 7, RepoID: webRepo.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = executeLiveOperation(s, ctx, a)
	req := b.last(t)
	if req.RepoToken == "" || strings.Contains(req.RepoURL, req.RepoToken) || req.RepoURL != "https://github.com/acme/web.git" {
		t.Fatalf("request URL %q, token set %t — want the token beside a clean URL", req.RepoURL, req.RepoToken != "")
	}
	fake.mu.Lock()
	reach := fake.tokens[req.RepoToken]
	fake.mu.Unlock()
	if reach != [2]int64{7, webRepo.ID} {
		t.Fatalf("the token reaches %v, want installation 7's web repository alone", reach)
	}
	var stored string
	if err := s.pool.QueryRow(ctx,
		`SELECT string_agg(repo_url || message || log, '') FROM builds WHERE app_id = $1`,
		a.ID).Scan(&stored); err != nil {
		t.Fatalf("read builds: %v", err)
	}
	if stored == "" || strings.Contains(stored, req.RepoToken) {
		t.Fatalf("stored build %q — want one, without the token", stored)
	}
}
