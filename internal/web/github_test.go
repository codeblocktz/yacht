package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/github"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// fakeGitHubSurface records what the dashboard asked the service, so a test
// can say what reached it from a request. Whether a connect is allowed is the
// service's, and tested there; what is tested here is that the dashboard
// hands it the session's team and person, and GitHub's query, and nothing a
// request can choose instead.
type fakeGitHubSurface struct {
	mu sync.Mutex

	connects  []fakeConnect
	choices   []string
	result    app.GitHubConnect
	err       error
	gone      []int64
	conns     []app.GitHubConnection
	linkOwner string
	linkUser  string

	deliveries []app.GitHubDelivery
	deliverErr error

	deploys   []fakeDeploy
	deployErr error
}

type fakeDeploy struct {
	owner   string
	ref     app.GitHubRef
	project uuid.UUID
}

func (f *fakeGitHubSurface) DeployFromGitHub(
	_ context.Context, owner string, ref app.GitHubRef, project uuid.UUID,
) (app.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deployErr != nil {
		return app.App{}, f.deployErr
	}
	f.deploys = append(f.deploys, fakeDeploy{owner, ref, project})
	return app.App{Name: "web"}, nil
}

type fakeConnect struct {
	owner, user, email, state, code string
	installation                    int64
}

func (f *fakeGitHubSurface) GitHubInstallURL(owner, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkOwner, f.linkUser = owner, user
	return "https://github.com/apps/example/installations/new?state=signed", nil
}

func (f *fakeGitHubSurface) GitHubAuthorizeURL(owner, user string) (string, error) {
	return "https://github.com/login/oauth/authorize?state=signed", nil
}

func (f *fakeGitHubSurface) GitHubConnections(context.Context, string) ([]app.GitHubConnection, error) {
	return f.conns, nil
}

func (f *fakeGitHubSurface) CompleteGitHubConnect(
	_ context.Context, owner, user, email, state, code string, installation int64,
) (app.GitHubConnect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, fakeConnect{owner, user, email, state, code, installation})
	return f.result, f.err
}

func (f *fakeGitHubSurface) ChooseGitHubInstallation(
	_ context.Context, owner, user, email, choice string,
) (app.GitHubConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.choices = append(f.choices, owner+"|"+user+"|"+choice)
	return app.GitHubConnection{Account: "acme"}, nil
}

func (f *fakeGitHubSurface) DisconnectGitHub(_ context.Context, _ string, id int64) error {
	f.gone = append(f.gone, id)
	return nil
}

func (f *fakeGitHubSurface) GitHubRepos(context.Context, string) ([]app.GitHubRepo, []string, error) {
	return []app.GitHubRepo{
		{Ref: app.GitHubRef{InstallationID: 7, RepoID: 501}, FullName: "acme/web", Private: true},
		{Ref: app.GitHubRef{InstallationID: 7, RepoID: 502}, FullName: "acme/api"},
	}, nil, nil
}

func (f *fakeGitHubSurface) GitHubBranches(_ context.Context, _ string, ref app.GitHubRef, _ string) ([]string, error) {
	if ref.RepoID != 501 {
		return nil, app.ErrRepoUnreachable
	}
	return []string{"main", "release"}, nil
}

func (f *fakeGitHubSurface) GitHubDirectories(context.Context, string, app.GitHubRef, string) ([]string, error) {
	return []string{"server"}, nil
}

func (f *fakeGitHubSurface) DeliverGitHub(_ context.Context, d app.GitHubDelivery) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, d)
	return "Deploying 0123456.", f.deliverErr
}

func githubTeam(t *testing.T, teamID string) (*roleTeam, *fakeGitHubSurface) {
	t.Helper()
	gh := &fakeGitHubSurface{}
	rt := newRoleTeamWith(t, teamID, teamID+"-app", nil, func(o *Options) { o.GitHub = gh })
	return rt, gh
}

