package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

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

// ReferenceTargets lists every other app of the owner's and what each can be
// referred to by: its variables, secret or not, and where it is reachable.
// Names only — a value never leaves its app to be offered.
func (s *Service) ReferenceTargets(ctx context.Context, ownerID, appName string) ([]ReferenceTarget, error) {
	apps, err := s.q.ListApps(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("app: list apps for references: %w", err)
	}
	var out []ReferenceTarget
	for _, a := range apps {
		if a.Name == appName {
			continue
		}
		vars, err := s.variablesFor(ctx, s.q, a.ID)
		if err != nil {
			return nil, err
		}
		t := ReferenceTarget{App: a.Name}
		for _, v := range vars {
			t.Keys = append(t.Keys, ReferenceKey{Key: v.Key, Secret: v.Secret || v.Reference})
		}
		t.Keys = append(t.Keys, ReferenceKey{Key: RefPrivateHost}, ReferenceKey{Key: RefPrivatePort})
		out = append(out, t)
	}
	return out, nil
}

// checkReferences refuses a value that names an app or key that does not
// exist, at the moment it is typed, rather than at the deploy that would
// otherwise fail on it.
func (s *Service) checkReferences(ctx context.Context, ownerID string, self App, value string) error {
	for _, r := range References(value) {
		if r.App == self.Name {
			return fmt.Errorf("%s refers to this app itself; use the variable's value instead", r)
		}
		target, err := s.q.GetApp(ctx, dbgen.GetAppParams{OwnerID: ownerID, Name: r.App})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s refers to %s, and there is no app called that", r, r.App)
		}
		if err != nil {
			return fmt.Errorf("app: read referenced app: %w", err)
		}
		if r.Key == RefPrivateHost || r.Key == RefPrivatePort {
			continue
		}
		if _, err := s.q.GetVariable(ctx, dbgen.GetVariableParams{
			OwnerID: ownerID, AppID: target.ID, Key: r.Key,
		}); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s refers to %s, which %s does not have", r, r.Key, r.App)
		} else if err != nil {
			return fmt.Errorf("app: read referenced variable: %w", err)
		}
	}
	return nil
}

// resolveReferences replaces each reference in value with what it names.
//
// Read at the moment of the deploy, from the referenced app's current
// variables, secrets opened: the result goes to the cluster as a secret,
// never into a release, so a database password a reference reaches is not
// copied into the plain history of every app that uses it.
func (s *Service) resolveReferences(
	ctx context.Context, q *dbgen.Queries, ownerID, key, value string, depth int,
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
		resolved, err := s.referenceValue(ctx, q, ownerID, r, depth)
		if err != nil {
			failed = fmt.Errorf("app: %s: %w", key, err)
			return m
		}
		return resolved
	})
	return out, failed
}

func (s *Service) referenceValue(
	ctx context.Context, q *dbgen.Queries, ownerID string, r Reference, depth int,
) (string, error) {
	target, err := q.GetApp(ctx, dbgen.GetAppParams{OwnerID: ownerID, Name: r.App})
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
		switch r.Key {
		case RefPrivateHost:
			return target.Name + "." + target.Namespace + ".svc.cluster.local", nil
		case RefPrivatePort:
			return strconv.Itoa(int(orchestrator.ServicePort(target.Internal, target.Port))), nil
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
		return s.resolveReferences(ctx, q, ownerID, r.App+"."+r.Key, value, depth+1)
	}
	return value, nil
}
