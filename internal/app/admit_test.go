package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// admitter is a wrapper's Admit: it keeps what it was asked, and refuses an
// owner's deploys when told to.
type admitter struct {
	mu      sync.Mutex
	asked   []AdmitRequest
	refused map[string]bool
}

const refusedSentence = "Deploys are paused for this workspace."

func (a *admitter) admit(_ context.Context, r AdmitRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, r)
	if a.refused[r.OwnerID] {
		return &AdmitRefusal{Message: refusedSentence}
	}
	return nil
}

func (a *admitter) refuse(owner string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refused == nil {
		a.refused = map[string]bool{}
	}
	a.refused[owner] = true
}

func (a *admitter) reset() []AdmitRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.asked
	a.asked = nil
	return out
}

// hooked gives a service an admitter.
func hooked(s *Service) *admitter {
	a := &admitter{}
	s.opts.Admit = a.admit
	return a
}

func isRefusal(err error) bool {
	var r *AdmitRefusal
	return errors.As(err, &r) && r.Message == refusedSentence && err.Error() == refusedSentence
}

// rows counts what a query finds, for "nothing was written".
func rows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// written is everything a deploy writes for an owner, to compare before and
// after a refusal.
func written(t *testing.T, pool *pgxpool.Pool, owner string) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"apps", "deployments", "deployment_operations", "app_releases",
		"projects", "environments", "source_uploads", "database_backups"} {
		parts = append(parts, fmt.Sprintf("%s=%d", table,
			rows(t, pool, `SELECT count(*) FROM `+table+` WHERE owner_id = $1`, owner)))
	}
	return strings.Join(parts, " ")
}

// Nil admits everything, as before there was a hook; and with one, every
// deploy is asked about once, as what it is.
func TestEveryDeployIsAskedAboutOnce(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "admit-once")
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "plain", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatalf("with no hook: %v", err)
	}

	a := hooked(s)
	web, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Redeploy(bg, id, "web"); err != nil {
		t.Fatal(err)
	}
	mustExecute(t, s, id, "web")
	if _, err := s.Scale(bg, id, "web", 2); err != nil {
		t.Fatal(err)
	}
	mustExecute(t, s, id, "web")
	if _, err := s.Update(bg, id, "web", UpdateInput{Image: "nginx:alpine", Port: 9090}); err != nil {
		t.Fatal(err)
	}
	mustExecute(t, s, id, "web")
	if err := s.SetHealth(bg, id, "web", "/healthz", false); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, r := range a.reset() {
		got = append(got, fmt.Sprintf("%s:%s:%d:%s", r.Trigger, r.App, r.Replicas, r.ActorKind))
		if r.OwnerID != id || r.Environment != DefaultEnvironment {
			t.Errorf("request %+v: want owner %s in production", r, id)
		}
		if r.Trigger != AdmitCreate && r.AppID != web.ID {
			t.Errorf("request %+v names app %s, want %s", r, r.AppID, web.ID)
		}
	}
	want := "create:web:1:user redeploy:web:1:user scale:web:2:user update:web:2:user update:web:2:user"
	if strings.Join(got, " ") != want {
		t.Errorf("asked\n%s\nwant\n%s", strings.Join(got, " "), want)
	}
}

// Refused, a deploy is the wrapper's sentence, unwrapped, and leaves
// nothing behind: no app, deployment, release or change of settings.
func TestARefusedDeployWritesNothing(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "admit-refused")
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	hooked(s).refuse(id)
	before := written(t, pool, id)
	app, _ := s.Get(bg, id, "web")

	for name, deploy := range map[string]func() error{
		"create": func() error {
			_, err := s.Create(bg, id, CreateInput{Name: "api", Image: "nginx:alpine", Replicas: 1, Port: 8080})
			return err
		},
		"redeploy": func() error { return s.Redeploy(bg, id, "web") },
		"scale": func() error {
			_, err := s.Scale(bg, id, "web", 3)
			return err
		},
		"update": func() error {
			_, err := s.Update(bg, id, "web", UpdateInput{Image: "nginx:alpine", Port: 9090})
			return err
		},
		"health":   func() error { return s.SetHealth(bg, id, "web", "/healthz", true) },
		"admitted": func() error { _, err := s.admitDeployment(bg, id, app, "redeploy", app.ID, false); return err },
	} {
		if err := deploy(); !isRefusal(err) {
			t.Errorf("%s: err = %v, want the refusal", name, err)
		}
		if after := written(t, pool, id); after != before {
			t.Errorf("%s wrote: %s, was %s", name, after, before)
		}
	}
	got, _ := s.Get(bg, id, "web")
	if got.Replicas != 1 || got.Port != 8080 || got.HealthPath != "" || got.ConfigVersion != app.ConfigVersion {
		t.Errorf("settings changed by refused deploys: %+v", got)
	}
}

