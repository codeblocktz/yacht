package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// admitter is a wrapper's Admit: it keeps what it was asked, and refuses an
// owner's deploys, or fails to decide on them, when told to.
type admitter struct {
	mu      sync.Mutex
	asked   []AdmitRequest
	refused map[string]bool
	broken  map[string]bool
}

const refusedSentence = "Deploys are paused for this workspace."

// brokenCause is what a hook that cannot decide says: nothing a person
// should be shown.
const brokenCause = "dial tcp 10.0.3.7:5432: password authentication failed for user ledger"

func (a *admitter) admit(_ context.Context, r AdmitRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, r)
	switch {
	case a.broken[r.OwnerID]:
		return errors.New(brokenCause)
	case a.refused[r.OwnerID]:
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

func (a *admitter) breakFor(owner string, broken bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.broken == nil {
		a.broken = map[string]bool{}
	}
	a.broken[owner] = broken
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

// saysRefused reports whether err is the admitter's refusal, as itself.
func saysRefused(err error) bool {
	r, ok := isRefusal(err)
	return ok && r.Message == refusedSentence && err.Error() == refusedSentence
}

// written is a fingerprint of every row a deploy could write for an owner,
// to compare before and after a refusal: a changed value changes it, not
// only a new row.
func written(t *testing.T, pool *pgxpool.Pool, owner string) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"apps", "deployments", "deployment_operations", "app_releases",
		"projects", "environments", "source_uploads", "database_backups",
		"variables", "staged_variables", "volumes"} {
		var sum string
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) || ':' || md5(coalesce(string_agg(t::text, '|' ORDER BY t::text), ''))
			 FROM `+table+` t WHERE owner_id = $1`, owner).Scan(&sum); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		parts = append(parts, table+"="+sum)
	}
	return strings.Join(parts, " ")
}

// Nil admits everything, as before there was a hook; and with one, every
// deploy is asked about once, as what it is — and a change that deploys
// nothing is not asked about.
func TestEveryDeployIsAskedAboutOnce(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	id := owner(t, s, pool, "admit-once")
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "plain", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatalf("with no hook: %v", err)
	}

	a := hooked(s)
	web, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	steps := []func() error{
		func() error { return s.Redeploy(bg, id, "web") },
		func() error { _, err := s.Scale(bg, id, "web", 2); return err },
		func() error { _, err := s.Scale(bg, id, "web", 2); return err }, // nothing to do
		func() error {
			_, err := s.Update(bg, id, "web", UpdateInput{Image: "nginx:alpine", Port: 9090})
			return err
		},
		func() error { return s.SetHealth(bg, id, "web", "/healthz", false) },
		func() error { return s.SetHealth(bg, id, "web", "/healthz", false) }, // nothing to do
		func() error { return s.SetVariable(bg, id, "web", VariableInput{Key: "MODE", Value: "a"}) },
		func() error {
			return s.SetVariable(bg, id, "web", VariableInput{Key: "TOKEN", Value: "x", Secret: true})
		},
		func() error { return s.DeleteVariable(bg, id, "web", "TOKEN") },
		func() error { return s.DeleteVariable(bg, id, "web", "MODE") },
		func() error { return s.SetRawVariables(bg, id, "web", "MODE=b\n") },
		func() error { return s.SetRawVariables(bg, id, "web", "MODE=b\n") }, // nothing to do
		func() error { _, err := s.Scale(bg, id, "web", 0); return err },     // a stop
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if a, err := s.Get(bg, id, "web"); err == nil {
			if _, err := s.liveOperation(bg, a); err == nil {
				mustExecute(t, s, id, "web")
			}
		}
	}

	var got []string
	for _, r := range a.reset() {
		got = append(got, fmt.Sprintf("%s:%s:%d/%d:%s", r.Trigger, r.App, r.Replicas, r.CurrentReplicas, r.ActorKind))
		if r.OwnerID != id || r.Environment != DefaultEnvironment {
			t.Errorf("request %+v: want owner %s in production", r, id)
		}
		if r.Trigger != AdmitCreate && r.AppID != web.ID {
			t.Errorf("request %+v names app %s, want %s", r, r.AppID, web.ID)
		}
	}
	want := "create:web:1/0:user redeploy:web:1/1:user scale:web:2/1:user update:web:2/2:user " +
		"update:web:2/2:user update:web:2/2:user update:web:2/2:user update:web:2/2:user"
	if strings.Join(got, " ") != want {
		t.Errorf("asked\n%s\nwant\n%s", strings.Join(got, " "), want)
	}

	// A wrapper's own call says so.
	if err := s.Redeploy(WithActor(bg, Actor{Kind: "system"}), id, "plain"); err != nil {
		t.Fatal(err)
	}
	if asked := a.reset(); len(asked) != 1 || asked[0].ActorKind != "system" {
		t.Errorf("a system redeploy was asked as %+v", asked)
	}
}

// Refused, a deploy is the wrapper's sentence, unwrapped, and leaves
// nothing behind: no app, deployment, release, variable or volume, and no
// change of settings.
func TestARefusedDeployWritesNothing(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	id := owner(t, s, pool, "admit-refused")
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		Env: map[string]string{"MODE": "a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "db", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachVolume(bg, id, "db", VolumeInput{Name: "data", MountPath: "/data", SizeBytes: 1 << 30}); err != nil {
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
		"health":        func() error { return s.SetHealth(bg, id, "web", "/healthz", true) },
		"set variable":  func() error { return s.SetVariable(bg, id, "web", VariableInput{Key: "MODE", Value: "b"}) },
		"new variable":  func() error { return s.SetVariable(bg, id, "web", VariableInput{Key: "NEW", Value: "x"}) },
		"delete":        func() error { return s.DeleteVariable(bg, id, "web", "MODE") },
		"raw variables": func() error { return s.SetRawVariables(bg, id, "web", "MODE=c\nOTHER=1\n") },
		"attach": func() error {
			_, err := s.AttachVolume(bg, id, "web", VolumeInput{Name: "data", MountPath: "/data", SizeBytes: 1 << 30})
			return err
		},
		"resize":   func() error { return s.ResizeVolume(bg, id, "db", "data", 2<<30) },
		"fallback": func() error { _, err := s.admitDeployment(bg, id, app, "redeploy", app.ID, false); return err },
	} {
		if err := deploy(); !saysRefused(err) {
			t.Errorf("%s: err = %v, want the refusal", name, err)
		}
		if after := written(t, pool, id); after != before {
			t.Errorf("%s wrote:\n%s\nwas\n%s", name, after, before)
		}
	}

	// A stop is never refused.
	if _, err := s.Scale(bg, id, "web", 0); err != nil {
		t.Errorf("a scale to zero: %v", err)
	}
}

// What Update asks about is worked out before its transaction, from what it
// will store: it must be what it stores.
func TestUpdateAsksAboutWhatItStores(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "admit-update-shape")
	a, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	in := UpdateInput{
		Image: "nginx:1.27", Port: 9090, Internal: true,
		CPURequest: "100m", CPULimit: "500m", MemoryRequest: "64Mi", MemoryLimit: "256Mi",
		StartCommand: "  serve  ", PreDeployCommand: " migrate ", RestartPolicy: "always",
		HealthTimeoutSeconds: 30,
	}
	predicted := a.updatedBy(in, in.Image)
	if _, err := s.Update(bg, id, "web", in); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.Get(bg, id, "web")
	if RollsOut(stored, predicted) || RollsOut(predicted, stored) {
		t.Errorf("predicted %+v\nstored %+v", predicted, stored)
	}
}

// A region move is asked about, and refused moves nothing.
func TestARefusedMoveStaysPut(t *testing.T) {
	bg := context.Background()
	s, _, id := regionService(t, Options{})
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080, Region: "fsn",
	}); err != nil {
		t.Fatal(err)
	}
	adm := hooked(s)
	adm.refuse(id)
	before := written(t, s.pool, id)
	if err := s.MoveApp(bg, id, "web", "dar"); !saysRefused(err) {
		t.Fatalf("MoveApp: err = %v, want the refusal", err)
	}
	if after := written(t, s.pool, id); after != before {
		t.Errorf("a refused move wrote: %s, was %s", after, before)
	}
	if asked := adm.reset(); len(asked) != 1 || asked[0].Trigger != AdmitMove || asked[0].Region != "dar" {
		t.Errorf("asked %+v", asked)
	}
}

// A hook that cannot decide fails the deploy with a sentence of the
// engine's own: what went wrong is in the log, never shown.
func TestAnAdmitErrorFailsTheDeploy(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "admit-error")
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	hooked(s).breakFor(id, true)
	before := written(t, pool, id)
	err := s.Redeploy(bg, id, "web")
	if !errors.Is(err, ErrAdmissionUnavailable) || strings.Contains(err.Error(), "10.0.3.7") ||
		strings.Contains(err.Error(), "password") {
		t.Fatalf("Redeploy: err = %v, want ErrAdmissionUnavailable without the cause", err)
	}
	if after := written(t, pool, id); after != before {
		t.Errorf("a failed admission wrote: %s, was %s", after, before)
	}

	// Too slow is the same.
	saved := admitTimeout
	admitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { admitTimeout = saved })
	s.opts.Admit = func(ctx context.Context, _ AdmitRequest) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := s.Redeploy(bg, id, "web"); !errors.Is(err, ErrAdmissionUnavailable) {
		t.Errorf("a hook that does not answer: err = %v", err)
	}
}

// A refusal with no sentence gets one; a typed nil is the hook's fault.
func TestARefusalAlwaysSaysSomething(t *testing.T) {
	bg := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "admit-edges")
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080}); err != nil {
		t.Fatal(err)
	}
	s.opts.Admit = func(context.Context, AdmitRequest) error { return &AdmitRefusal{} }
	if err := s.Redeploy(bg, id, "web"); err == nil || err.Error() != defaultRefusal {
		t.Errorf("an empty refusal: %v", err)
	}
	s.opts.Admit = func(context.Context, AdmitRequest) error { return (*AdmitRefusal)(nil) }
	if err := s.Redeploy(bg, id, "web"); !errors.Is(err, ErrAdmissionUnavailable) {
		t.Errorf("a typed nil refusal: %v", err)
	}
}
func TestARefusedRollbackChangesNothing(t *testing.T) {
	bg := context.Background()
	s, _, _, id, a, first := rollbackFixture(t, "admit-rollback")
	hooked(s).refuse(id)
	before := written(t, s.pool, id)
	if err := s.Rollback(bg, id, a.Name, first.ID); !saysRefused(err) {
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
	if !saysRefused(err) {
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
	if err := s.DeployStaged(bg, id, "web"); !saysRefused(err) {
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
	if _, err := s.DeployTemplate(bg, id, "postgres-app", "shop"); !saysRefused(err) {
		t.Fatalf("DeployTemplate: err = %v, want the refusal", err)
	}
	if got := written(t, pool, id); !strings.Contains(got, "apps=0:") || !strings.Contains(got, "projects=0:") {
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
	if _, _, err := s.CreateEnvironment(bg, id, p.Slug, "Staging", DefaultEnvironment); !saysRefused(err) {
		t.Fatalf("CreateEnvironment: err = %v, want the refusal", err)
	}
	if after := written(t, pool, id); after != before {
		t.Errorf("a refused environment wrote: %s, was %s", after, before)
	}
	if asked := a.reset(); len(asked) != 1 || asked[0].Trigger != AdmitEnvironment ||
		asked[0].Environment != "staging" || asked[0].App != "web-staging" || asked[0].ProjectID != p.ID {
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
	if _, err := s.UpdateDatabase(ctx(), id, "pg", "ana@example.com"); !saysRefused(err) {
		t.Fatalf("UpdateDatabase: err = %v, want the refusal", err)
	}
	if after := written(t, s.pool, id); after != before {
		t.Errorf("a refused update wrote: %s, was %s", after, before)
	}
}

// A queued push the hook could not decide on stays queued, and is not asked
// about again until its wait is up. A refusal drops only the push that was
// refused: a newer one queued since stays.
func TestAQueuedPushWaitsOutAHookThatCannotDecide(t *testing.T) {
	bg := context.Background()
	s, id, a, secret := hookFixture(t, "admit-queued-wait")
	adm := hooked(s)
	if err := s.Redeploy(bg, id, a.Name); err != nil {
		t.Fatal(err)
	}
	if result, _ := s.DeliverPush(bg, a.ID, signedPush(secret, "push", "refs/heads/main", sha)); !strings.HasPrefix(result, "Queued") {
		t.Fatalf("a push behind a deploy: %q", result)
	}
	if err := s.CancelLiveDeployment(bg, id, a.Name); err != nil {
		t.Fatal(err)
	}
	adm.breakFor(id, true)
	adm.reset()
	for range 3 {
		if err := s.admitPendingPushes(bg); err != nil {
			t.Fatal(err)
		}
	}
	if asked := adm.reset(); len(asked) != 1 {
		t.Errorf("a hook that cannot decide was asked %d times in three passes, want once", len(asked))
	}
	if hook, _ := s.GitHook(bg, id, a.Name); !hook.Pending {
		t.Error("a push the hook could not decide on was dropped")
	}

	// Refused for a revision that is no longer the one queued: kept.
	s.dropQueuedPush(bg, a, "fedcba9", refusedDelivery(&AdmitRefusal{Message: refusedSentence}))
	if hook, _ := s.GitHook(bg, id, a.Name); !hook.Pending || strings.Contains(hook.LastDelivery, refusedSentence) {
		t.Errorf("a newer queued push was dropped for an older one's refusal: %+v", hook)
	}

	// Answered again, it goes ahead.
	adm.breakFor(id, false)
	s.pushBackoff.cleared(a.ID)
	if err := s.admitPendingPushes(bg); err != nil {
		t.Fatal(err)
	}
	if got := liveTriggerOf(t, s, id, a.Name); got != "push:0123456" {
		t.Errorf("after the hook answered, the live deploy is %q", got)
	}
}

// A GitHub push some of whose apps could not be checked fails, so it can be
// sent again, and sent again it reaches only those apps: the others took it
// the first time. Nothing of the hook's error is said.
func TestAGitHubPushThatCouldNotBeCheckedCanBeSentAgain(t *testing.T) {
	bg := context.Background()
	s, _ := githubFixture(t, nil)
	teamA := owner(t, s, s.pool, "admit-gh-unchecked-a")
	teamB := owner(t, s, s.pool, "admit-gh-unchecked-b")
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
	adm.breakFor(teamB, true)
	d := signedDelivery("admit-unchecked", "push", pushTo(webRepo.ID, "acme/web", "main"))
	if _, err := s.DeliverGitHub(bg, d); !errors.Is(err, errPushUnchecked) {
		t.Fatalf("a push one app could not be checked for: err = %v", err)
	}
	if hook, _ := s.GitHook(bg, teamB, "web"); hook.LastDelivery != uncheckedPush {
		t.Errorf("team B's last delivery = %q", hook.LastDelivery)
	}
	before := len(s.pushTriggersOf(t, teamA))

	adm.breakFor(teamB, false)
	result, err := s.DeliverGitHub(bg, d)
	if err != nil {
		t.Fatalf("sent again: %v", err)
	}
	if strings.Contains(result, teamA+"/") || !strings.Contains(result, teamB+"/web: Deploying 0123456") {
		t.Errorf("sent again: %q", result)
	}
	if after := len(s.pushTriggersOf(t, teamA)); after != before {
		t.Errorf("team A took the push %d times", after)
	}
}

// pushTriggersOf is every push deployment of a team's web.
func (s *Service) pushTriggersOf(t *testing.T, team string) []string {
	t.Helper()
	a, err := s.Get(context.Background(), team, "web")
	if err != nil {
		t.Fatal(err)
	}
	deps, err := s.Deployments(context.Background(), team, a.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range deps {
		if strings.HasPrefix(d.Trigger, PushTriggerPrefix) {
			out = append(out, d.Trigger)
		}
	}
	return out
}

// A scale changes the replica count and nothing else. With changes staged —
// a variable, a setting — a scale up, a scale down and a stop each run the
// release the app was running at the new count, and what is staged stays
// staged until it is deployed, which is asked about as a staged deploy.
func TestAScaleDeploysNothingStaged(t *testing.T) {
	bg := context.Background()
	s, _, id := stagingService(t, "admit-scale-staged", Options{})
	if _, err := createAndDeploy(t, s, bg, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080, Env: map[string]string{"MODE": "a"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(bg, id, "web", VariableInput{Key: "MODE", Value: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(bg, id, "web", UpdateInput{Image: "nginx:alpine", Port: 9090}); err != nil {
		t.Fatal(err)
	}
	adm := hooked(s)
	for _, n := range []int32{3, 2, 0} {
		if _, err := s.Scale(bg, id, "web", n); err != nil {
			t.Fatalf("Scale %d: %v", n, err)
		}
		mustExecute(t, s, id, "web")
		a, _ := s.Get(bg, id, "web")
		rel := activeRelease(t, s, id, "web")
		if rel.Replicas != n || rel.Env["MODE"] != "a" || rel.Port != 8080 {
			t.Errorf("after scaling to %d the release runs x%d, MODE=%q, port %d — want x%d, MODE=a, port 8080",
				n, rel.Replicas, rel.Env["MODE"], rel.Port, n)
		}
		if changes, _ := s.stagedChanges(bg, a); len(changes) != 2 {
			t.Errorf("after scaling to %d, staged: %+v — want MODE and the port still waiting", n, changes)
		}
	}
	adm.reset()
	if err := s.DeployStaged(bg, id, "web"); err != nil {
		t.Fatal(err)
	}
	if asked := adm.reset(); len(asked) != 1 || asked[0].Trigger != AdmitStaged {
		t.Errorf("deploying what was staged was asked as %+v", asked)
	}
	mustExecute(t, s, id, "web")
	if rel := activeRelease(t, s, id, "web"); rel.Env["MODE"] != "b" || rel.Port != 9090 || rel.Replicas != 0 {
		t.Errorf("after deploying what was staged the release runs x%d, MODE=%q, port %d", rel.Replicas, rel.Env["MODE"], rel.Port)
	}
}
