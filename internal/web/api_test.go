package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/apitoken"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/audit"
	"github.com/codeblocktz/yacht/internal/cli"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store"
)

// apiImages is a registry that is always there, so a Git app can exist and
// build.
type apiImages struct{}

func (apiImages) ImageFor(_ context.Context, ownerID, a, revision string) (string, error) {
	return "registry.test/" + ownerID + "/" + a + ":" + revision, nil
}
func (apiImages) Configured(context.Context) bool              { return true }
func (apiImages) Insecure(context.Context) bool                { return false }
func (apiImages) DockerConfig(context.Context) ([]byte, error) { return []byte("{}"), nil }

// fetchingBuilder is a builder that does what the build pod's first step
// does for an upload — fetches the archive from the engine with the token it
// was handed, over HTTP — and records what it got. That is the whole path a
// real build takes to its source, short of a cluster.
type fetchingBuilder struct {
	mu       sync.Mutex
	archives [][]byte
	reqs     []orchestrator.BuildRequest
	fetchErr error
}

func (b *fetchingBuilder) Build(ctx context.Context, req orchestrator.BuildRequest) (orchestrator.BuildResult, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	b.mu.Unlock()
	if req.SourceURL == "" {
		return orchestrator.BuildResult{CommitSHA: strings.Repeat("c", 40)}, nil
	}
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, req.SourceURL, nil)
	hreq.Header.Set("Authorization", "Bearer "+req.SourceToken)
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return orchestrator.BuildResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("fetch answered %d", resp.StatusCode)
		b.mu.Lock()
		b.fetchErr = err
		b.mu.Unlock()
		return orchestrator.BuildResult{}, err
	}
	b.mu.Lock()
	b.archives = append(b.archives, body)
	b.mu.Unlock()
	req.Log("Fetched the uploaded source\nStep 1/1 : FROM scratch\n")
	return orchestrator.BuildResult{}, nil
}
func (b *fetchingBuilder) BuildJobName(req orchestrator.BuildRequest) string {
	return "build-" + req.Image[strings.LastIndex(req.Image, ":")+1:]
}
func (b *fetchingBuilder) BuildState(context.Context, string) (orchestrator.BuildState, error) {
	return orchestrator.BuildState{}, nil
}
func (b *fetchingBuilder) CancelBuild(context.Context, string) error { return nil }

type apiHarness struct {
	t        *testing.T
	pool     *pgxpool.Pool
	accounts *account.Service
	apps     *app.Service
	tokens   *apitoken.Service
	audit    *audit.Log
	builder  *fetchingBuilder
	base     string
}

// newAPIHarness is the engine assembled over a real database and served on
// a real port: the API, the dashboard, and the fetch a build makes back to
// the engine. Teams begin web-api- and addresses end @api.web.test.
func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run the API tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	purge := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM teams WHERE id LIKE 'web-api-%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE lower(email) LIKE '%@api.web.test'`)
	}
	purge()
	t.Cleanup(purge)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + l.Addr().String()
	h := &apiHarness{t: t, pool: pool, base: base, builder: &fetchingBuilder{}}
	h.accounts = account.NewService(pool, log)
	h.tokens = apitoken.New(pool, log, "")
	h.audit = audit.New(pool, log)
	h.apps = app.NewService(pool, orchestrator.NewNoop(), log, app.Options{
		Manifests: testManifests{}, Images: apiImages{}, Builder: h.builder,
		SourceFetchURL: base + APIPrefix + "/build-sources",
		RolloutPollInterval: 10 * time.Millisecond,
	})
	handler := testServer(t, Options{
		Apps: h.apps, Logs: h.apps, Accounts: h.accounts,
		Identity: h.accounts.Provider(SessionCookie), Mailer: &fakeMailer{},
		BaseURL: base, BootstrapTeamID: "web-api-a", SessionTTL: time.Hour,
		Tokens: h.tokens, Audit: h.audit,
	})
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: handler}}
	srv.Start()
	t.Cleanup(srv.Close)
	return h
}

