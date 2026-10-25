package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/github"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// fakeGitHub is GitHub's API as the App sees it, in memory: installations
// and their repositories, people and what they can reach, and the tokens it
// handed out.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// installations by id; a missing one is uninstalled.
	installations map[int64]*fakeInstallation
	// people by the code GitHub issued them: their login, and the
	// installations GitHub lists for them.
	people map[string]fakePerson
	// tokens handed out, to the installation and repository (0 for a list
	// token) they reach.
	tokens map[string][2]int64
	minted int
	// files by repository and path: "acme/web/Dockerfile".
	files map[string]string
}

type fakeInstallation struct {
	account   github.Account
	suspended bool
	repos     []github.Repository
}

type fakePerson struct {
	login         string
	installations []int64
}

const fakeHookSecret = "fake-hook-secret-0123456789"

func newFakeGitHub(t *testing.T) (*fakeGitHub, *github.App) {
	t.Helper()
	f := &fakeGitHub{
		t: t, installations: map[int64]*fakeInstallation{},
		people: map[string]fakePerson{}, tokens: map[string][2]int64{}, files: map[string]string{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh, err := github.New(github.Config{
		AppID: 1, Slug: "test-app", ClientID: "client", ClientSecret: "client-secret",
		PrivateKey: key, WebhookSecret: fakeHookSecret,
		APIURL: f.srv.URL, WebURL: f.srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, gh
}

func (f *fakeGitHub) install(id int64, login string, repos ...github.Repository) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installations[id] = &fakeInstallation{
		account: github.Account{Login: login, Type: "Organization"}, repos: repos,
	}
}

func (f *fakeGitHub) person(code, login string, installations ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people[code] = fakePerson{login: login, installations: installations}
}

func (f *fakeGitHub) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	auth := r.Header.Get("Authorization")
	switch {
	case path == "/login/oauth/access_token":
		_ = r.ParseForm()
		if _, ok := f.people[r.Form.Get("code")]; !ok {
			f.write(w, 200, map[string]string{"error": "bad_verification_code"})
			return
		}
		f.write(w, 200, map[string]string{"access_token": "ghu-" + r.Form.Get("code")})
	case path == "/user":
		p, ok := f.people[strings.TrimPrefix(auth, "Bearer ghu-")]
		if !ok {
			f.write(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		f.write(w, 200, map[string]string{"login": p.login})
	case path == "/user/installations":
		p, ok := f.people[strings.TrimPrefix(auth, "Bearer ghu-")]
		if !ok {
			f.write(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		var list []map[string]any
		for _, id := range p.installations {
			if in, ok := f.installations[id]; ok {
				list = append(list, map[string]any{"id": id, "app_id": 1, "account": in.account})
			}
		}
		f.write(w, 200, map[string]any{"total_count": len(list), "installations": list})
	case strings.HasPrefix(path, "/app/installations/"):
		if !strings.HasPrefix(auth, "Bearer ey") {
			f.t.Errorf("%s asked without the App's JWT", path)
		}
		rest := strings.TrimPrefix(path, "/app/installations/")
		idText, action, _ := strings.Cut(rest, "/")
		id, _ := strconv.ParseInt(idText, 10, 64)
		in, ok := f.installations[id]
		if !ok {
			f.write(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		if action == "" {
			f.write(w, 200, map[string]any{"id": id, "app_id": 1, "account": in.account})
			return
		}
		if in.suspended {
			f.write(w, 403, map[string]string{"message": "This installation has been suspended"})
			return
		}
		var body struct {
			RepositoryIDs []int64 `json:"repository_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.minted++
		tok := fmt.Sprintf("ghs_%d_%d", id, f.minted)
		var repos []github.Repository
		var repo int64
		for _, want := range body.RepositoryIDs {
			found := false
			for _, have := range in.repos {
				if have.ID == want {
					repos, repo, found = append(repos, have), want, true
				}
			}
			if !found {
				f.write(w, 422, map[string]string{"message": "There is at least one repository that does not exist or is not accessible to the parent installation."})
				return
			}
		}
		f.tokens[tok] = [2]int64{id, repo}
		f.write(w, 201, map[string]any{
			"token": tok, "expires_at": time.Now().Add(time.Hour), "repositories": repos,
		})
	case path == "/installation/repositories":
		reach, ok := f.tokens[strings.TrimPrefix(auth, "token ")]
		if !ok {
			f.write(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		f.write(w, 200, map[string]any{"repositories": f.installations[reach[0]].repos})
	case strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/branches"):
		reach, ok := f.tokens[strings.TrimPrefix(auth, "token ")]
		full := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/branches")
		if !ok || reach[1] == 0 {
			f.write(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		for _, repo := range f.installations[reach[0]].repos {
			if repo.ID == reach[1] && repo.FullName == full {
				f.write(w, 200, []map[string]string{{"name": "main"}, {"name": "dev"}, {"name": "feature/a"}})
				return
			}
		}
		f.write(w, 404, map[string]string{"message": "Not Found"})
	case strings.HasPrefix(path, "/repos/") && strings.Contains(path, "/contents/"):
		reach, ok := f.tokens[strings.TrimPrefix(auth, "token ")]
		full, file, _ := strings.Cut(strings.TrimPrefix(path, "/repos/"), "/contents/")
		content, found := f.files[full+"/"+file]
		if !ok || reach[1] == 0 || !found {
			f.write(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		f.write(w, 200, map[string]any{"type": "file", "encoding": "base64", "size": len(content),
			"content": base64.StdEncoding.EncodeToString([]byte(content))})
	default:
		http.NotFound(w, r)
	}
}

// buildRecorder is a builder that keeps what it was asked to build.
type buildRecorder struct {
	mu   sync.Mutex
	reqs []orchestrator.BuildRequest
}

func (b *buildRecorder) Build(_ context.Context, req orchestrator.BuildRequest) (orchestrator.BuildResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reqs = append(b.reqs, req)
	return orchestrator.BuildResult{}, errors.New("recorded, not built")
}
func (*buildRecorder) BuildJobName(orchestrator.BuildRequest) string { return "build-test" }
func (*buildRecorder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{}, nil
}
func (*buildRecorder) CancelBuild(context.Context, string) error { return nil }

func (b *buildRecorder) last(t *testing.T) orchestrator.BuildRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reqs) == 0 {
		t.Fatal("nothing was built")
	}
	return b.reqs[len(b.reqs)-1]
}

var (
	webRepo   = github.Repository{ID: 501, Name: "web", FullName: "acme/web", Private: true, DefaultBranch: "trunk"}
	otherRepo = github.Repository{ID: 502, Name: "api", FullName: "acme/api", Private: true, DefaultBranch: "main"}
)

// githubFixture is a service with a GitHub App, installation 7 on acme
// reaching web and api, and alice able to reach it.
func githubFixture(t *testing.T, builder Builder) (*Service, *fakeGitHub) {
	t.Helper()
	fake, gh := newFakeGitHub(t)
	fake.install(7, "acme", webRepo, otherRepo)
	fake.person("alice-code", "alice", 7)
	if builder == nil {
		builder = fakeBuilder{}
	}
	s, _, pool := testService(t, Options{Images: fakeImages{}, Builder: builder, GitHub: gh})
	// Installations and deliveries are the install's rather than a team's,
	// so they outlive the teams a test removes; each test starts without.
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM github_installations; DELETE FROM github_deliveries`); err != nil {
		t.Fatalf("reset GitHub state: %v", err)
	}
	return s, fake
}

// connect runs the return from GitHub for a team, as alice.
func connect(t *testing.T, s *Service, ownerID string, installationID int64) GitHubConnect {
	t.Helper()
	state := mustState(t, s, ownerID, "alice-id")
	got, err := s.CompleteGitHubConnect(context.Background(), ownerID, "alice-id",
		"alice@example.test", state, "alice-code", installationID)
	if err != nil {
		t.Fatalf("CompleteGitHubConnect: %v", err)
	}
	return got
}

func mustState(t *testing.T, s *Service, ownerID, user string) string {
	t.Helper()
	link, err := s.GitHubInstallURL(ownerID, user)
	if err != nil {
		t.Fatal(err)
	}
	_, state, _ := strings.Cut(link, "state=")
	return state
}

// An installation id in a query string proves nothing: one GitHub does not
// list among the person's own is refused, and nothing is connected.
func TestConnectingAnInstallationNeedsGitHubToListItAsTheirs(t *testing.T) {
	ctx := context.Background()
	s, fake := githubFixture(t, nil)
	fake.install(8, "someone-else", otherRepo)
	team := owner(t, s, s.pool, "gh-connect-proof")

	state := mustState(t, s, team, "alice-id")
	if _, err := s.CompleteGitHubConnect(ctx, team, "alice-id", "alice@example.test",
		state, "alice-code", 8); !errors.Is(err, ErrGitHubNotYours) {
		t.Fatalf("someone else's installation: err = %v, want ErrGitHubNotYours", err)
	}
	if _, err := s.CompleteGitHubConnect(ctx, team, "alice-id", "alice@example.test",
		state, "forged-code", 7); !errors.Is(err, github.ErrNotAuthorized) {
		t.Fatalf("a code GitHub never issued: err = %v, want ErrNotAuthorized", err)
	}
	if conns, _ := s.GitHubConnections(ctx, team); len(conns) != 0 {
		t.Fatalf("refused connects left %+v", conns)
	}

	got := connect(t, s, team, 7)
	c := got.Connected
	if c.InstallationID != 7 || c.Account != "acme" || c.ConnectedBy != "alice@example.test" ||
		c.ConnectedAs != "alice" || c.ConnectedAt.IsZero() {
		t.Fatalf("connected = %+v", c)
	}
}

// The state names the team and the person it was made for, and a session
// that is neither cannot use it.
func TestAGitHubLinkWorksOnlyForWhoItWasMadeFor(t *testing.T) {
	ctx := context.Background()
	s, _ := githubFixture(t, nil)
	team := owner(t, s, s.pool, "gh-state-a")
	other := owner(t, s, s.pool, "gh-state-b")

	state := mustState(t, s, team, "alice-id")
	for name, tc := range map[string][2]string{
		"another team":   {other, "alice-id"},
		"another person": {team, "mallory-id"},
	} {
		if _, err := s.CompleteGitHubConnect(ctx, tc[0], tc[1], "x@example.test",
			state, "alice-code", 7); !errors.Is(err, ErrGitHubStateMismatch) {
			t.Errorf("%s: err = %v, want ErrGitHubStateMismatch", name, err)
		}
	}
	if _, err := s.CompleteGitHubConnect(ctx, team, "alice-id", "x@example.test",
		"made.up", "alice-code", 7); !errors.Is(err, github.ErrStateInvalid) {
		t.Errorf("a state not made here: err = %v", err)
	}
}

// Authorizing without installing: GitHub's one installation is connected,
// several are offered, and an offer is good only for whom it was made.
func TestAuthorizingAloneConnectsOrOffersWhatGitHubLists(t *testing.T) {
	ctx := context.Background()
	s, fake := githubFixture(t, nil)
	team := owner(t, s, s.pool, "gh-authorize")
	other := owner(t, s, s.pool, "gh-authorize-other")

	fake.person("nobody-code", "nobody")
	if _, err := s.CompleteGitHubConnect(ctx, team, "u", "u@example.test",
		mustState(t, s, team, "u"), "nobody-code", 0); !errors.Is(err, ErrGitHubNoInstallation) {
		t.Fatalf("nothing installed: err = %v", err)
	}

	if got := connect(t, s, team, 0); got.Connected.InstallationID != 7 {
		t.Fatalf("the one installation was not connected: %+v", got)
	}

	fake.install(9, "alice", webRepo)
	fake.person("alice-code", "alice", 7, 9)
	got := connect(t, s, other, 0)
	if len(got.Choices) != 2 || got.Connected.InstallationID != 0 {
		t.Fatalf("several installations: %+v, want a choice", got)
	}
	var pick GitHubChoice
	for _, c := range got.Choices {
		if c.InstallationID == 9 {
			pick = c
		}
	}
	if _, err := s.ChooseGitHubInstallation(ctx, team, "alice-id", "a@example.test", pick.Token); !errors.Is(err, ErrGitHubStateMismatch) {
		t.Fatalf("another team used the choice: err = %v", err)
	}
	c, err := s.ChooseGitHubInstallation(ctx, other, "alice-id", "a@example.test", pick.Token)
	if err != nil || c.InstallationID != 9 || c.Account != "alice" || c.ConnectedAs != "alice" {
		t.Fatalf("chosen = %+v, %v", c, err)
	}
}

// A repository chosen from an installation is stored at GitHub's address for
// it, on GitHub's default branch — and only a team connected to the
// installation may choose from it, whoever else has.
func TestAnAppBuildsFromAChosenRepositoryOnlyThroughItsTeamsInstallation(t *testing.T) {
	ctx := context.Background()
	s, fake := githubFixture(t, nil)
	team := owner(t, s, s.pool, "gh-create")
	stranger := owner(t, s, s.pool, "gh-create-stranger")
	connect(t, s, team, 7)

	ref := GitHubRef{InstallationID: 7, RepoID: webRepo.ID}
	in := CreateInput{Name: "web", Source: SourceGit, Replicas: 1, Port: 8080, Repo: Repo{GitHub: ref}}
	if _, err := s.Create(ctx, stranger, in); !errors.Is(err, ErrGitHubNotConnected) {
		t.Fatalf("a team not connected: err = %v, want ErrGitHubNotConnected", err)
	}

	a, err := s.Create(ctx, team, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Repo.URL != "https://github.com/acme/web.git" || a.Repo.Branch != "trunk" || a.Repo.GitHub != ref {
		t.Fatalf("repo = %+v, want GitHub's address, its default branch and the ref", a.Repo)
	}

	// A repository the installation does not reach is GitHub's to refuse.
	fake.install(7, "acme", webRepo)
	in.Name, in.Repo.GitHub.RepoID = "api", otherRepo.ID
	if _, err := s.Create(ctx, team, in); err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("a repository not in the installation: err = %v", err)
	}

	// Pasting a URL in its place clears the choice.
	if err := s.CancelLiveDeployment(ctx, team, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, team, "web", UpdateInput{
		Port: 8080, Repo: Repo{URL: "https://github.com/acme/public.git", Branch: "main"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := s.Get(ctx, team, "web"); got.Repo.GitHub.Set() || got.Repo.URL != "https://github.com/acme/public.git" {
		t.Fatalf("after pasting a URL: %+v", got.Repo)
	}
	// And choosing again, with the URL field saying something else, stores
	// GitHub's address.
	if _, err := s.Update(ctx, team, "web", UpdateInput{
		Port: 8080, Repo: Repo{URL: "https://evil.example/x.git", GitHub: ref},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := s.Get(ctx, team, "web"); got.Repo.GitHub != ref || got.Repo.URL != "https://github.com/acme/web.git" {
		t.Fatalf("after choosing again: %+v", got.Repo)
	}
}

// Disconnecting pauses the team's apps from that installation; connecting
// again resumes them, and nothing another reason paused.
func TestDisconnectingPausesAndReconnectingResumes(t *testing.T) {
	ctx := context.Background()
	s, _ := githubFixture(t, nil)
	team := owner(t, s, s.pool, "gh-disconnect")
	connect(t, s, team, 7)
	if _, err := s.Create(ctx, team, CreateInput{Name: "web", Source: SourceGit, Replicas: 1, Port: 8080,
		Repo: Repo{GitHub: GitHubRef{InstallationID: 7, RepoID: webRepo.ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DisconnectGitHub(ctx, team, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.DisconnectGitHub(ctx, team, 7); !errors.Is(err, ErrGitHubNotConnected) {
		t.Fatalf("disconnecting twice: %v", err)
	}
	if a, _ := s.Get(ctx, team, "web"); a.GitHubPaused != GitHubPausedDisconnected {
		t.Fatalf("paused = %q", a.GitHubPaused)
	}
	if _, err := s.GitHubBranches(ctx, team, GitHubRef{InstallationID: 7, RepoID: webRepo.ID}, ""); !errors.Is(err, ErrGitHubNotConnected) {
		t.Fatalf("branches of a disconnected installation: %v", err)
	}
	connect(t, s, team, 7)
	if a, _ := s.Get(ctx, team, "web"); a.GitHubPaused != "" {
		t.Fatalf("after reconnecting, paused = %q", a.GitHubPaused)
	}
}

func TestPickersListTheTeamsRepositoriesAndBranches(t *testing.T) {
	ctx := context.Background()
	s, _ := githubFixture(t, nil)
	team := owner(t, s, s.pool, "gh-picker")
	if repos, _, _ := s.GitHubRepos(ctx, team); len(repos) != 0 {
		t.Fatalf("a team with nothing connected lists %v", repos)
	}
	connect(t, s, team, 7)
	repos, problems, err := s.GitHubRepos(ctx, team)
	if err != nil || len(problems) != 0 || len(repos) != 2 || repos[0].FullName != "acme/api" ||
		repos[1].Ref != (GitHubRef{InstallationID: 7, RepoID: webRepo.ID}) {
		t.Fatalf("repos = %+v, %v, %v", repos, problems, err)
	}
	branches, err := s.GitHubBranches(ctx, team, repos[1].Ref, "")
	if err != nil || strings.Join(branches, ",") != "main,dev,feature/a" {
		t.Fatalf("branches = %v, %v", branches, err)
	}
	if got, _ := s.GitHubBranches(ctx, team, repos[1].Ref, "feat"); strings.Join(got, ",") != "feature/a" {
		t.Fatalf("filtered branches = %v", got)
	}
}

func TestGitHubRefRoundTrips(t *testing.T) {
	ref := GitHubRef{InstallationID: 7, RepoID: 501}
	if got, err := ParseGitHubRef(ref.String()); err != nil || got != ref {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if got, err := ParseGitHubRef(""); err != nil || got.Set() {
		t.Fatalf("empty = %+v, %v", got, err)
	}
	for _, bad := range []string{"7", "7:", ":1", "a:b", "-1:2", "7:501:1"} {
		if _, err := ParseGitHubRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
