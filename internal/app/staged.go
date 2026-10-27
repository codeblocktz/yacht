package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Staged changes are Railway's: a person edits an app's variables, or a
// setting that rolls out, and nothing deploys. The edits collect, the canvas
// says "N changes", and Deploy ships them as one deployment — or Discard
// throws them away.
//
// What is staged is not a separate copy of the app. Settings are saved on the
// app row as they always were, and the release the app is running still has
// the old ones; the difference between the two is the change. Variables are
// different, because a deploy is not the only thing that reads them: every
// apply opens the current secrets and resolves the current references, and
// the reconciler applies whenever anything about the app moves. An edited
// secret saved in place would be in the cluster within seconds, staged or
// not. So a variable edit waits in staged_variables, which nothing on the
// path to the cluster reads, and moves into variables in the same statement
// that a release is made from them (createRelease). Any deploy carries
// everything saved — the bar's, a redeploy, a push — because a release is
// always made from the app as it is now.

// StagedRemoved says a change takes something away; StagedAdded and
// StagedChanged are in variable.go, because a listed variable carries them.
const StagedRemoved = "removed"

// ErrNothingStaged means a deploy of staged changes was asked for and there
// are none.
var ErrNothingStaged = errors.New("app: nothing is waiting to be deployed")

// StagedChange is one difference between what an app is set to and what it
// is running, as a person reads it before deploying.
type StagedChange struct {
	// App is the app the change is to.
	App string

	// Key is the variable, or for a setting its name: "Port", "CPU limit".
	Key     string
	Setting bool

	// Kind is StagedAdded, StagedChanged or StagedRemoved.
	Kind string

	// From and To are a setting's values, as a person would write them. A
	// variable's are never carried: this list is drawn on a canvas anyone in
	// the team can open, and a changed password is listed by name.
	From, To string

	// Note is anything a person needs before choosing: where a change came
	// from, or why Discard will not undo it.
	Note string

	// Kept means Discard leaves this change as it is. Note says why.
	Kept bool
}

// stagedVariable is one variable of an app's as staging sees it: what is
// deployed, and the edit waiting on top of it.
type stagedVariable struct {
	Key     string
	Applied *dbgen.Variable
	Staged  *dbgen.StagedVariable
}

// desired reports whether the variable exists once its edit is deployed.
func (v stagedVariable) desired() bool {
	if v.Staged != nil {
		return !v.Staged.Removed
	}
	return v.Applied != nil
}

// secretish is whether the variable, once deployed, reaches the cluster with
// the secrets rather than in the release: a secret, or a reference.
func (v stagedVariable) secretish() bool {
	if v.Staged != nil {
		return v.Staged.Secret || HasReference(v.Staged.Value)
	}
	return v.Applied.Secret || HasReference(v.Applied.Value)
}

// value is the variable's readable value once deployed, empty for a secret.
func (v stagedVariable) value() string {
	if v.Staged != nil {
		return v.Staged.Value
	}
	return v.Applied.Value
}

func (v stagedVariable) secret() bool {
	if v.Staged != nil {
		return v.Staged.Secret
	}
	return v.Applied.Secret
}