func TestARefusedRollbackChangesNothing(t *testing.T) {
	bg := context.Background()
	s, _, _, id, a, first := rollbackFixture(t, "admit-rollback")
	hooked(s).refuse(id)
	before := written(t, s.pool, id)
	if err := s.Rollback(bg, id, a.Name, first.ID); !isRefusal(err) {
		t.Fatalf("Rollback: err = %v, want the refusal", err)
	}
	if after := written(t, s.pool, id); after != before {
		t.Errorf("a refused rollback wrote: %s, was %s", after, before)
	}
	if got, _ := s.Get(bg, id, a.Name); got.Image != "nginx:1.27" {
		t.Errorf("a refused rollback restored the image: %s", got.Image)
	}
}

func TestARefusedUploadStoresNothing(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{
		Images: fakeImages{}, Builder: &uploadBuilder{}, SourceFetchURL: "https://yacht.example.test",
	})
	id := owner(t, s, pool, "admit-upload")
	if _, err := s.Create(bg, id, CreateInput{Name: "api", Empty: true, Port: 8080, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	a := hooked(s)
	a.refuse(id)
	before := written(t, pool, id)
	_, _, err := s.DeployUpload(bg, id, "api", sourceArchive(t, map[string]string{"Dockerfile": "FROM busybox\n"}))
	if !isRefusal(err) {
		t.Fatalf("DeployUpload: err = %v, want the refusal", err)
	}
	if after := written(t, pool, id); after != before {
		t.Errorf("a refused upload wrote: %s, was %s", after, before)
	}
	if asked := a.reset(); len(asked) != 1 || asked[0].Trigger != AdmitUpload {
		t.Errorf("asked %+v", asked)
	}
}

func TestARefusedStagedDeployKeepsTheChangesWaiting(t *testing.T) {
	bg := context.Background()
	s, _, id := stagingService(t, "admit-staged", Options{})
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(bg, id, "web", VariableInput{Key: "MODE", Value: "b"}); err != nil {
		t.Fatal(err)
	}
	hooked(s).refuse(id)
	before := written(t, s.pool, id)
	if err := s.DeployStaged(bg, id, "web"); !isRefusal(err) {
		t.Fatalf("DeployStaged: err = %v, want the refusal", err)
	}
	if after := written(t, s.pool, id); after != before {
		t.Errorf("a refused staged deploy wrote: %s, was %s", after, before)
	}
	a, _ := s.Get(bg, id, "web")
	if changes, _ := s.stagedChanges(bg, a); len(changes) != 1 {
		t.Errorf("waiting after the refusal: %+v", changes)
	}
}

// A template's stack is refused whole, before its project exists.
func TestARefusedTemplateMakesNoProject(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	id := owner(t, s, pool, "admit-template")
	a := hooked(s)
	a.refuse(id)
	if _, err := s.DeployTemplate(bg, id, "postgres-app", "shop"); !isRefusal(err) {
		t.Fatalf("DeployTemplate: err = %v, want the refusal", err)
	}
	if got := written(t, pool, id); !strings.Contains(got, "apps=0 ") || !strings.Contains(got, "projects=0 ") {
		t.Errorf("a refused template wrote: %s", got)
	}
}

// An environment copied from another is refused before it exists, so there
// is no environment with some services and not others.
func TestARefusedEnvironmentIsNotMade(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "admit-environment")
	p, err := s.CreateProject(bg, id, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080, ProjectID: p.ID,
	}); err != nil {
		t.Fatal(err)
	}
	a := hooked(s)
	a.refuse(id)
	before := written(t, pool, id)
	if _, _, err := s.CreateEnvironment(bg, id, p.Slug, "Staging", DefaultEnvironment); !isRefusal(err) {
		t.Fatalf("CreateEnvironment: err = %v, want the refusal", err)
	}
	if after := written(t, pool, id); after != before {
		t.Errorf("a refused environment wrote: %s, was %s", after, before)
	}
	if asked := a.reset(); len(asked) != 1 || asked[0].Trigger != AdmitEnvironment ||
		asked[0].Environment != "staging" || asked[0].App != "web" || asked[0].ProjectID != p.ID {
		t.Errorf("asked %+v", asked)
	}
}