// team makes a team with an owner, an admin and a member, and returns their
// ids in that order.
func (h *apiHarness) team(id string) (owner, admin, member uuid.UUID) {
	h.t.Helper()
	ctx := context.Background()
	user := func(role string) uuid.UUID {
		u, err := h.accounts.EnsureUser(ctx, role+"-"+id+"@api.web.test", "")
		if err != nil {
			h.t.Fatalf("EnsureUser: %v", err)
		}
		return u.ID
	}
	owner, admin, member = user("owner"), user("admin"), user("member")
	if _, err := h.accounts.CreateTeam(ctx, id, id, owner); err != nil {
		h.t.Fatalf("CreateTeam: %v", err)
	}
	if err := h.apps.EnsureOwner(ctx, id, id, ""); err != nil {
		h.t.Fatalf("EnsureOwner: %v", err)
	}
	if err := h.accounts.SetRole(ctx, owner, id, admin, account.RoleAdmin); err != nil {
		h.t.Fatalf("SetRole: %v", err)
	}
	if err := h.accounts.SetRole(ctx, owner, id, member, account.RoleMember); err != nil {
		h.t.Fatalf("SetRole: %v", err)
	}
	return owner, admin, member
}

func (h *apiHarness) token(team string, actor uuid.UUID, role account.Role, project *uuid.UUID) string {
	h.t.Helper()
	raw, _, err := h.tokens.Create(context.Background(), apitoken.CreateInput{
		OwnerID: team, ProjectID: project, Name: "t-" + string(role), Role: role, Actor: actor,
	})
	if err != nil {
		h.t.Fatalf("token: %v", err)
	}
	return raw
}

func (h *apiHarness) session(user uuid.UUID, team string) *http.Cookie {
	h.t.Helper()
	raw, err := h.accounts.CreateSession(context.Background(), user, team, "test", "127.0.0.1", time.Hour)
	if err != nil {
		h.t.Fatalf("session: %v", err)
	}
	return &http.Cookie{Name: SessionCookie, Value: raw}
}

// settle marks an app's admitted deploy done, as a worker would have.
func (h *apiHarness) settle(team, name string) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), `
		UPDATE deployment_operations o SET status = 'succeeded', finished_at = now(),
		    claim_token = NULL, lease_expires_at = NULL
		FROM apps a WHERE o.app_id = a.id AND a.owner_id = $1 AND a.name = $2
		  AND o.status IN ('queued', 'claimed', 'building', 'applying', 'verifying')`, team, name); err != nil {
		h.t.Fatalf("settle: %v", err)
	}
	if _, err := h.pool.Exec(context.Background(), `
		UPDATE deployments d SET status = 'succeeded', finished_at = now()
		FROM apps a WHERE d.app_id = a.id AND a.owner_id = $1 AND a.name = $2 AND d.status = 'running'`, team, name); err != nil {
		h.t.Fatalf("settle deployments: %v", err)
	}
}

func (h *apiHarness) gitApp(team, name string, project uuid.UUID) app.App {
	h.t.Helper()
	a, err := h.apps.Create(context.Background(), team, app.CreateInput{
		Source: app.SourceGit, Name: name, Port: 8080, Replicas: 1, ProjectID: project,
		Repo: app.Repo{URL: "https://github.com/example/" + name + ".git"},
	})
	if err != nil {
		h.t.Fatalf("Create %s: %v", name, err)
	}
	h.settle(team, name)
	return a
}

// imageApp is an app that runs an image, which — unlike a Git app that has
// not built yet — can have its variables set and deployed at once.
func (h *apiHarness) imageApp(team, name string, project uuid.UUID) app.App {
	h.t.Helper()
	a, err := h.apps.Create(context.Background(), team, app.CreateInput{
		Name: name, Image: "nginx:alpine", Port: 8080, Replicas: 1, ProjectID: project,
	})
	if err != nil {
		h.t.Fatalf("Create %s: %v", name, err)
	}
	h.settle(team, name)
	return a
}

func (h *apiHarness) project(team, name string) app.Project {
	h.t.Helper()
	p, err := h.apps.CreateProject(context.Background(), team, name)
	if err != nil {
		h.t.Fatalf("CreateProject: %v", err)
	}
	return p
}