// stagedVariablesFor reads an app's deployed variables with its staged edits
// beside them, in key order.
func (s *Service) stagedVariablesFor(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID,
) ([]stagedVariable, error) {
	applied, err := q.ListVariablesForApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("app: list variables: %w", err)
	}
	staged, err := q.ListStagedVariablesForApp(ctx, dbgen.ListStagedVariablesForAppParams{
		OwnerID: ownerID, AppID: appID,
	})
	if err != nil {
		return nil, fmt.Errorf("app: list staged variables: %w", err)
	}
	byKey := make(map[string]*stagedVariable, len(applied)+len(staged))
	for i := range applied {
		byKey[applied[i].Key] = &stagedVariable{Key: applied[i].Key, Applied: &applied[i]}
	}
	for i := range staged {
		v, ok := byKey[staged[i].Key]
		if !ok {
			v = &stagedVariable{Key: staged[i].Key}
			byKey[staged[i].Key] = v
		}
		v.Staged = &staged[i]
	}
	out := make([]stagedVariable, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// desiredVariablesFor is an app's variables as they will be once its staged
// edits deploy, each marked with how it differs. This is what a page lists:
// what somebody typed is what they expect to see, deployed or not.
func (s *Service) desiredVariablesFor(
	ctx context.Context, q *dbgen.Queries, ownerID string, appID uuid.UUID,
) ([]Variable, error) {
	vars, err := s.stagedVariablesFor(ctx, q, ownerID, appID)
	if err != nil {
		return nil, err
	}
	out := make([]Variable, 0, len(vars))
	for _, v := range vars {
		if !v.desired() {
			continue
		}
		listed := Variable{Key: v.Key, Secret: v.secret()}
		if !listed.Secret {
			listed.Value = v.value()
			listed.Reference = HasReference(listed.Value)
		}
		switch {
		case v.Staged != nil && v.Applied == nil:
			listed.Staged = StagedAdded
		case v.Staged != nil:
			listed.Staged = StagedChanged
		}
		out = append(out, listed)
	}
	return out, nil
}

// stageVariable saves a variable edit for the next deploy.
func (s *Service) stageVariable(ctx context.Context, ownerID, appName string, in VariableInput) error {
	in.Key = strings.TrimSpace(in.Key)
	if err := in.Validate(); err != nil {
		return err
	}
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}
	if err := s.checkReferences(ctx, ownerID, a, in.Value); err != nil {
		return err
	}
	return s.stageVariableQ(ctx, s.q, a, in)
}

// stageVariableQ writes one staged edit on the caller's handle. The input is
// validated and its references checked already.
func (s *Service) stageVariableQ(ctx context.Context, q *dbgen.Queries, a App, in VariableInput) error {
	if in.Secret && !s.keeper.Configured() {
		return ErrNoSecretKey
	}
	applied, err := q.GetVariable(ctx, dbgen.GetVariableParams{
		OwnerID: a.OwnerID, AppID: a.ID, Key: in.Key,
	})
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("app: read variable: %w", err)
	}

	// Typed back to what is deployed is no change at all, and the bar should
	// stop counting it rather than offer to deploy nothing.
	if exists {
		same, err := s.sameVariable(applied, in)
		if err != nil {
			return err
		}
		if same {
			if _, err := q.DeleteStagedVariable(ctx, dbgen.DeleteStagedVariableParams{
				OwnerID: a.OwnerID, AppID: a.ID, Key: in.Key,
			}); err != nil {
				return fmt.Errorf("app: clear staged variable: %w", err)
			}
			return s.recordLinks(ctx, q, a.OwnerID, a, in.Key, in.Value)
		}
	}

	params := dbgen.UpsertStagedVariableParams{
		OwnerID: a.OwnerID, AppID: a.ID, Key: in.Key, Secret: in.Secret,
	}
	if in.Secret {
		if params.Sealed, err = s.keeper.Seal(in.Value); err != nil {
			return fmt.Errorf("app: seal %s: %w", in.Key, err)
		}
	} else {
		params.Value = in.Value
	}
	if _, err := q.UpsertStagedVariable(ctx, params); err != nil {
		return fmt.Errorf("app: stage variable: %w", err)
	}
	// The canvas draws what the app is set to, so the edge follows the edit
	// now rather than at the deploy; Discard puts it back.
	if err := s.recordLinks(ctx, q, a.OwnerID, a, in.Key, in.Value); err != nil {
		return err
	}
	s.log.Info("variable staged",
		slog.String("app", a.Name), slog.String("key", in.Key),
		slog.Bool("secret", in.Secret))
	return nil
}

// sameVariable reports whether an input says what a deployed row already
// does. A secret has to be opened to know.
func (s *Service) sameVariable(row dbgen.Variable, in VariableInput) (bool, error) {
	if row.Secret != in.Secret {
		return false, nil
	}
	if !row.Secret {
		return row.Value == in.Value, nil
	}
	value, err := s.keeper.Open(row.Sealed)
	if err != nil {
		return false, fmt.Errorf("app: opening %s: %w", in.Key, err)
	}
	return value == in.Value, nil
}