// GitHub comes back to the callback with what it issued. The service is
// handed GitHub's query as it came, with the team and person from the
// session — never from the query.
func TestTheCallbackHandsGitHubsAnswerToTheServiceWithTheSession(t *testing.T) {
	rt, gh := githubTeam(t, "web-gh-callback")
	gh.result = app.GitHubConnect{Connected: app.GitHubConnection{Account: "acme"}}

	q := url.Values{
		"code": {"the-code"}, "state": {"the-state"}, "installation_id": {"7"},
		"setup_action": {"install"}, "team": {"web-someone-else"},
	}
	rec := rt.getAs(t, GitHubCallbackPath+"?"+q.Encode(), rt.admin)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/team" {
		t.Fatalf("callback = %d to %q, want 303 to /team", rec.Code, rec.Header().Get("Location"))
	}
	if len(gh.connects) != 1 {
		t.Fatalf("connects = %+v, want one", gh.connects)
	}
	c := gh.connects[0]
	if c.owner != rt.teamID || c.user != rt.adminID.String() || c.email != "admin@web.test" ||
		c.state != "the-state" || c.code != "the-code" || c.installation != 7 {
		t.Fatalf("connect = %+v, want the session's team and person with GitHub's query", c)
	}

	// An installation id that is not a number reaches nothing.
	rt.getAs(t, GitHubCallbackPath+"?code=x&state=y&installation_id=seven", rt.admin)
	// Nor a return from changing an installation on GitHub's own pages,
	// which carries no state because nothing here started it.
	if rec := rt.getAs(t, GitHubCallbackPath+"?code=x&installation_id=7&setup_action=update", rt.admin); rec.Code != http.StatusSeeOther {
		t.Fatalf("a stateless return = %d, want 303", rec.Code)
	}
	// Nor does a return GitHub marked as refused.
	rt.getAs(t, GitHubCallbackPath+"?error=access_denied&state=y", rt.admin)
	if len(gh.connects) != 1 {
		t.Fatalf("connects = %+v, want the malformed returns stopped here", gh.connects)
	}
}

// Connecting is an admin's: a member following somebody's link connects
// nothing, and is not sent to GitHub either.
func TestOnlyAnAdminConnectsGitHub(t *testing.T) {
	rt, gh := githubTeam(t, "web-gh-roles")

	for _, path := range []string{
		GitHubCallbackPath + "?code=x&state=y&installation_id=7",
		"/github/install", "/github/authorize",
	} {
		if code := rt.getAs(t, path, rt.member).Code; code != http.StatusForbidden {
			t.Errorf("GET %s as a member = %d, want 403", path, code)
		}
	}
	if code := rt.postFormAs(t, "/team/github/7/disconnect", rt.member, url.Values{}).Code; code != http.StatusForbidden {
		t.Errorf("disconnect as a member = %d, want 403", code)
	}
	if len(gh.connects) != 0 || len(gh.gone) != 0 {
		t.Fatalf("a member reached the service: %+v %v", gh.connects, gh.gone)
	}

	rec := rt.getAs(t, "/github/install", rt.admin)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "https://github.com/apps/example/") {
		t.Fatalf("install = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if gh.linkOwner != rt.teamID || gh.linkUser != rt.adminID.String() {
		t.Fatalf("the link was made for %q/%q, want the session's", gh.linkOwner, gh.linkUser)
	}
	if code := rt.postFormAs(t, "/team/github/7/disconnect", rt.admin, url.Values{}).Code; code != http.StatusSeeOther || len(gh.gone) != 1 {
		t.Fatalf("disconnect as an admin = %d, %v", code, gh.gone)
	}

	// Members see the connections; only admins are offered the controls.
	gh.conns = []app.GitHubConnection{{InstallationID: 7, Account: "acme", ConnectedAt: time.Now()}}
	body := rt.getAs(t, "/team", rt.member).Body.String()
	if !strings.Contains(body, "acme") || strings.Contains(body, "/github/install") ||
		strings.Contains(body, "/disconnect") {
		t.Errorf("a member's team page: want the connection and no controls")
	}
	if body := rt.getAs(t, "/team", rt.admin).Body.String(); !strings.Contains(body, `href="/github/install"`) ||
		!strings.Contains(body, `/team/github/7/disconnect`) {
		t.Errorf("an admin's team page offers no controls")
	}
}

// Several installations: a page to choose from, with each proof as the value
// its form posts, and the choice handed back with the session.
func TestSeveralInstallationsAreOfferedAndChosen(t *testing.T) {
	rt, gh := githubTeam(t, "web-gh-choose")
	gh.result = app.GitHubConnect{Choices: []app.GitHubChoice{
		{InstallationID: 7, Account: "acme", AccountType: "Organization", Token: "proof-7"},
		{InstallationID: 9, Account: "alice", AccountType: "User", Token: "proof-9"},
	}}
	rec := rt.getAs(t, GitHubCallbackPath+"?code=c&state=s", rt.admin)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `value="proof-7"`) ||
		!strings.Contains(body, `value="proof-9"`) || strings.Contains(body, "c&amp;state") {
		t.Fatalf("choose page = %d:\n%s", rec.Code, body)
	}
	if code := rt.postFormAs(t, "/team/github/choose", rt.admin, url.Values{"choice": {"proof-9"}}).Code; code != http.StatusSeeOther {
		t.Fatalf("choose = %d", code)
	}
	if len(gh.choices) != 1 || gh.choices[0] != rt.teamID+"|"+rt.adminID.String()+"|proof-9" {
		t.Fatalf("choices = %v", gh.choices)
	}
}