// call makes one API request with a bearer token, or with none.
func (h *apiHarness) call(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.base+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// form posts to the dashboard as a signed-in browser.
func (h *apiHarness) form(path string, c *http.Cookie, values url.Values) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.base+path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.base)
	req.AddCookie(c)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *apiHarness) page(path string, c *http.Cookie) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.base+path, nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *apiHarness) auditActions(team string) []audit.Entry {
	h.t.Helper()
	es, err := h.audit.List(context.Background(), team, audit.Filter{Limit: 200})
	if err != nil {
		h.t.Fatalf("audit: %v", err)
	}
	return es
}

func TestTheAPIRefusesAnythingButAValidToken(t *testing.T) {
	h := newAPIHarness(t)
	_, admin, member := h.team("web-api-a")
	cookie := h.session(admin, "web-api-a")
	revoked := h.token("web-api-a", member, account.RoleMember, nil)
	toks, _ := h.tokens.List(context.Background(), "web-api-a")
	if _, err := h.tokens.Revoke(context.Background(), "web-api-a", member, toks[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	for name, tok := range map[string]string{"none": "", "garbage": "yacht_nope", "revoked": revoked} {
		code, body := h.call(http.MethodGet, "/api/v1/apps", tok, nil)
		if code != http.StatusUnauthorized || body["error"] == nil {
			t.Errorf("%s: GET /api/v1/apps = %d %v, want a JSON 401", name, code, body)
		}
	}
	// A session cookie is not an API credential.
	req, _ := http.NewRequest(http.MethodGet, h.base+"/api/v1/apps", nil)
	req.AddCookie(cookie)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the API answered a session cookie with %d, want 401", resp.StatusCode)
	}
}

// The scoping that matters most: every app route, walked off the router so
// one added later is covered, answered for team B's app with team A's
// strongest token. None may succeed, and team B's app must be unchanged.
func TestATokenFromOneTeamNeverTouchesAnother(t *testing.T) {
	h := newAPIHarness(t)
	_, adminA, _ := h.team("web-api-a")
	_, adminB, _ := h.team("web-api-b")
	h.imageApp("web-api-a", "shared-name", uuid.Nil)
	b := h.imageApp("web-api-b", "victim", uuid.Nil)
	if err := h.apps.SetVariable(context.Background(), "web-api-b", "victim",
		app.VariableInput{Key: "DB_PASSWORD", Value: "b-only", Secret: false}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	h.settle("web-api-b", "victim")
	depsBefore, _ := h.apps.Deployments(context.Background(), "web-api-b", b.ID, 100)
	tokA := h.token("web-api-a", adminA, account.RoleAdmin, nil)
	_ = adminB

	srv, err := New(Options{
		Orchestrator: orchestrator.NewNoop(), Apps: h.apps, Logs: h.apps, Accounts: h.accounts,
		Identity: h.accounts.Provider(SessionCookie), BaseURL: h.base, BootstrapTeamID: "web-api-a",
		Tokens: h.tokens, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	_ = chi.Walk(srv.Handler().(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/api/v1/apps/{name}") {
			routes = append(routes, method+" "+route)
		}
		return nil
	})
	if len(routes) < 10 {
		t.Fatalf("walked %d app routes (%v); the walk is not seeing the API", len(routes), routes)
	}
	deps, _ := h.apps.Deployments(context.Background(), "web-api-b", b.ID, 1)
	for _, r := range routes {
		method, route, _ := strings.Cut(r, " ")
		path := strings.NewReplacer("{name}", "victim", "{key}", "DB_PASSWORD", "{id}", deps[0].ID.String()).Replace(route)
		var body any
		if method == http.MethodPut {
			body = map[string]any{"value": "pwned"}
		}
		if strings.HasSuffix(route, "/rollback") {
			body = map[string]any{"deployment_id": deps[0].ID.String()}
		}
		code, out := h.call(method, path, tokA, body)
		if code != http.StatusNotFound {
			t.Errorf("%s %s with another team's token = %d %v, want 404", method, path, code, out)
		}
	}
	if code, out := h.call(http.MethodGet, "/api/v1/apps", tokA, nil); code != http.StatusOK ||
		strings.Contains(fmt.Sprint(out), "victim") {
		t.Errorf("team A's app list = %d %v, must not include team B's", code, out)
	}
	after, _ := h.apps.Get(context.Background(), "web-api-b", "victim")
	for _, v := range after.Variables {
		if v.Key == "DB_PASSWORD" && v.Value != "b-only" {
			t.Fatal("team A's token changed team B's variable")
		}
	}
	depsAfter, _ := h.apps.Deployments(context.Background(), "web-api-b", b.ID, 100)
	if len(depsAfter) != len(depsBefore) {
		t.Fatalf("team A's token admitted a deploy of team B's app (%d → %d)", len(depsBefore), len(depsAfter))
	}
}

func TestAProjectTokenReachesOnlyItsProject(t *testing.T) {
	h := newAPIHarness(t)
	_, admin, _ := h.team("web-api-a")
	p1, p2 := h.project("web-api-a", "Shop"), h.project("web-api-a", "Billing")
	h.gitApp("web-api-a", "storefront", p1.ID)
	h.gitApp("web-api-a", "invoices", p2.ID)
	tok := h.token("web-api-a", admin, account.RoleAdmin, &p1.ID)

	code, out := h.call(http.MethodGet, "/api/v1/apps", tok, nil)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(out), "storefront") || strings.Contains(fmt.Sprint(out), "invoices") {
		t.Errorf("project token's apps = %d %v", code, out)
	}
	code, out = h.call(http.MethodGet, "/api/v1/projects", tok, nil)
	if code != http.StatusOK || strings.Contains(fmt.Sprint(out), "billing") {
		t.Errorf("project token's projects = %d %v", code, out)
	}
	for _, path := range []string{"/api/v1/apps/invoices", "/api/v1/apps/invoices/variables", "/api/v1/projects/" + p2.Slug + "/apps"} {
		if code, _ := h.call(http.MethodGet, path, tok, nil); code != http.StatusNotFound {
			t.Errorf("GET %s with another project's token = %d, want 404", path, code)
		}
	}
	if code, _ := h.call(http.MethodPost, "/api/v1/apps/invoices/redeploy", tok, nil); code != http.StatusNotFound {
		t.Errorf("redeploying another project's app = %d, want 404", code)
	}
	if code, _ := h.call(http.MethodGet, "/api/v1/apps/storefront", tok, nil); code != http.StatusOK {
		t.Errorf("GET its own project's app = %d", code)
	}
}

// The API's gates are the dashboard's: a member deploys and reads, an admin
// also changes variables and reads secrets, and both are written down.
func TestTheAPIsRolesAreTheDashboards(t *testing.T) {
	h := newAPIHarness(t)
	_, admin, member := h.team("web-api-a")
	h.imageApp("web-api-a", "api", uuid.Nil)
	memberTok := h.token("web-api-a", member, account.RoleMember, nil)
	adminTok := h.token("web-api-a", admin, account.RoleAdmin, nil)

	if code, _ := h.call(http.MethodPut, "/api/v1/apps/api/variables/STRIPE_KEY", memberTok, map[string]any{"value": "sk_live_x"}); code != http.StatusForbidden {
		t.Errorf("a member's token setting a variable = %d, want 403", code)
	}
	if code, _ := h.call(http.MethodDelete, "/api/v1/apps/api/variables/STRIPE_KEY", memberTok, nil); code != http.StatusForbidden {
		t.Errorf("a member's token deleting a variable = %d, want 403", code)
	}
	code, out := h.call(http.MethodPut, "/api/v1/apps/api/variables/STRIPE_KEY", adminTok, map[string]any{"value": "sk_live_x"})
	if code != http.StatusOK {
		t.Fatalf("an admin's token setting a variable = %d %v", code, out)
	}
	h.settle("web-api-a", "api")

	code, out = h.call(http.MethodGet, "/api/v1/apps/api/variables", memberTok, nil)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(out), "STRIPE_KEY") {
		t.Errorf("a member listing variables = %d %v", code, out)
	}
	if code, _ := h.call(http.MethodGet, "/api/v1/apps/api/variables?reveal=1", memberTok, nil); code != http.StatusForbidden {
		t.Errorf("a member revealing = %d, want 403", code)
	}
	code, out = h.call(http.MethodPost, "/api/v1/apps/api/redeploy", memberTok, nil)
	if code != http.StatusAccepted {
		t.Errorf("a member redeploying = %d %v, want 202", code, out)
	}
	dep, _ := out["deployment"].(map[string]any)
	if dep == nil || dep["actor"] != "token:t-member" {
		t.Errorf("the deploy is recorded as %v, want the token's", dep)
	}

	var set, deploy bool
	for _, e := range h.auditActions("web-api-a") {
		if e.Action == "variable.set" && e.Actor.Kind == audit.ActorToken && e.Detail["key"] == "STRIPE_KEY" {
			set = true
		}
		if e.Action == "deploy.redeploy" && e.Actor.Name == "t-member" {
			deploy = true
		}
		if strings.Contains(fmt.Sprint(e.Detail), "sk_live_x") {
			t.Fatalf("the audit log holds a variable's value: %+v", e)
		}
	}
	if !set || !deploy {
		t.Errorf("audit log missing the token's actions (set=%v deploy=%v): %+v", set, deploy, h.auditActions("web-api-a"))
	}
	for _, e := range h.auditActions("web-api-a") {
		if e.Action == "variable.set" && e.Actor.Name == "t-member" {
			t.Error("a refused change was recorded as done")
		}
	}
}

func TestSecretsAreRevealedToAdminsAndTheRevealIsRecorded(t *testing.T) {
	h := newAPIHarness(t)
	_, admin, _ := h.team("web-api-a")
	h.gitApp("web-api-a", "api", uuid.Nil)
	adminTok := h.token("web-api-a", admin, account.RoleAdmin, nil)
	code, out := h.call(http.MethodGet, "/api/v1/apps/api/variables?reveal=1", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("an admin revealing = %d %v", code, out)
	}
	var revealed bool
	for _, e := range h.auditActions("web-api-a") {
		revealed = revealed || e.Action == "secret.reveal"
	}
	if !revealed {
		t.Error("a reveal through the API was not recorded")
	}
}

// The API never reads a cookie, so CSRF cannot ride one — but only a request
// with a bearer token is let past the origin check, so an install answering
// the API without one cannot be driven from another site's form.
func TestTheOriginCheckStepsAsideOnlyForABearer(t *testing.T) {
	h := newAPIHarness(t)
	_, _, member := h.team("web-api-a")
	h.gitApp("web-api-a", "api", uuid.Nil)
	tok := h.token("web-api-a", member, account.RoleMember, nil)
	if code, out := h.call(http.MethodPost, "/api/v1/apps/api/redeploy", tok, nil); code == http.StatusForbidden {
		t.Fatalf("a bearer call with no Origin was refused: %v", out)
	}
	req, _ := http.NewRequest(http.MethodPost, h.base+"/api/v1/apps/api/redeploy", nil)
	req.Header.Set("Origin", "https://evil.test")
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "cross-origin") {
		t.Errorf("a cross-site post with no bearer = %d %s, want the origin check's 403", resp.StatusCode, b)
	}
}