// stageRemoval stages taking a variable away. One that exists only as a
// staged addition simply stops existing: there is nothing deployed to
// remove.
func (s *Service) stageRemoval(ctx context.Context, q *dbgen.Queries, a App, key string) error {
	_, err := q.GetVariable(ctx, dbgen.GetVariableParams{OwnerID: a.OwnerID, AppID: a.ID, Key: key})
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("app: read variable: %w", err)
	}
	staged, err := q.GetStagedVariable(ctx, dbgen.GetStagedVariableParams{
		OwnerID: a.OwnerID, AppID: a.ID, Key: key,
	})
	isStaged := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("app: read staged variable: %w", err)
	}
	switch {
	case isStaged && staged.Removed, !exists && !isStaged:
		return ErrVariableNotFound
	case !exists:
		if _, err := q.DeleteStagedVariable(ctx, dbgen.DeleteStagedVariableParams{
			OwnerID: a.OwnerID, AppID: a.ID, Key: key,
		}); err != nil {
			return fmt.Errorf("app: clear staged variable: %w", err)
		}
	default:
		if _, err := q.UpsertStagedVariable(ctx, dbgen.UpsertStagedVariableParams{
			OwnerID: a.OwnerID, AppID: a.ID, Key: key, Removed: true,
		}); err != nil {
			return fmt.Errorf("app: stage removal: %w", err)
		}
	}
	if err := q.ReplaceAppLinks(ctx, dbgen.ReplaceAppLinksParams{
		FromAppID: a.ID, ViaKey: key,
	}); err != nil {
		return fmt.Errorf("app: clear links: %w", err)
	}
	s.log.Info("variable removal staged", slog.String("app", a.Name), slog.String("key", key))
	return nil
}

