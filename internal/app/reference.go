package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// A reference is ${{app.KEY}} inside a variable's value: another app's
// variable, read when this one deploys. The value stays the reference, so
// the connection string is typed once, on the app that owns it, and every
// app that uses it follows when it changes and is redeployed.
//
// The spelling is Railway's, because it is the one people arriving here
// already know.
var referenceRE = regexp.MustCompile(`\$\{\{\s*([a-z0-9](?:[-a-z0-9]*[a-z0-9])?)\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// Keys every app answers a reference with, besides its own variables: where
// it is reachable inside the cluster. Named so they do not collide with the
// PORT and HOST an app often sets for itself, which are where it listens
// rather than where others reach it.
const (
	RefPrivateHost = "PRIVATE_HOST"
	RefPrivatePort = "PRIVATE_PORT"
)

// SharedScope is the name a reference uses for its project's shared
// variables: ${{shared.KEY}}. No app may be called it — CreateInput.Validate
// refuses the name — so the spelling can never mean another app's variable.
const SharedScope = "shared"

// referenceDepth bounds a reference to a value that is itself a reference.
// Two is the honest case — an API's URL built from a database's — and a
// chain deeper than this is a cycle or a mistake either way.
const referenceDepth = 4

// Reference is one ${{app.KEY}}.
type Reference struct {
	App string
	Key string
}

func (r Reference) String() string { return "${{" + r.App + "." + r.Key + "}}" }

// References lists the references in a value, in order, each once.
func References(value string) []Reference {
	var out []Reference
	for _, m := range referenceRE.FindAllStringSubmatch(value, -1) {
		r := Reference{App: m[1], Key: m[2]}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// HasReference reports whether a value refers to another app.
func HasReference(value string) bool { return referenceRE.MatchString(value) }

// ReferenceTarget is what a reference can name, as the picker offers it.
type ReferenceTarget struct {
	App  string
	Keys []ReferenceKey
}

// ReferenceKey is one variable another app can be pointed at.
type ReferenceKey struct {
	Key    string
	Secret bool
}

// ReferenceTargets lists what a variable of appName's can be pointed at:
// its project's shared variables first, then every other app of the owner's
// with its variables, secret or not, the platform's, and where it is
// reachable. Names only — a value never leaves its app to be offered.
//
// An edit still waiting for a deploy is offered as though it were deployed:
// a variable just added to the database is the one somebody is about to
// point the API at, and deploying everything ships both together.
func (s *Service) ReferenceTargets(ctx context.Context, ownerID, appName string) ([]ReferenceTarget, error) {
	apps, err := s.q.ListApps(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("app: list apps for references: %w", err)
	}
	var out []ReferenceTarget
	for _, row := range apps {
		if row.Name != appName {
			continue
		}
		shared, err := s.projectVariables(ctx, ownerID, toApp(row).ProjectID)
		if err != nil && !errors.Is(err, ErrProjectNotFound) {
			return nil, err
		}
		if len(shared) > 0 {
			t := ReferenceTarget{App: SharedScope}
			for _, v := range shared {
				t.Keys = append(t.Keys, ReferenceKey{Key: v.Key, Secret: v.Secret || v.Reference})
			}
			out = append(out, t)
		}
	}
	var self App
	for _, row := range apps {
		if row.Name == appName {
			self = toApp(row)
		}
	}
	for _, row := range apps {
		a := toApp(row)
		if a.Name == appName {
			continue
		}
		// Only this environment's services from this app's own project:
		// staging's api reaches staging's db, and the picker offers no other.
		// Another project's apps are offered by name, as before environments.
		label := a.Name
		if self.ProjectID != uuid.Nil && a.ProjectID == self.ProjectID {
			if a.Environment() != self.Environment() {
				continue
			}
			label = a.ServiceName()
		}
		vars, err := s.desiredVariablesFor(ctx, s.q, ownerID, a.ID)
		if err != nil {
			return nil, err
		}
		t := ReferenceTarget{App: label}
		for _, v := range vars {
			t.Keys = append(t.Keys, ReferenceKey{Key: v.Key, Secret: v.Secret || v.Reference})
		}
		t.Keys = append(t.Keys, ReferenceKey{Key: RefPrivateHost}, ReferenceKey{Key: RefPrivatePort})
		for _, n := range platformNames {
			t.Keys = append(t.Keys, ReferenceKey{Key: s.opts.PlatformVariablePrefix + n})
		}
		out = append(out, t)
	}
	return out, nil
}

// checkReferences refuses a value that names an app or key that does not
// exist, at the moment it is typed, rather than at the deploy that would
// otherwise fail on it. A key staged on the other app counts: see
// ReferenceTargets.
func (s *Service) checkReferences(ctx context.Context, ownerID string, self App, value string) error {
	for _, r := range References(value) {
		if r.App == SharedScope {
			if err := s.checkSharedReference(ctx, ownerID, self.ProjectID, r); err != nil {
				return err
			}
			continue
		}
		if r.App == self.Name || r.App == self.ServiceName() {
			return fmt.Errorf("%s refers to this app itself; use the variable's value instead", r)
		}
		target, err := s.referencedApp(ctx, s.q, ownerID, scopeOf(self), r.App)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s refers to %s, and there is no app called that", r, r.App)
		}
		if err != nil {
			return fmt.Errorf("app: read referenced app: %w", err)
		}
		if r.Key == RefPrivateHost || r.Key == RefPrivatePort || s.isPlatformKey(r.Key) {
			continue
		}
		has, err := s.hasDesiredVariable(ctx, ownerID, target.ID, r.Key)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("%s refers to %s, which %s does not have", r, r.Key, r.App)
		}
	}
	return nil
}

// checkSharedReference refuses ${{shared.KEY}} for a key the app's project
// does not share.
func (s *Service) checkSharedReference(
	ctx context.Context, ownerID string, projectID uuid.UUID, r Reference,
) error {
	project, err := s.projectFor(ctx, s.q, ownerID, projectID)
	if err != nil {
		return fmt.Errorf("app: read the project for %s: %w", r, err)
	}
	if _, err := s.q.GetProjectVariable(ctx, dbgen.GetProjectVariableParams{
		OwnerID: ownerID, ProjectID: project.ID, Key: r.Key,
	}); errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s refers to %s, which %s does not share — add it under Project Settings › Shared Variables",
			r, r.Key, project.Name)
	} else if err != nil {
		return fmt.Errorf("app: read shared variable: %w", err)
	}
	return nil
}

// hasDesiredVariable reports whether an app has key, deployed or staged and
// not staged for removal.
func (s *Service) hasDesiredVariable(ctx context.Context, ownerID string, appID uuid.UUID, key string) (bool, error) {
	staged, err := s.q.GetStagedVariable(ctx, dbgen.GetStagedVariableParams{
		OwnerID: ownerID, AppID: appID, Key: key,
	})
	if err == nil {
		return !staged.Removed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("app: read staged variable: %w", err)
	}
	if _, err := s.q.GetVariable(ctx, dbgen.GetVariableParams{
		OwnerID: ownerID, AppID: appID, Key: key,
	}); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("app: read referenced variable: %w", err)
	}
	return true, nil
}

// resolveReferences replaces each reference in value with what it names.
//
// Read at the moment of the deploy, from the referenced app's current
// variables, secrets opened: the result goes to the cluster as a secret,
// never into a release, so a database password a reference reaches is not
// copied into the plain history of every app that uses it. What another app
// has staged is not read — it is not that app's value until it deploys.
//
// projectID is the project whose shared variables ${{shared.KEY}} means:
// the app the value belongs to, which changes as a chain of references
// passes from one app to another.
func (s *Service) resolveReferences(
	ctx context.Context, q *dbgen.Queries, ownerID string, scope refScope,
	key, value string, depth int,
) (string, error) {
	if depth > referenceDepth {
		return "", fmt.Errorf("app: %s refers through more than %d variables; is there a loop?", key, referenceDepth)
	}
	var failed error
	out := referenceRE.ReplaceAllStringFunc(value, func(m string) string {
		if failed != nil {
			return m
		}
		sub := referenceRE.FindStringSubmatch(m)
		r := Reference{App: sub[1], Key: sub[2]}
		resolved, err := s.referenceValue(ctx, q, ownerID, scope, r, depth)
		if err != nil {
			failed = fmt.Errorf("app: %s: %w", key, err)
			return m
		}
		return resolved
	})
	return out, failed
}

func (s *Service) referenceValue(
	ctx context.Context, q *dbgen.Queries, ownerID string, scope refScope, r Reference, depth int,
) (string, error) {
	if r.App == SharedScope {
		return s.sharedReferenceValue(ctx, q, ownerID, scope, r, depth)
	}
	target, err := s.referencedApp(ctx, q, ownerID, scope, r.App)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%s refers to %s, which no longer exists", r, r.App)
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", r.App, err)
	}
	row, err := q.GetVariable(ctx, dbgen.GetVariableParams{OwnerID: ownerID, AppID: target.ID, Key: r.Key})
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		switch {
		case r.Key == RefPrivateHost:
			return target.Name + "." + target.Namespace + ".svc.cluster.local", nil
		case r.Key == RefPrivatePort:
			return strconv.Itoa(int(orchestrator.ServicePort(target.Internal, target.Port))), nil
		case s.isPlatformKey(r.Key):
			return s.platformReferenceValue(ctx, q, toApp(target), r)
		}
		if staged, stagedErr := q.GetStagedVariable(ctx, dbgen.GetStagedVariableParams{
			OwnerID: ownerID, AppID: target.ID, Key: r.Key,
		}); stagedErr == nil && !staged.Removed {
			return "", fmt.Errorf("%s refers to %s, which %s has not deployed yet — deploy %s first, or deploy every change together",
				r, r.Key, r.App, r.App)
		}
		return "", fmt.Errorf("%s refers to %s, which %s no longer has", r, r.Key, r.App)
	default:
		return "", fmt.Errorf("read %s: %w", r, err)
	}
	value := row.Value
	if row.Secret {
		if value, err = s.keeper.Open(row.Sealed); err != nil {
			return "", fmt.Errorf("opening %s: %w", r, err)
		}
	}
	if HasReference(value) {
		return s.resolveReferences(ctx, q, ownerID, scopeOf(toApp(target)), r.App+"."+r.Key, value, depth+1)
	}
	return value, nil
}

// platformReferenceValue is another app's platform variable, as its own
// deploy would set it. Its public domain comes from the hostnames it is
// routed on now, which is what the app itself was last told only if it has
// deployed since — the same caveat as any reference.
func (s *Service) platformReferenceValue(
	ctx context.Context, q *dbgen.Queries, target App, r Reference,
) (string, error) {
	var hosts []string
	if !target.Internal && target.Port != 0 {
		var err error
		if hosts, err = domain.RoutableHosts(ctx, q, target.ID); err != nil {
			return "", fmt.Errorf("read %s's hostnames: %w", target.Name, err)
		}
	}
	value, ok := s.platformEnv(ctx, q, target, hosts)[r.Key]
	if !ok {
		return "", fmt.Errorf("%s refers to %s, which %s does not have", r, r.Key, target.Name)
	}
	return value, nil
}

// refScope is where a reference is read from: the project whose shared
// variables ${{shared.KEY}} means, and the environment whose services
// ${{db.KEY}} means — so a reference written in staging reaches staging's db,
// not production's.
type refScope struct {
	Project uuid.UUID
	Env     string
}

func scopeOf(a App) refScope { return refScope{Project: a.ProjectID, Env: a.Environment()} }

// referencedApp is the app a reference names: the service by that name in
// the referring app's environment, or else the app by that name — which is
// how every reference written before environments reads, and how one to
// another project's app does.
func (s *Service) referencedApp(
	ctx context.Context, q *dbgen.Queries, ownerID string, scope refScope, name string,
) (dbgen.App, error) {
	if scope.Project != uuid.Nil {
		row, err := q.GetAppByService(ctx, dbgen.GetAppByServiceParams{
			OwnerID: ownerID, ProjectID: pgUUID(scope.Project),
			Environment: scope.Env, Service: name,
		})
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return dbgen.App{}, fmt.Errorf("app: read referenced service: %w", err)
		}
	}
	return q.GetApp(ctx, dbgen.GetAppParams{OwnerID: ownerID, Name: name})
}