// A push to an app's own webhook that is refused is ignored, with the
// reason beside the delivery and in the app's settings, and admits nothing.
func TestARefusedPushIsIgnoredWithTheReason(t *testing.T) {
	bg := context.Background()
	s, id, a, secret := hookFixture(t, "admit-push")
	adm := hooked(s)
	adm.refuse(id)
	before := written(t, s.pool, id)
	result, err := s.DeliverPush(bg, a.ID, signedPush(secret, "push", "refs/heads/main", sha))
	if err != nil || result != "Ignored — "+refusedSentence {
		t.Fatalf("DeliverPush: %q, %v", result, err)
	}
	if after := written(t, s.pool, id); after != before {
		t.Errorf("a refused push wrote: %s, was %s", after, before)
	}
	if hook, _ := s.GitHook(bg, id, a.Name); hook.LastDelivery != result {
		t.Errorf("the app's last delivery = %q", hook.LastDelivery)
	}
	if asked := adm.reset(); len(asked) != 1 || asked[0].Trigger != AdmitPush ||
		asked[0].ActorKind != "webhook" || asked[0].Revision != "0123456" {
		t.Errorf("asked %+v", asked)
	}
}

// A push that waited behind a deploy, refused when its turn comes, is
// dropped with the reason: not deployed, and not tried again every pass.
func TestARefusedQueuedPushIsDropped(t *testing.T) {
	bg := context.Background()
	s, id, a, secret := hookFixture(t, "admit-queued")
	adm := hooked(s)
	if err := s.Redeploy(bg, id, a.Name); err != nil {
		t.Fatal(err)
	}
	if result, err := s.DeliverPush(bg, a.ID, signedPush(secret, "push", "refs/heads/main", sha)); err != nil ||
		!strings.HasPrefix(result, "Queued") {
		t.Fatalf("a push behind a deploy: %q, %v", result, err)
	}
	if err := s.CancelLiveDeployment(bg, id, a.Name); err != nil {
		t.Fatal(err)
	}
	adm.refuse(id)
	adm.reset()
	before := written(t, s.pool, id)
	for range 2 {
		if err := s.admitPendingPushes(bg); err != nil {
			t.Fatalf("admitPendingPushes: %v", err)
		}
	}
	if after := written(t, s.pool, id); after != before {
		t.Errorf("a refused queued push wrote: %s, was %s", after, before)
	}
	hook, _ := s.GitHook(bg, id, a.Name)
	if hook.Pending || hook.LastDelivery != "Ignored — "+refusedSentence {
		t.Errorf("after the refusal: pending %v, last %q", hook.Pending, hook.LastDelivery)
	}
	if asked := adm.reset(); len(asked) != 1 || asked[0].Trigger != AdmitQueuedPush {
		t.Errorf("asked %+v — want one queued push, once", asked)
	}
}

