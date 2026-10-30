package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/secret"
)

// stagingService is the product's service: edits wait for DeployStaged.
func stagingService(t *testing.T, team string, opts Options) (*Service, *recordingOrchestrator, string) {
	t.Helper()
	keeper, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32)))
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	opts.Keeper, opts.StageChanges = keeper, true
	s, orch, pool := testService(t, opts)
	return s, orch, owner(t, s, pool, team)
}

func changeKeys(changes []StagedChange) string {
	var out []string
	for _, c := range changes {
		out = append(out, c.Kind+":"+c.Key)
	}
	return strings.Join(out, " ")
}

// A saved variable deploys nothing, reaches no cluster — not even through
// the reconciler, which applies whenever anything about an app moves — and
// is listed as waiting. Deploy ships every edit as one deployment.
func TestAStagedVariableWaitsForDeploy(t *testing.T) {
	ctx := context.Background()
	s, orch, id := stagingService(t, "svc-staged-waits", Options{})

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		Env: map[string]string{"OLD": "1", "MODE": "a"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, in := range []VariableInput{
		{Key: "MODE", Value: "b"},
		{Key: "TOKEN", Value: "hunter2", Secret: true},
		{Key: "NEW", Value: "x"},
	} {
		if err := s.SetVariable(ctx, id, "web", in); err != nil {
			t.Fatalf("set %s: %v", in.Key, err)
		}
	}
	if err := s.DeleteVariable(ctx, id, "web", "OLD"); err != nil {
		t.Fatalf("delete OLD: %v", err)
	}

	a, _ := s.Get(ctx, id, "web")
	if _, err := s.liveOperation(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a staged save admitted a deploy: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Secrets["TOKEN"] != "" || spec.Env["MODE"] != "a" {
		t.Fatalf("a staged edit reached the cluster: env %v secrets %v", spec.Env, spec.Secrets)
	}

	changes, err := s.StagedChanges(ctx, id, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := changeKeys(changes), "changed:MODE added:NEW removed:OLD added:TOKEN"; got != want {
		t.Fatalf("staged = %q, want %q", got, want)
	}
	// The page lists what was typed, marked; the removal is not listed.
	listed := map[string]string{}
	for _, v := range a.Variables {
		listed[v.Key] = v.Staged
	}
	if _, gone := listed["OLD"]; gone || listed["MODE"] != StagedChanged || listed["TOKEN"] != StagedAdded {
		t.Errorf("listed = %v", listed)
	}

	if err := s.DeployStaged(ctx, id, "web"); err != nil {
		t.Fatalf("DeployStaged: %v", err)
	}
	// Admitted means it is no longer waiting, before the worker has run.
	if changes, _ := s.StagedChanges(ctx, id, "web"); len(changes) != 0 {
		t.Errorf("after deploying, still staged: %s", changeKeys(changes))
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	spec := orch.lastAppSpec()
	if spec.Env["MODE"] != "b" || spec.Env["NEW"] != "x" || spec.Env["OLD"] != "" || spec.Secrets["TOKEN"] != "hunter2" {
		t.Errorf("deployed env %v secrets %v", spec.Env, spec.Secrets)
	}
	deps, _ := s.Deployments(ctx, id, a.ID, 5)
	if len(deps) == 0 || !strings.HasPrefix(deps[0].Revision, "changes:") {
		t.Errorf("the deployment says %+v", deps)
	}
	if err := s.DeployStaged(ctx, id, "web"); !errors.Is(err, ErrNothingStaged) {
		t.Errorf("deploying nothing = %v", err)
	}
}

// Saving while a deploy runs used to be a half-failure. Staged, it is
// simply a save: it waits for the next deploy like any other.
func TestSavingWhileDeployingStages(t *testing.T) {
	ctx := context.Background()
	s, _, id := stagingService(t, "svc-staged-busy", Options{})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "A", Value: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeployStaged(ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "B", Value: "2"}); err != nil {
		t.Fatalf("a save during a deploy = %v, want it staged", err)
	}
	if err := s.DeployStaged(ctx, id, "web"); !errors.Is(err, ErrOperationInFlight) {
		t.Errorf("a second deploy while one runs = %v", err)
	}
	changes, _ := s.StagedChanges(ctx, id, "web")
	if got := changeKeys(changes); got != "added:B" {
		t.Errorf("staged during the deploy = %q, want added:B alone", got)
	}
}

// Settings that roll out are saved on the app and listed against the release
// it runs, with their values; Discard puts them back, and every staged
// variable edit with them.
func TestDiscardPutsEverythingBack(t *testing.T) {
	ctx := context.Background()
	s, orch, id := stagingService(t, "svc-staged-discard", Options{})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		Env: map[string]string{"MODE": "a"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "TOKEN", Value: "first", Secret: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeployStaged(ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Update(ctx, id, "web", UpdateInput{Image: "nginx:alpine", Port: 9090, CPULimit: "500m"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.SetHealth(ctx, id, "web", "/healthz", false); err != nil {
		t.Fatalf("SetHealth: %v", err)
	}
	for _, in := range []VariableInput{{Key: "MODE", Value: "b"}, {Key: "TOKEN", Value: "second", Secret: true}} {
		if err := s.SetVariable(ctx, id, "web", in); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := s.Get(ctx, id, "web")
	if _, err := s.liveOperation(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatal("a staged settings change admitted a deploy")
	}
	changes, err := s.StagedChanges(ctx, id, "web")
	if err != nil {
		t.Fatal(err)
	}
	var port *StagedChange
	for i := range changes {
		if changes[i].Setting && changes[i].Key == "Port" {
			port = &changes[i]
		}
	}
	if port == nil || port.From != "8080" || port.To != "9090" {
		t.Fatalf("staged = %+v", changes)
	}
	if got := changeKeys(changes); !strings.Contains(got, "changed:MODE") ||
		!strings.Contains(got, "changed:TOKEN") || !strings.Contains(got, "changed:CPU limit") ||
		!strings.Contains(got, "changed:Health check") {
		t.Errorf("staged = %q", got)
	}

	kept, err := s.DiscardStaged(ctx, id, "web")
	if err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if len(kept) != 0 {
		t.Errorf("kept %v, want nothing: every edit was staged", kept)
	}
	if changes, _ := s.StagedChanges(ctx, id, "web"); len(changes) != 0 {
		t.Errorf("after discarding, staged = %q", changeKeys(changes))
	}
	a, _ = s.Get(ctx, id, "web")
	if a.Port != 8080 || a.CPULimit != "" || a.HealthPath != "" {
		t.Errorf("settings after discard: port %d cpu %q health %q", a.Port, a.CPULimit, a.HealthPath)
	}
	if v, err := s.RevealVariable(ctx, id, "web", "TOKEN"); err != nil || v != "first" {
		t.Errorf("the secret after discard = %q, %v; want the deployed one", v, err)
	}
	// And the next deploy runs what was running.
	if err := s.Redeploy(ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if spec := orch.lastAppSpec(); spec.Env["MODE"] != "a" || spec.Secrets["TOKEN"] != "first" || spec.Port != 8080 {
		t.Errorf("redeployed env %v secrets %v port %d", spec.Env, spec.Secrets, spec.Port)
	}
}

// A secret saved into the deployed variables without a deploy that ran with
// it has no earlier value anywhere. Discard keeps it, and says so before it
// is pressed.
func TestDiscardKeepsASecretItCannotRestore(t *testing.T) {
	ctx := context.Background()
	s, _, id := stagingService(t, "svc-staged-kept", Options{})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "TOKEN", Value: "first", Secret: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeployStaged(ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	// Written straight into the deployed variables, as the immediate path
	// does for a secret.
	if err := s.setVariableNow(ctx, id, "web", VariableInput{Key: "TOKEN", Value: "second", Secret: true}); err != nil {
		t.Fatal(err)
	}
	changes, err := s.StagedChanges(ctx, id, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Kept || !strings.Contains(changes[0].Note, "keeps") {
		t.Fatalf("staged = %+v", changes)
	}
	kept, err := s.DiscardStaged(ctx, id, "web")
	if err != nil || len(kept) != 1 || kept[0] != "TOKEN" {
		t.Errorf("discard kept %v, %v", kept, err)
	}
	if v, _ := s.RevealVariable(ctx, id, "web", "TOKEN"); v != "second" {
		t.Errorf("TOKEN = %q, want the current value kept", v)
	}
}

// Any deploy carries what is saved: a redeploy as much as the bar's.
func TestARedeployCarriesStagedVariables(t *testing.T) {
	ctx := context.Background()
	s, orch, id := stagingService(t, "svc-staged-redeploy", Options{})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "A", Value: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Redeploy(ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if orch.lastAppSpec().Env["A"] != "1" {
		t.Errorf("the redeploy left A behind: %v", orch.lastAppSpec().Env)
	}
	if changes, _ := s.StagedChanges(ctx, id, "web"); len(changes) != 0 {
		t.Errorf("still staged: %q", changeKeys(changes))
	}
}

// Typing a value back to what is deployed is no change.
func TestAnEditBackToTheDeployedValueIsNoChange(t *testing.T) {
	ctx := context.Background()
	s, _, id := stagingService(t, "svc-staged-noop", Options{})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		Env: map[string]string{"MODE": "a"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"b", "a"} {
		if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "MODE", Value: v}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "GONE", Value: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVariable(ctx, id, "web", "GONE"); err != nil {
		t.Fatal(err)
	}
	if changes, _ := s.StagedChanges(ctx, id, "web"); len(changes) != 0 {
		t.Errorf("staged = %q, want nothing", changeKeys(changes))
	}
	if err := s.DeleteVariable(ctx, id, "web", "GONE"); !errors.Is(err, ErrVariableNotFound) {
		t.Errorf("deleting a variable that never deployed twice = %v", err)
	}
}

// Deploying a whole project deploys each app after those it refers to, so an
// API pointed at a variable the database has only staged deploys after the
// database does — and alone, it is refused with that reason.
func TestDeployingAProjectOrdersByReference(t *testing.T) {
	ctx := context.Background()
	s, orch, id := stagingService(t, "svc-staged-project", Options{})
	for _, name := range []string{"api", "db"} {
		if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
			Name: name, Image: "nginx:alpine", Replicas: 1, Port: 8080,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetVariable(ctx, id, "db", VariableInput{Key: "PASSWORD", Value: "pw", Secret: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(ctx, id, "api", VariableInput{Key: "DB_PASSWORD", Value: "${{db.PASSWORD}}"}); err != nil {
		t.Fatalf("a reference to a staged variable: %v", err)
	}
	if err := s.DeployStaged(ctx, id, "api"); err == nil || !strings.Contains(err.Error(), "not deployed yet") {
		t.Fatalf("deploying the API alone = %v", err)
	}

	all, err := s.ProjectStagedChanges(ctx, id, DefaultProjectSlug)
	if err != nil || len(all) != 2 {
		t.Fatalf("project staged = %+v, %v", all, err)
	}
	deployed, err := s.DeployProjectStaged(ctx, id, DefaultProjectSlug)
	if err != nil {
		t.Fatalf("DeployProjectStaged: %v", err)
	}
	if strings.Join(deployed, ",") != "db,api" {
		t.Errorf("deployed %v, want db then api", deployed)
	}
	if err := executeNamedOperation(s, ctx, id, "api"); err != nil {
		t.Fatal(err)
	}
	if orch.lastAppSpec().Secrets["DB_PASSWORD"] != "pw" {
		t.Errorf("api's reference resolved to %q", orch.lastAppSpec().Secrets["DB_PASSWORD"])
	}
	if err := executeNamedOperation(s, ctx, id, "db"); err != nil {
		t.Fatal(err)
	}

	// The database's password changes and the database alone deploys. The
	// API reads it only at its own deploy, so it now has a change waiting
	// that Discard cannot take back.
	if err := s.SetVariable(ctx, id, "db", VariableInput{Key: "PASSWORD", Value: "pw2", Secret: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeployStaged(ctx, id, "db"); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx, id, "db"); err != nil {
		t.Fatal(err)
	}
	changes, _ := s.StagedChanges(ctx, id, "api")
	if len(changes) != 1 || !changes[0].Kept || !strings.Contains(changes[0].Note, "db.PASSWORD") {
		t.Fatalf("api after db's password changed: %+v", changes)
	}

	// Deploying the project from there redeploys the API with the new one.
	deployed, err = s.DeployProjectStaged(ctx, id, DefaultProjectSlug)
	if err != nil || strings.Join(deployed, ",") != "api" {
		t.Fatalf("deployed %v, %v; want the API alone", deployed, err)
	}
	if err := executeNamedOperation(s, ctx, id, "api"); err != nil {
		t.Fatal(err)
	}
	if orch.lastAppSpec().Secrets["DB_PASSWORD"] != "pw2" {
		t.Errorf("api's reference resolved to %q", orch.lastAppSpec().Secrets["DB_PASSWORD"])
	}
}

// The raw editor replaces every variable that is not a secret, and stages
// it; a secret is neither shown in it nor touchable from it.
func TestTheRawEditorReplacesThePlainSet(t *testing.T) {
	ctx := context.Background()
	s, _, id := stagingService(t, "svc-staged-raw", Options{})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		Env: map[string]string{"KEEP": "1", "DROP": "2", "EDIT": "3"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "TOKEN", Value: "t", Secret: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRawVariables(ctx, id, "web", "KEEP=1\nEDIT=\"three # not a comment\"\nADD=4 # a comment\n"); err != nil {
		t.Fatalf("SetRawVariables: %v", err)
	}
	changes, _ := s.StagedChanges(ctx, id, "web")
	if got, want := changeKeys(changes), "added:ADD removed:DROP changed:EDIT added:TOKEN"; got != want {
		t.Errorf("staged = %q, want %q", got, want)
	}
	if v, _ := s.RevealVariable(ctx, id, "web", "EDIT"); v != "three # not a comment" {
		t.Errorf("EDIT = %q", v)
	}
	if err := s.SetRawVariables(ctx, id, "web", "TOKEN=plain"); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Errorf("writing a secret from the raw editor = %v", err)
	}
	a, _ := s.Get(ctx, id, "web")
	if raw := FormatEnv(a.Variables); strings.Contains(raw, "TOKEN") || !strings.Contains(raw, `EDIT="three # not a comment"`) {
		t.Errorf("raw editor text:\n%s", raw)
	}
}

// The platform tells every app where it is and what it is called, under the
// prefix the product chose; a variable of the app's own wins.
func TestThePlatformVariablesReachEveryApp(t *testing.T) {
	ctx := context.Background()
	s, orch, id := stagingService(t, "svc-platform-vars", Options{
		AppDomain: "apps.example.com", PlatformVariablePrefix: "DATAHURU_",
	})
	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		Env: map[string]string{"DATAHURU_SERVICE_NAME": "mine"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A public address only once asked for, as on Railway; generating one
	// re-applies the app, with the variable that names it.
	if _, err := s.GenerateDomain(ctx, id, "web", 0); err != nil {
		t.Fatalf("GenerateDomain: %v", err)
	}
	// The route reaches the workload on the reconciler's next pass.
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if a, err = s.Get(ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	env := orch.lastAppSpec().Env
	want := map[string]string{
		"DATAHURU_PRIVATE_DOMAIN": "web." + a.Namespace + ".svc.cluster.local",
		"DATAHURU_PUBLIC_DOMAIN":  a.Host,
		"DATAHURU_PROJECT_NAME":   DefaultProjectName,
		"DATAHURU_SERVICE_NAME":   "mine",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if a.Host == "" {
		t.Error("the app has no host to be told about")
	}
	// Six: the team's default project has not been made yet, so it has no
	// id to give, and neither has its production.
	listed, err := s.PlatformVariables(ctx, id, "web")
	if err != nil || len(listed) != 6 || listed[0].Key != "DATAHURU_PUBLIC_DOMAIN" {
		t.Errorf("listed = %+v, %v", listed, err)
	}
	if err := ValidatePlatformVariablePrefix("9-bad"); err == nil {
		t.Error("a prefix no shell exports was accepted")
	}
}

// ${{shared.KEY}} reads the project's shared variable at deploy, a secret
// opened; an app cannot take the name, and a shared variable in use cannot
// be removed from under it.
func TestASharedVariableResolvesAtDeploy(t *testing.T) {
	ctx := context.Background()
	s, orch, id := stagingService(t, "svc-shared-vars", Options{})
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, id, CreateInput{Name: SharedScope, Image: "nginx:alpine", Replicas: 1}); err == nil {
		t.Error("an app called shared was created")
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "KEY", Value: "${{shared.SIGNING_KEY}}"}); err == nil {
		t.Error("a reference to a shared variable that does not exist was accepted")
	}
	if err := s.SetSharedVariable(ctx, id, DefaultProjectSlug, VariableInput{Key: "SIGNING_KEY", Value: "k1", Secret: true}); err != nil {
		t.Fatalf("SetSharedVariable: %v", err)
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "KEY", Value: "${{shared.SIGNING_KEY}}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeployStaged(ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if got := orch.lastAppSpec().Secrets["KEY"]; got != "k1" {
		t.Errorf("KEY reached the cluster as %q", got)
	}

	targets, _ := s.ReferenceTargets(ctx, id, "web")
	if len(targets) == 0 || targets[0].App != SharedScope || targets[0].Keys[0].Key != "SIGNING_KEY" {
		t.Errorf("the picker offers %+v", targets)
	}
	if err := s.DeleteSharedVariable(ctx, id, DefaultProjectSlug, "SIGNING_KEY"); err == nil ||
		!strings.Contains(err.Error(), "web") {
		t.Errorf("removing a shared variable in use = %v", err)
	}
	if v, err := s.RevealSharedVariable(ctx, id, DefaultProjectSlug, "SIGNING_KEY"); err != nil || v != "k1" {
		t.Errorf("reveal = %q, %v", v, err)
	}

	// Changed, it waits for the apps that use it to deploy.
	if err := s.SetSharedVariable(ctx, id, DefaultProjectSlug, VariableInput{Key: "SIGNING_KEY", Value: "k2", Secret: true}); err != nil {
		t.Fatal(err)
	}
	changes, _ := s.StagedChanges(ctx, id, "web")
	if len(changes) != 1 || !strings.Contains(changes[0].Note, "shared.SIGNING_KEY") {
		t.Errorf("after the shared variable changed: %+v", changes)
	}
}

// A rollback used to delete a reference variable the release it went back
// to ran with: it is delivered with the secrets, so it was not in the
// release's plain environment, and anything plain and absent was removed.
func TestARollbackKeepsTheReferencesItsReleaseUsed(t *testing.T) {
	ctx := context.Background()
	s, _, id := referenceService(t, "svc-rollback-reference")
	for _, name := range []string{"db", "web"} {
		if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
			Name: name, Image: "nginx:alpine", Replicas: 1, Port: 8080,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "DB", Value: "${{db.PRIVATE_HOST}}"}); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	web, _ := s.Get(ctx, id, "web")
	withRef := *web.ActiveReleaseID
	if err := s.SetVariable(ctx, id, "web", VariableInput{Key: "MODE", Value: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := executeNamedOperation(s, ctx, id, "web"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollback(ctx, id, "web", withRef); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if v, err := s.RevealVariable(ctx, id, "web", "DB"); err != nil || v != "${{db.PRIVATE_HOST}}" {
		t.Errorf("DB after the rollback = %q, %v", v, err)
	}
}

// A pre-deploy command saved and not yet deployed is a staged change, like
// a port or a limit. On the live install the bar did not count one.
func TestADeploySettingIsAStagedChange(t *testing.T) {
	ctx := context.Background()
	s, _, id := stagingService(t, "svc-staged-commands", Options{})
	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, id, "web", UpdateInput{
		Image: a.Image, Port: a.Port, PreDeployCommand: "bin/migrate",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	changes, err := s.StagedChanges(ctx, id, "web")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range changes {
		found = found || (c.Key == "Pre-deploy command" && c.To == "bin/migrate")
	}
	if !found {
		t.Errorf("staged = %+v, want the pre-deploy command", changes)
	}
}