func TestDashboardChangesAreAuditedWithKeysOnly(t *testing.T) {
	h := newAPIHarness(t)
	_, admin, member := h.team("web-api-a")
	h.imageApp("web-api-a", "api", uuid.Nil)
	c := h.session(admin, "web-api-a")
	if code, _ := h.form("/apps/api/variables", c, url.Values{"key": {"API_SECRET"}, "value": {"hunter2-value"}}); code >= 400 {
		t.Fatalf("set variable = %d", code)
	}
	h.settle("web-api-a", "api")
	// A failure said by flash is not recorded as done.
	if code, _ := h.form("/apps/nope/redeploy", c, nil); code >= 500 {
		t.Fatalf("redeploy of nothing = %d", code)
	}
	if code, _ := h.form("/team/members/"+member.String()+"/remove", c, nil); code >= 400 {
		t.Fatalf("remove member = %d", code)
	}

	var raw string
	_ = h.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(e)::text, ' '), '') FROM audit_events e WHERE owner_id = 'web-api-a'`).Scan(&raw)
	if strings.Contains(raw, "hunter2-value") {
		t.Fatal("the audit log holds a variable's value")
	}
	got := map[string]audit.Entry{}
	for _, e := range h.auditActions("web-api-a") {
		got[e.Action] = e
	}
	if e, ok := got["variable.set"]; !ok || e.Detail["key"] != "API_SECRET" || e.Target != "api" ||
		e.Actor.Name != "admin-web-api-a@api.web.test" || e.Actor.IP == "" {
		t.Errorf("variable.set entry = %+v", got["variable.set"])
	}
	if e, ok := got["member.remove"]; !ok || e.Target != "member-web-api-a@api.web.test" {
		t.Errorf("member.remove entry = %+v", got["member.remove"])
	}
	if _, ok := got["deploy.redeploy"]; ok {
		t.Error("a redeploy that failed was recorded as done")
	}
	// The page lists them, to an admin and not to a member.
	if code, body := h.page("/team/audit?category=variable", c); code != http.StatusOK || !strings.Contains(body, "variable.set") || strings.Contains(body, "member.remove") {
		t.Errorf("filtered audit page = %d", code)
	}
}

func TestATokenMadeOnTheTeamPageIsShownOnce(t *testing.T) {
	h := newAPIHarness(t)
	_, admin, _ := h.team("web-api-a")
	c := h.session(admin, "web-api-a")
	code, body := h.form("/team/tokens", c, url.Values{"name": {"GitHub Actions"}, "role": {"admin"}})
	if code != http.StatusOK {
		t.Fatalf("create token = %d", code)
	}
	m := regexp.MustCompile(`data-token>(yacht_[A-Za-z0-9_-]+)<`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the new token is not on the page that made it")
	}
	if code, _ := h.call(http.MethodGet, "/api/v1/me", m[1], nil); code != http.StatusOK {
		t.Errorf("the token shown does not work: %d", code)
	}
	if _, again := h.page("/team/tokens", c); strings.Contains(again, m[1]) {
		t.Fatal("the token is shown again after it was made")
	}
	var created bool
	for _, e := range h.auditActions("web-api-a") {
		created = created || (e.Action == "token.create" && e.Target == "GitHub Actions" && e.Detail["role"] == "admin")
	}
	if !created {
		t.Error("making a token was not recorded")
	}

	// Revoked, it is named in the log as it was made: an id means nothing
	// once the token is gone from the list.
	toks, err := h.tokens.List(context.Background(), "web-api-a")
	if err != nil || len(toks) == 0 {
		t.Fatalf("tokens = %v, %v", toks, err)
	}
	var id string
	for _, tok := range toks {
		if tok.Name == "GitHub Actions" {
			id = tok.ID.String()
		}
	}
	if code, _ := h.form("/team/tokens/"+id+"/revoke", c, url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("revoke = %d", code)
	}
	var revoked bool
	for _, e := range h.auditActions("web-api-a") {
		revoked = revoked || (e.Action == "token.revoke" && e.Target == "GitHub Actions" && e.Detail["token"] == id)
	}
	if !revoked {
		t.Errorf("the revoke is not recorded by the token's name: %+v", h.auditActions("web-api-a"))
	}
}

func TestBuildSourceIsOnlyForTheBuildsToken(t *testing.T) {
	h := newAPIHarness(t)
	for _, tok := range []string{"", "wrong"} {
		req, _ := http.NewRequest(http.MethodGet, h.base+"/api/v1/build-sources/"+uuid.NewString(), nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("fetch with %q = %d, want 404", tok, resp.StatusCode)
		}
	}
}

func TestTheRateLimiterBrakes(t *testing.T) {
	l := newAPIRateLimiter(60, 3)
	for i := range 3 {
		if !l.allow("k") {
			t.Fatalf("request %d refused inside the burst", i)
		}
	}
	if l.allow("k") {
		t.Fatal("a fourth request at once was let through a burst of three")
	}
	if !l.allow("other") {
		t.Fatal("one token's burst spent another's")
	}
}

// `up`, end to end: the CLI packs a directory honouring its ignore files,
// uploads it, the worker builds it with a builder that fetches the archive
// back from the engine with the token minted for it, and the deploy
// finishes — read back as "Deploy via CLI", made by the token.
func TestUpUploadsBuildsAndDeploys(t *testing.T) {
	h := newAPIHarness(t)
	_, _, member := h.team("web-api-a")
	p := h.project("web-api-a", "Site")
	a := h.gitApp("web-api-a", "site", p.ID)
	tok := h.token("web-api-a", member, account.RoleMember, nil)

	dir := t.TempDir()
	for path, body := range map[string]string{
		"Dockerfile":               "FROM scratch\n",
		"main.go":                  "package main\n",
		"web/index.html":           "<h1>hi</h1>\n",
		".gitignore":               "node_modules/\n*.log\n",
		"web/.gitignore":           "dist/\n",
		".dockerignore":            "secrets.env\n",
		"node_modules/left/out.js": "x",
		"debug.log":                "x",
		"web/dist/bundle.js":       "x",
		"secrets.env":              "TOKEN=x",
		".git/HEAD":                "ref: refs/heads/main\n",
	} {
		full := filepath.Join(dir, path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.apps.RunOperationAdmission(ctx)

	var out, errOut bytes.Buffer
	env := cli.Env{
		Stdout: &out, Stderr: &errOut, Dir: dir, ConfigDir: t.TempDir(),
		Getenv: func(k string) string {
			return map[string]string{"DATAHURU_TOKEN": tok, "DATAHURU_HOST": h.base}[k]
		},
		PollInterval: 50 * time.Millisecond,
	}
	brand := cli.Brand{Name: "datahuru", Product: "Datahuru"}
	if code := cli.Run(ctx, brand, []string{"link", "--project", p.Slug, "--app", "site"}, env); code != 0 {
		t.Fatalf("link exited %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".datahuru", "config.json")); err != nil {
		t.Fatalf("link wrote nothing: %v", err)
	}
	runCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if code := cli.Run(runCtx, brand, []string{"up"}, env); code != 0 {
		t.Fatalf("up exited %d\nstdout: %s\nstderr: %s\nfetch: %v", code, out.String(), errOut.String(), h.builder.fetchErr)
	}
	if !strings.Contains(out.String(), "Deployed site.") || !strings.Contains(out.String(), "Fetched the uploaded source") {
		t.Errorf("up said:\n%s", out.String())
	}

	h.builder.mu.Lock()
	archives := h.builder.archives
	req := h.builder.reqs[len(h.builder.reqs)-1]
	h.builder.mu.Unlock()
	if len(archives) != 1 {
		t.Fatalf("the builder fetched %d archives, want 1", len(archives))
	}
	if req.RepoURL != "" || req.SourceURL == "" || req.SourceToken == "" {
		t.Errorf("build request = repo %q source %q — an upload builds from the upload", req.RepoURL, req.SourceURL)
	}
	files := tarNames(t, archives[0])
	want := []string{"Dockerfile", ".dockerignore", ".gitignore", "main.go", "web/.gitignore", "web/index.html"}
	for _, w := range want {
		if !files[w] {
			t.Errorf("%s was left out of the upload; it has %v", w, keys(files))
		}
	}
	for _, gone := range []string{"node_modules/left/out.js", "debug.log", "web/dist/bundle.js", "secrets.env", ".git/HEAD", ".datahuru/config.json"} {
		if files[gone] {
			t.Errorf("%s was uploaded; the ignore files leave it out", gone)
		}
	}

	deps, _ := h.apps.Deployments(context.Background(), "web-api-a", a.ID, 1)
	if len(deps) == 0 || deploymentTitle(deps[0]) != "Deploy via CLI" || deps[0].ActorKind != "token" ||
		deps[0].Status != app.DeploySucceeded || deploymentSource(a, deps[0]) != "CLI" {
		t.Fatalf("latest deployment = %+v (%q)", deps, deploymentTitle(deps[0]))
	}
	// The archive is dropped once built, and its credential with it.
	var archive []byte
	var purged bool
	_ = h.pool.QueryRow(context.Background(), `SELECT archive, purged_at IS NOT NULL FROM source_uploads WHERE deployment_id = $1`, deps[0].ID).Scan(&archive, &purged)
	if archive != nil || !purged {
		t.Error("the uploaded archive was kept after its build")
	}
	hreq, _ := http.NewRequest(http.MethodGet, req.SourceURL, nil)
	hreq.Header.Set("Authorization", "Bearer "+req.SourceToken)
	resp, _ := http.DefaultClient.Do(hreq)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the build's credential still fetches after the build: %d", resp.StatusCode)
	}
	var uploaded bool
	for _, e := range h.auditActions("web-api-a") {
		uploaded = uploaded || (e.Action == "deploy.upload" && e.Actor.Kind == audit.ActorToken && e.Target == "site")
	}
	if !uploaded {
		t.Error("the upload was not recorded")
	}

	// status, logs --build and variables read the same app.
	out.Reset()
	if code := cli.Run(ctx, brand, []string{"status"}, env); code != 0 || !strings.Contains(out.String(), "Deploy via CLI") {
		t.Errorf("status exited %d: %s %s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := cli.Run(ctx, brand, []string{"logs", "--build"}, env); code != 0 || !strings.Contains(out.String(), "Fetched the uploaded source") {
		t.Errorf("logs --build exited %d: %s %s", code, out.String(), errOut.String())
	}
}

// An upload to an app that does not build from source is refused at once,
// with the reason, and nothing is stored.
func TestUpToAnImageAppIsRefused(t *testing.T) {
	h := newAPIHarness(t)
	_, _, member := h.team("web-api-a")
	if _, err := h.apps.Create(context.Background(), "web-api-a", app.CreateInput{Name: "nginx", Image: "nginx:alpine", Replicas: 1, Port: 80}); err != nil {
		t.Fatal(err)
	}
	tok := h.token("web-api-a", member, account.RoleMember, nil)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "a", Size: 1, Mode: 0o644, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("a"))
	_ = tw.Close()
	_ = zw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.base+"/api/v1/apps/nginx/up", &buf)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "runs an image") {
		t.Fatalf("up to an image app = %d %s", resp.StatusCode, body)
	}
	var n int
	_ = h.pool.QueryRow(context.Background(), `SELECT count(*) FROM source_uploads WHERE owner_id = 'web-api-a'`).Scan(&n)
	if n != 0 {
		t.Error("a refused upload was stored")
	}
}

// login through the browser: the CLI starts a login and opens the approval
// page, which the person approves while signed in; the CLI's next poll is
// given a token, and it works.
func TestLoginThroughTheBrowser(t *testing.T) {
	h := newAPIHarness(t)
	_, _, member := h.team("web-api-a")
	c := h.session(member, "web-api-a")
	var out, errOut bytes.Buffer
	approved := make(chan error, 1)
	env := cli.Env{
		Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), ConfigDir: t.TempDir(),
		Getenv:       func(string) string { return "" },
		PollInterval: 50 * time.Millisecond,
		OpenBrowser: func(u string) error {
			parsed, _ := url.Parse(u)
			code := parsed.Query().Get("code")
			if st, body := h.page("/cli/authorize?code="+code, c); st != http.StatusOK || !strings.Contains(body, code) {
				approved <- fmt.Errorf("approval page = %d", st)
				return nil
			}
			st, body := h.form("/cli/authorize", c, url.Values{"code": {code}, "role": {"member"}, "decision": {"approve"}})
			if st != http.StatusOK || !strings.Contains(body, "signed in") {
				approved <- fmt.Errorf("approve = %d", st)
				return nil
			}
			approved <- nil
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if code := cli.Run(ctx, cli.Brand{Name: "datahuru"}, []string{"login", "--host", h.base}, env); code != 0 {
		t.Fatalf("login exited %d: %s %s", code, out.String(), errOut.String())
	}
	if err := <-approved; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Signed in to") {
		t.Errorf("login said: %s", out.String())
	}
	raw, _ := os.ReadFile(filepath.Join(env.ConfigDir, "config.json"))
	var saved struct{ Token string }
	_ = json.Unmarshal(raw, &saved)
	if code, _ := h.call(http.MethodGet, "/api/v1/me", saved.Token, nil); code != http.StatusOK {
		t.Fatalf("the token login kept does not work: %d", code)
	}
	if info, _ := os.Stat(filepath.Join(env.ConfigDir, "config.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("the token file is %v, want 0600", info.Mode().Perm())
	}
}

func tarNames(t *testing.T, archive []byte) map[string]bool {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	out := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		out[hdr.Name] = true
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