// A GitHub push reaching several teams is refused only for the teams the
// wrapper refuses; the others deploy.
func TestAGitHubPushIsRefusedTeamByTeam(t *testing.T) {
	bg := context.Background()
	s, _ := githubFixture(t, nil)
	teamA := owner(t, s, s.pool, "admit-gh-a")
	teamB := owner(t, s, s.pool, "admit-gh-b")
	for _, team := range []string{teamA, teamB} {
		connect(t, s, team, 7)
		if _, err := s.Create(bg, team, CreateInput{Name: "web", Source: SourceGit, Replicas: 1, Port: 8080,
			Repo: Repo{Branch: "main", GitHub: GitHubRef{InstallationID: 7, RepoID: webRepo.ID}}}); err != nil {
			t.Fatal(err)
		}
		if err := s.CancelLiveDeployment(bg, team, "web"); err != nil {
			t.Fatal(err)
		}
	}
	adm := hooked(s)
	adm.refuse(teamB)
	result, err := s.DeliverGitHub(bg, signedDelivery("admit-1", "push", pushTo(webRepo.ID, "acme/web", "main")))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{teamA + "/web: Deploying 0123456", teamB + "/web: Ignored — " + refusedSentence} {
		if !strings.Contains(result, want) {
			t.Errorf("result %q does not say %q", result, want)
		}
	}
	if got := liveTriggerOf(t, s, teamA, "web"); got != "push:0123456" {
		t.Errorf("team A's live deploy = %q", got)
	}
	if got := liveTriggerOf(t, s, teamB, "web"); got != "" {
		t.Errorf("team B's live deploy = %q, want none", got)
	}
	for _, r := range adm.reset() {
		if r.Trigger != AdmitGitHubPush {
			t.Errorf("asked %+v, want a GitHub push", r)
		}
	}
}

// A pull request's environment is not made in a project whose deploys are
// refused, and the delivery says why.
func TestARefusedPullRequestMakesNoEnvironment(t *testing.T) {
	bg := context.Background()
	s, _ := githubFixture(t, nil)
	team := owner(t, s, s.pool, "admit-pr")
	connect(t, s, team, 7)
	p, err := s.CreateProject(bg, team, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeployFromGitHub(bg, team, GitHubRef{InstallationID: 7, RepoID: webRepo.ID}, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPREnvironments(bg, team, p.Slug, true); err != nil {
		t.Fatal(err)
	}
	adm := hooked(s)
	adm.refuse(team)
	before := written(t, s.pool, team)
	got, err := s.gitHubPullRequest(bg, []byte(fmt.Sprintf(`{"action":"opened","number":7,
		"pull_request":{"head":{"ref":"feature/cart","repo":{"id":%d}}},
		"repository":{"id":%d,"full_name":"acme/web"},"installation":{"id":7}}`, webRepo.ID, webRepo.ID)))
	if err != nil || got != team+"/"+p.Slug+": Ignored — "+refusedSentence {
		t.Fatalf("a refused pull request: %q, %v", got, err)
	}
	if after := written(t, s.pool, team); after != before {
		t.Errorf("a refused pull request wrote: %s, was %s", after, before)
	}
	if asked := adm.reset(); len(asked) != 1 || asked[0].Trigger != AdmitPullRequest || asked[0].Environment != "pr-7" {
		t.Errorf("asked %+v", asked)
	}
}

// A database update is asked about before the backup it waits on.
func TestARefusedDatabaseUpdateStartsNoBackup(t *testing.T) {
	reg := pgRegistry()
	s, _, id := backupService(t, func(o *Options) { o.Manifests = reg })
	a, err := createAndDeploy(t, s, ctx(), id, CreateInput{Source: SourcePostgres, Name: "pg"})
	if err != nil {
		t.Fatal(err)
	}
	running(s, a, "16.15 (Debian 16.15-1.pgdg120+1)")
	reg.set("postgres:16-alpine", digestOf("b"))
	reg.set("postgres:16.16-alpine", digestOf("b"))
	s.tagDigests = nil
	hooked(s).refuse(id)
	before := written(t, s.pool, id)
	if _, err := s.UpdateDatabase(ctx(), id, "pg", "ana@example.com"); !isRefusal(err) {
		t.Fatalf("UpdateDatabase: err = %v, want the refusal", err)
	}
	if after := written(t, s.pool, id); after != before {
		t.Errorf("a refused update wrote: %s, was %s", after, before)
	}
}

// A hook that cannot decide fails the deploy, and is not a refusal: the
// person is not shown the wrapper's error.
func TestAnAdmitErrorFailsTheDeploy(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "admit-error")
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	s.opts.Admit = func(context.Context, AdmitRequest) error { return errors.New("store unreachable") }
	before := written(t, pool, id)
	err := s.Redeploy(bg, id, "web")
	if err == nil || isRefusal(err) || !strings.Contains(err.Error(), "admission") {
		t.Fatalf("Redeploy: err = %v, want a failed admission", err)
	}
	if after := written(t, pool, id); after != before {
		t.Errorf("a failed admission wrote: %s, was %s", after, before)
	}
}