// commitStaged moves an app's staged variable edits into the variables a
// deploy reads, and brings a's config version along with them.
func (s *Service) commitStaged(ctx context.Context, q *dbgen.Queries, a *App) error {
	rows, err := q.CommitStagedVariables(ctx, dbgen.CommitStagedVariablesParams{
		OwnerID: a.OwnerID, AppID: a.ID,
	})
	if err != nil {
		return fmt.Errorf("app: commit staged variables: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	a.ConfigVersion = rows[0].ConfigVersion
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	s.log.Info("staged variables committed",
		slog.String("app", a.Name), slog.String("keys", strings.Join(keys, ",")))
	return nil
}

// StagedChanges lists what an app would change by deploying: added, changed
// and removed variables by name, and the settings that roll out, against the
// release it runs — or the one a deploy in flight is about to run, so a
// change somebody has just deployed stops being listed as waiting.
func (s *Service) StagedChanges(ctx context.Context, ownerID, appName string) ([]StagedChange, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return nil, err
	}
	return s.stagedChanges(ctx, a)
}

func (s *Service) stagedChanges(ctx context.Context, a App) ([]StagedChange, error) {
	vars, err := s.stagedVariablesFor(ctx, s.q, a.OwnerID, a.ID)
	if err != nil {
		return nil, err
	}
	base, ok, err := s.stagedBase(ctx, a)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Never deployed, so there is nothing to compare settings with; the
		// edits waiting on its variables are still worth saying.
		var out []StagedChange
		for _, v := range vars {
			if v.Staged == nil {
				continue
			}
			kind := StagedChanged
			switch {
			case v.Applied == nil:
				kind = StagedAdded
			case v.Staged.Removed:
				kind = StagedRemoved
			}
			out = append(out, StagedChange{App: a.Name, Key: v.Key, Kind: kind})
		}
		return out, nil
	}
	anchor, err := s.liveSince(ctx, a, base)
	if err != nil {
		return nil, err
	}

	var out []StagedChange
	inRelease := map[string]bool{}
	secretKeys := map[string]bool{}
	for _, k := range base.SecretKeys {
		secretKeys[k], inRelease[k] = true, true
	}
	for k := range base.Env {
		inRelease[k] = true
	}
	desired := map[string]bool{}
	for _, v := range vars {
		if !v.desired() {
			continue
		}
		desired[v.Key] = true
		change := StagedChange{App: a.Name, Key: v.Key, Kind: StagedChanged}
		plain, inEnv := base.Env[v.Key]
		switch {
		case !inRelease[v.Key]:
			change.Kind = StagedAdded
		case inEnv && v.secretish():
			change.Note = "Now " + secretishWord(v) + "."
		case inEnv:
			if v.value() == plain {
				continue
			}
		case !v.secretish():
			change.Note = "Now a plain value."
		case v.Staged != nil:
		case v.Applied.UpdatedAt.After(anchor):
			// Saved into the deployed variables without a deploy that ran
			// with it: a deploy that failed, say. A plain value is in the
			// release and Discard restores it; a secret's earlier value was
			// never kept anywhere, so it stays.
			change.Kept = true
			if v.Applied.Secret {
				change.Note = "Saved since the last deploy. Discard keeps this value: a secret's earlier value is not kept."
			} else {
				change.Note = "Saved since the last deploy. Discard keeps this reference: the one it replaced is not kept."
			}
		default:
			stale, err := s.staleReference(ctx, a, v, anchor)
			if err != nil {
				return nil, err
			}
			if stale == "" {
				continue
			}
			change.Kept = true
			change.Note = stale + " has changed since this app last deployed."
		}
		out = append(out, change)
	}
	var removed []string
	for k := range inRelease {
		if !desired[k] {
			removed = append(removed, k)
		}
	}
	sort.Strings(removed)
	for _, k := range removed {
		out = append(out, StagedChange{App: a.Name, Key: k, Kind: StagedRemoved})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return append(out, settingChanges(a, base)...), nil
}

func secretishWord(v stagedVariable) string {
	if v.secret() {
		return "a secret"
	}
	return "a reference"
}

// staleReference names the first thing a reference variable reads that has
// changed since the app last deployed, empty when nothing has. The app runs
// what the reference resolved to then, and only a deploy reads it again.
func (s *Service) staleReference(ctx context.Context, a App, v stagedVariable, anchor time.Time) (string, error) {
	if v.secret() {
		return "", nil
	}
	for _, r := range References(v.value()) {
		var updated time.Time
		if r.App == SharedScope {
			project, err := s.projectFor(ctx, s.q, a.OwnerID, a.ProjectID)
			if err != nil {
				continue
			}
			row, err := s.q.GetProjectVariable(ctx, dbgen.GetProjectVariableParams{
				OwnerID: a.OwnerID, ProjectID: project.ID, Key: r.Key,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("app: read shared variable: %w", err)
			}
			updated = row.UpdatedAt
		} else {
			target, err := s.q.GetApp(ctx, dbgen.GetAppParams{OwnerID: a.OwnerID, Name: r.App})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("app: read referenced app: %w", err)
			}
			row, err := s.q.GetVariable(ctx, dbgen.GetVariableParams{
				OwnerID: a.OwnerID, AppID: target.ID, Key: r.Key,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("app: read referenced variable: %w", err)
			}
			updated = row.UpdatedAt
		}
		if updated.After(anchor) {
			return r.App + "." + r.Key, nil
		}
	}
	return "", nil
}

// stagedBase is the release staged changes are measured against: the one a
// deploy in flight is about to run, if it has one, and otherwise the one
// running. False when the app has neither.
func (s *Service) stagedBase(ctx context.Context, a App) (Release, bool, error) {
	op, err := s.liveOperation(ctx, a)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Release{}, false, err
	}
	id := a.ActiveReleaseID
	if err == nil && op.ReleaseID != nil {
		id = op.ReleaseID
	}
	if id == nil {
		return Release{}, false, nil
	}
	r, err := s.ReleaseByID(ctx, a.OwnerID, a.ID, *id)
	if err != nil {
		return Release{}, false, err
	}
	return r, true, nil
}

// liveSince is when a release's secrets and references were last resolved:
// its newest successful deployment, or when it was made, whichever is later.
func (s *Service) liveSince(ctx context.Context, a App, r Release) (time.Time, error) {
	at := r.CreatedAt
	started, err := s.q.GetReleaseLiveSince(ctx, dbgen.GetReleaseLiveSinceParams{
		OwnerID: a.OwnerID, AppID: a.ID, ReleaseID: pgUUID(r.ID),
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("app: read when the release went live: %w", err)
	}
	if err == nil && started.After(at) {
		at = started
	}
	return at, nil
}

// settingChanges lists the release-owned settings an app is set to and is
// not running.
func settingChanges(a App, r Release) []StagedChange {
	var out []StagedChange
	add := func(name, from, to string) {
		if from != to {
			out = append(out, StagedChange{
				App: a.Name, Key: name, Setting: true, Kind: StagedChanged, From: from, To: to,
			})
		}
	}
	if a.Image != PendingImage {
		add("Image", r.ImageRef, a.Image)
	}
	add("Port", portWord(r.Port), portWord(a.Port))
	add("Networking", reachWord(r.Internal), reachWord(a.Internal))
	add("Replicas", strconv.Itoa(int(r.Replicas)), strconv.Itoa(int(a.Replicas)))
	add("CPU request", orDefault(r.CPURequest), orDefault(a.CPURequest))
	add("CPU limit", orDefault(r.CPULimit), orDefault(a.CPULimit))
	add("Memory request", orDefault(r.MemoryRequest), orDefault(a.MemoryRequest))
	add("Memory limit", orDefault(r.MemoryLimit), orDefault(a.MemoryLimit))
	add("Health check", orNone(r.HealthPath), orNone(a.HealthPath))
	if a.HealthPath != "" || r.HealthPath != "" {
		add("Restart on failed check", onOff(r.Liveness), onOff(a.Liveness))
	}
	// The Deploy section: each is how the release runs, so each is a change
	// that waits for Deploy like the rest. Found missing on the live install,
	// where a pre-deploy command was saved and the bar did not count it.
	add("Pre-deploy command", orNone(r.PreDeployCommand), orNone(a.PreDeployCommand))
	add("Start command", orNone(r.StartCommand), orNone(a.StartCommand))
	add("Cron schedule", orNone(r.CronSchedule), orNone(a.CronSchedule))
	add("Restart policy", orDefault(string(r.RestartPolicy)), orDefault(string(a.RestartPolicy)))
	add("Max restart retries", strconv.Itoa(int(r.RestartMaxRetries)), strconv.Itoa(int(a.RestartMaxRetries)))
	add("Healthcheck timeout", secondsWord(r.HealthTimeoutSeconds), secondsWord(a.HealthTimeoutSeconds))
	return out
}

func secondsWord(n int32) string {
	if n == 0 {
		return "default"
	}
	return strconv.Itoa(int(n)) + "s"
}

func portWord(p int32) string {
	if p == 0 {
		return "none"
	}
	return strconv.Itoa(int(p))
}

func reachWord(internal bool) string {
	if internal {
		return "private"
	}
	return "public"
}

func orDefault(v string) string {
	if v == "" {
		return "default"
	}
	return v
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// DeployStaged ships everything an app has waiting as one deployment.
//
// The staged variables move into the deployed ones, the release is made from
// them and the app's settings, and the deployment is admitted, all in one
// transaction: a deploy already running refuses admission and the edits stay
// staged, rather than being half-applied behind a deploy that never ran.
func (s *Service) DeployStaged(ctx context.Context, ownerID, appName string) error {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}
	changes, err := s.stagedChanges(ctx, a)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return ErrNothingStaged
	}
	if _, err := s.liveOperation(ctx, a); err == nil {
		return ErrOperationInFlight
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := s.checkStagedReferences(ctx, a); err != nil {
		return err
	}
	trigger := stagedTrigger(changes)

	// A bucket's edits are kept and nothing is deployed: its variables are
	// read by the apps that refer to them, at their own next deploy.
	if !a.Source.Workload() {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("app: begin staged changes: %w", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
		if err := s.commitStaged(ctx, s.q.WithTx(tx), &a); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// A Git app that has never built has no image to make a release from;
	// its first build makes one, and carries the staged edits with it.
	if a.Image == PendingImage {
		if a.Source != SourceGit {
			return fmt.Errorf("%w: %s has not produced an image yet", ErrNoManifestResolver, a.Name)
		}
		_, err := s.admitDeployment(ctx, ownerID, a, trigger, uuid.Nil, true)
		return err
	}
	if s.manifests == nil {
		return ErrNoManifestResolver
	}
	digest, err := s.manifests.ResolveDigest(ctx, a.Image)
	if err != nil {
		return fmt.Errorf("app: resolve %s for release: %w", a.Image, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin staged deploy: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	if err := s.commitStaged(ctx, q, &a); err != nil {
		return err
	}
	release, err := s.recordRelease(ctx, q, a, "", digest, "deployment")
	if err != nil {
		return err
	}
	if _, err := s.admitDeploymentTx(ctx, q, ownerID, a, trigger, release.ID, false, "user"); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit staged deploy: %w", err)
	}
	s.log.Info("staged changes deployed",
		slog.String("app", appName), slog.Int("changes", len(changes)))
	return nil
}

// checkStagedReferences refuses deploying an app whose variables refer to
// another app's variable that is itself only staged. It would resolve to
// nothing, and the deploy would fail on it in the worker; said here, the
// fix — deploy that app too — is in front of the person who can do it.
func (s *Service) checkStagedReferences(ctx context.Context, a App) error {
	vars, err := s.desiredVariablesFor(ctx, s.q, a.OwnerID, a.ID)
	if err != nil {
		return err
	}
	for _, v := range vars {
		for _, r := range v.Refs() {
			if r.App == SharedScope || r.Key == RefPrivateHost || r.Key == RefPrivatePort || s.isPlatformKey(r.Key) {
				continue
			}
			target, err := s.q.GetApp(ctx, dbgen.GetAppParams{OwnerID: a.OwnerID, Name: r.App})
			if err != nil {
				continue
			}
			if _, err := s.q.GetVariable(ctx, dbgen.GetVariableParams{
				OwnerID: a.OwnerID, AppID: target.ID, Key: r.Key,
			}); errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("app: %s refers to %s, which %s has not deployed yet — deploy %s first, or deploy every change together",
					v.Key, r, r.App, r.App)
			}
		}
	}
	return nil
}

// stagedTrigger is what the deployment's history says it was: the first few
// things it changed, by name.
func stagedTrigger(changes []StagedChange) string {
	const shown = 3
	var names []string
	for i, c := range changes {
		if i == shown {
			names = append(names, "+"+strconv.Itoa(len(changes)-shown))
			break
		}
		name := c.Key
		if c.Setting {
			name = strings.ToLower(strings.ReplaceAll(name, " ", "-"))
		}
		names = append(names, name)
	}
	return "changes:" + strings.Join(names, ",")
}

// DiscardStaged throws an app's waiting changes away and returns the names
// of those it could not undo.
//
// Staged variable edits are deleted outright: nothing deployed ever read
// them. Settings and plain variables go back to the release the app is
// running, which has them. A secret saved without a deploy — one whose
// deploy failed, say — is the exception: a release keeps a secret's name and
// never its value, so there is no earlier value to put back, and it stays
// as it is. Those are the names returned.
func (s *Service) DiscardStaged(ctx context.Context, ownerID, appName string) ([]string, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return nil, err
	}
	if _, err := s.liveOperation(ctx, a); err == nil {
		// What is staged is measured against the release being deployed,
		// and putting the app back to the one it is leaving would discard
		// the change in flight as well.
		return nil, ErrOperationInFlight
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	changes, err := s.stagedChanges(ctx, a)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: begin discard: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	keys, err := q.DiscardStagedVariables(ctx, dbgen.DiscardStagedVariablesParams{
		OwnerID: ownerID, AppID: a.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("app: discard staged variables: %w", err)
	}
	restored := a
	if a.ActiveReleaseID != nil {
		release, err := s.ReleaseByID(ctx, ownerID, a.ID, *a.ActiveReleaseID)
		if err != nil {
			return nil, err
		}
		if len(settingChanges(a, release)) > 0 {
			row, err := q.RestoreAppFromRelease(ctx, dbgen.RestoreAppFromReleaseParams{
				OwnerID: ownerID, AppID: a.ID, ReleaseID: release.ID,
			})
			if err != nil {
				return nil, fmt.Errorf("app: restore settings from release: %w", err)
			}
			restored = toApp(row)
		}
		if err := s.restoreVariables(ctx, q, ownerID, restored, release.Env, release.SecretKeys); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("app: commit discard: %w", err)
	}

	// Each discarded edit had moved its canvas edge; it goes back to where
	// the deployed value puts it.
	for _, key := range keys {
		if err := s.relinkDeployed(ctx, a, key); err != nil {
			s.log.Warn("could not restore a discarded variable's edge",
				slog.String("app", a.Name), slog.String("key", key), slog.String("error", err.Error()))
		}
	}
	if restored.Internal != a.Internal || (restored.Port == 0) != (a.Port == 0) {
		s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: ownerID, AppID: a.ID, App: a.Name})
	}

	var kept []string
	for _, c := range changes {
		if c.Kept {
			kept = append(kept, c.Key)
		}
	}
	s.log.Info("staged changes discarded",
		slog.String("app", appName), slog.Int("changes", len(changes)), slog.Int("kept", len(kept)))
	return kept, nil
}

// relinkDeployed records a variable's canvas edge from its deployed value.
func (s *Service) relinkDeployed(ctx context.Context, a App, key string) error {
	row, err := s.q.GetVariable(ctx, dbgen.GetVariableParams{OwnerID: a.OwnerID, AppID: a.ID, Key: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.recordLinks(ctx, s.q, a.OwnerID, a, key, "")
	}
	if err != nil {
		return err
	}
	value := row.Value
	if row.Secret {
		if value, err = s.keeper.Open(row.Sealed); err != nil {
			return err
		}
	}
	return s.recordLinks(ctx, s.q, a.OwnerID, a, key, value)
}

// ProjectStagedChanges lists every staged change across a project's apps,
// for the bar across the top of its canvas.
func (s *Service) ProjectStagedChanges(ctx context.Context, ownerID, projectSlug string) ([]StagedChange, error) {
	apps, err := s.projectApps(ctx, ownerID, projectSlug)
	if err != nil {
		return nil, err
	}
	var out []StagedChange
	for _, a := range apps {
		changes, err := s.stagedChanges(ctx, a)
		if err != nil {
			return nil, err
		}
		out = append(out, changes...)
	}
	return out, nil
}

func (s *Service) projectApps(ctx context.Context, ownerID, projectSlug string) ([]App, error) {
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListAppsInProject(ctx, dbgen.ListAppsInProjectParams{
		OwnerID: ownerID, ProjectID: pgUUID(p.ID),
	})
	if err != nil {
		return nil, fmt.Errorf("app: list the project's apps: %w", err)
	}
	// One environment's, when the request names one: the bar on staging's
	// canvas is staging's changes, and its Deploy deploys staging's alone.
	env := environmentFrom(ctx)
	out := make([]App, 0, len(rows))
	for _, row := range rows {
		a := toApp(row)
		if env != "" && a.Environment() != env {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// StagedFailure is one app a project-wide deploy or discard could not do,
// and why.
type StagedFailure struct {
	App string
	Err error
}

// StagedErrors is every app a project-wide deploy or discard could not do.
// The others went ahead: one app with a deploy already running is no reason
// to hold back the rest.
type StagedErrors []StagedFailure

func (e StagedErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, f := range e {
		parts = append(parts, f.App+": "+strings.TrimPrefix(f.Err.Error(), "app: "))
	}
	return "app: " + strings.Join(parts, "; ")
}

// DeployProjectStaged deploys every app in a project that has changes
// waiting, and returns the names of those it deployed.
//
// An app is deployed after the apps its variables refer to, and its changes
// are read only at its turn: a database's new password, deployed first, is
// what makes the API that refers to it one of the apps with a change.
func (s *Service) DeployProjectStaged(ctx context.Context, ownerID, projectSlug string) ([]string, error) {
	apps, err := s.projectApps(ctx, ownerID, projectSlug)
	if err != nil {
		return nil, err
	}
	ordered, err := s.referenceOrder(ctx, apps)
	if err != nil {
		return nil, err
	}
	var (
		deployed []string
		failed   StagedErrors
	)
	for _, a := range ordered {
		err := s.DeployStaged(ctx, ownerID, a.Name)
		switch {
		case err == nil:
			deployed = append(deployed, a.Name)
		case errors.Is(err, ErrNothingStaged):
		default:
			failed = append(failed, StagedFailure{App: a.Name, Err: err})
		}
	}
	if len(failed) > 0 {
		return deployed, failed
	}
	return deployed, nil
}

// DiscardProjectStaged discards every app's waiting changes in a project,
// returning the changes it could not undo as app.KEY.
func (s *Service) DiscardProjectStaged(ctx context.Context, ownerID, projectSlug string) ([]string, error) {
	apps, err := s.projectApps(ctx, ownerID, projectSlug)
	if err != nil {
		return nil, err
	}
	var (
		kept   []string
		failed StagedErrors
	)
	for _, a := range apps {
		changes, err := s.stagedChanges(ctx, a)
		if err != nil {
			return kept, err
		}
		if len(changes) == 0 {
			continue
		}
		keys, err := s.DiscardStaged(ctx, ownerID, a.Name)
		if err != nil {
			failed = append(failed, StagedFailure{App: a.Name, Err: err})
			continue
		}
		for _, k := range keys {
			kept = append(kept, a.Name+"."+k)
		}
	}
	if len(failed) > 0 {
		return kept, failed
	}
	return kept, nil
}

// referenceOrder sorts apps so each comes after the apps its variables,
// deployed or staged, refer to. A cycle is broken where it is found, as the
// canvas breaks one: two apps naming each other still both deploy.
func (s *Service) referenceOrder(ctx context.Context, apps []App) ([]App, error) {
	needs := make(map[string][]string, len(apps))
	byName := make(map[string]App, len(apps))
	for _, a := range apps {
		byName[a.Name] = a
		vars, err := s.desiredVariablesFor(ctx, s.q, a.OwnerID, a.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range vars {
			for _, r := range v.Refs() {
				needs[a.Name] = append(needs[a.Name], r.App)
			}
		}
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	var (
		out  []App
		done = map[string]bool{}
		seen = map[string]bool{}
	)
	var visit func(name string)
	visit = func(name string) {
		a, ok := byName[name]
		if !ok || done[name] || seen[name] {
			return
		}
		seen[name] = true
		for _, dep := range needs[name] {
			visit(dep)
		}
		done[name] = true
		out = append(out, a)
	}
	for _, a := range apps {
		visit(a.Name)
	}
	return out, nil
}

// RevealVariable opens one of an app's variables for the person asking: the
// value it will deploy with, staged or not, a secret opened by the keeper.
//
// The one read path a value has to a page. Callers log who asked — the
// engine cannot tell a person from a team — and never the value.
func (s *Service) RevealVariable(ctx context.Context, ownerID, appName, key string) (string, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return "", err
	}
	staged, err := s.q.GetStagedVariable(ctx, dbgen.GetStagedVariableParams{
		OwnerID: ownerID, AppID: a.ID, Key: key,
	})
	switch {
	case err == nil && staged.Removed:
		return "", ErrVariableNotFound
	case err == nil:
		return s.openValue(key, staged.Value, staged.Sealed, staged.Secret)
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("app: read staged variable: %w", err)
	}
	row, err := s.q.GetVariable(ctx, dbgen.GetVariableParams{OwnerID: ownerID, AppID: a.ID, Key: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrVariableNotFound
	}
	if err != nil {
		return "", fmt.Errorf("app: read variable: %w", err)
	}
	return s.openValue(key, row.Value, row.Sealed, row.Secret)
}

func (s *Service) openValue(key, value string, sealed []byte, secret bool) (string, error) {
	if !secret {
		return value, nil
	}
	plain, err := s.keeper.Open(sealed)
	if err != nil {
		return "", fmt.Errorf("app: opening %s: %w", key, err)
	}
	return plain, nil
}