// The pickers read through the App, and a refused repository is a sentence
// under the field rather than an error page.
func TestThePickersListWhatTheTeamReaches(t *testing.T) {
	rt, _ := githubTeam(t, "web-gh-pickers")

	body := rt.getAs(t, "/github/repositories?github_repo=7:501&label=acme/web", rt.member).Body.String()
	if !strings.Contains(body, `data-tui-selectbox-value="7:501" data-tui-selectbox-selected="true"`) ||
		!strings.Contains(body, "acme/api") || !strings.Contains(body, `name="github_repo"`) {
		t.Fatalf("repositories:\n%s", body)
	}
	gone := rt.getAs(t, "/github/repositories?github_repo=7:999&label=acme/old", rt.member).Body.String()
	if !strings.Contains(gone, "acme/old — no longer reachable") {
		t.Fatalf("a chosen repository no longer listed is not kept and said:\n%s", gone)
	}
	if b := rt.getAs(t, "/github/branches?github_repo=7:501", rt.member).Body.String(); !strings.Contains(b, `data-branch="release"`) {
		t.Fatalf("branches:\n%s", b)
	}
	if b := rt.getAs(t, "/github/branches?github_repo=7:502", rt.member).Body.String(); !strings.Contains(b, "Could not read") {
		t.Fatalf("an unreadable repository's branches:\n%s", b)
	}
}

// The App's routes exist only where an App is configured, and need accounts.
func TestNoGitHubAppMountsNothing(t *testing.T) {
	h := testServer(t, Options{})
	for _, path := range []string{GitHubCallbackPath, "/github/repositories", "/github/install"} {
		if code := get(t, h, path).Code; code != http.StatusNotFound {
			t.Errorf("GET %s with no App = %d, want 404", path, code)
		}
	}
	_, err := New(Options{
		Orchestrator: orchestrator.NewNoop(), GitHub: &fakeGitHubSurface{},
		Identity: identity.NewSingleOwner(identity.Owner{ID: "o"}),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		t.Fatal("a GitHub App was accepted without accounts")
	}
}

func TestGitHubRefusalsReadAsSentences(t *testing.T) {
	for err, want := range map[error]string{
		github.ErrStateInvalid:  "expired",
		app.ErrGitHubNotYours:   "GitHub does not list that installation",
		github.ErrNotAuthorized: "did not accept",
		fmt.Errorf("%w: %w", github.ErrUnreachable, errors.New("lookup github.com: i/o timeout")): "Try again",
	} {
		if got := githubSentence(err); !strings.Contains(got, want) || strings.HasPrefix(got, "app:") || strings.Contains(got, "lookup") {
			t.Errorf("githubSentence(%v) = %q", err, got)
		}
	}
}

// The App's webhook is a server calling a server: no Origin, no cookie. It
// reaches the service with the headers GitHub signs and names deliveries by,
// and nothing beside that one path is let past the cross-origin check.
func TestTheAppsWebhookReachesTheServiceWithoutAnOrigin(t *testing.T) {
	rt, gh := githubTeam(t, "web-gh-hook")

	req := httptest.NewRequest(http.MethodPost, GitHubHookPath, strings.NewReader(`{"ref":"refs/heads/main"}`))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", "sha256=abc")
	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "Deploying 0123456.") {
		t.Fatalf("delivery = %d %q, want 202 with what the service said", rec.Code, rec.Body.String())
	}
	if len(gh.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(gh.deliveries))
	}
	d := gh.deliveries[0]
	if d.ID != "d-1" || d.Event != "push" || d.Signature != "sha256=abc" || string(d.Body) != `{"ref":"refs/heads/main"}` {
		t.Fatalf("delivery = %+v, want GitHub's headers and body as they came", d)
	}

	gh.deliverErr = app.ErrHookUnauthorized
	rec = httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, GitHubHookPath, strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a refused signature = %d, want 401", rec.Code)
	}

	// The exemption is the path, not a prefix of it.
	rec = httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, GitHubHookPath+"x", strings.NewReader(`{}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST %sx without an Origin = %d, want 403", GitHubHookPath, rec.Code)
	}
}
