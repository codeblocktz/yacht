package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Shared variables are a project's: set once, under Project Settings, and
// used by any of its apps as ${{shared.KEY}}. Railway's, and for the same
// reason — the region every service in a project runs in, or the key they
// all sign with, should be typed in one place rather than kept in step by
// hand across several.
//
// They are not staged. A shared variable is not any one app's change, and
// nothing reads it until an app that uses it deploys: each such app is
// listed among the staged changes from then until it has, which is what
// says the new value is waiting. See StagedChanges.

// SharedVariables lists a project's shared variables, secrets without their
// values, the way an app's are listed.
func (s *Service) SharedVariables(ctx context.Context, ownerID, projectSlug string) ([]Variable, error) {
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return nil, err
	}
	return s.projectVariables(ctx, ownerID, p.ID)
}

func (s *Service) projectVariables(ctx context.Context, ownerID string, projectID uuid.UUID) ([]Variable, error) {
	project, err := s.projectFor(ctx, s.q, ownerID, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListProjectVariables(ctx, dbgen.ListProjectVariablesParams{
		OwnerID: ownerID, ProjectID: project.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("app: list shared variables: %w", err)
	}
	out := make([]Variable, 0, len(rows))
	for _, row := range rows {
		v := Variable{Key: row.Key, Secret: row.Secret}
		if !row.Secret {
			v.Value = row.Value
			v.Reference = HasReference(row.Value)
		}
		out = append(out, v)
	}
	return out, nil
}

// SetSharedVariable stores one of a project's shared variables.
//
// Saved at once, and read by each app that uses it when that app next
// deploys — never by a running one, which keeps what it was started with.
func (s *Service) SetSharedVariable(ctx context.Context, ownerID, projectSlug string, in VariableInput) error {
	in.Key = strings.TrimSpace(in.Key)
	if err := in.Validate(); err != nil {
		return err
	}
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return err
	}
	// A shared value may name an app's variable, the way an app's may. It is
	// checked as though it belonged to none of them, so there is no "itself"
	// for it to refer to.
	if err := s.checkReferences(ctx, ownerID, App{ProjectID: p.ID}, in.Value); err != nil {
		return err
	}
	for _, r := range References(in.Value) {
		if r.App == SharedScope && r.Key == in.Key {
			return fmt.Errorf("%s refers to itself", r)
		}
	}

	params := dbgen.UpsertProjectVariableParams{
		OwnerID: ownerID, ProjectID: p.ID, Key: in.Key, Secret: in.Secret,
	}
	if in.Secret {
		if !s.keeper.Configured() {
			return ErrNoSecretKey
		}
		if params.Sealed, err = s.keeper.Seal(in.Value); err != nil {
			return fmt.Errorf("app: seal %s: %w", in.Key, err)
		}
	} else {
		params.Value = in.Value
	}
	if _, err := s.q.UpsertProjectVariable(ctx, params); err != nil {
		return fmt.Errorf("app: set shared variable: %w", err)
	}
	s.log.Info("shared variable set",
		slog.String("project", p.Slug), slog.String("key", in.Key),
		slog.Bool("secret", in.Secret))
	return nil
}

// ErrSharedVariableInUse refuses removing a shared variable an app still
// refers to. Its next deploy would fail on a name that is no longer there,
// and the person removing it is the one who can see which apps to change.
type ErrSharedVariableInUse struct {
	Key  string
	Apps []string
}

func (e ErrSharedVariableInUse) Error() string {
	return "app: " + strings.Join(e.Apps, ", ") + " " + plural(len(e.Apps), "uses", "use") +
		" ${{shared." + e.Key + "}} — change " + plural(len(e.Apps), "it", "them") + " first"
}

// DeleteSharedVariable removes one of a project's shared variables.
func (s *Service) DeleteSharedVariable(ctx context.Context, ownerID, projectSlug, key string) error {
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return err
	}
	users, err := s.sharedVariableUsers(ctx, ownerID, p.ID, key)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return ErrSharedVariableInUse{Key: key, Apps: users}
	}
	n, err := s.q.DeleteProjectVariable(ctx, dbgen.DeleteProjectVariableParams{
		OwnerID: ownerID, ProjectID: p.ID, Key: key,
	})
	if err != nil {
		return fmt.Errorf("app: delete shared variable: %w", err)
	}
	if n == 0 {
		return ErrVariableNotFound
	}
	s.log.Info("shared variable deleted", slog.String("project", p.Slug), slog.String("key", key))
	return nil
}

// sharedVariableUsers lists the project's apps whose variables, deployed or
// staged, refer to ${{shared.key}} — and the project's other shared
// variables that do, named as shared.OTHER.
func (s *Service) sharedVariableUsers(
	ctx context.Context, ownerID string, projectID uuid.UUID, key string,
) ([]string, error) {
	uses := func(value string) bool {
		for _, r := range References(value) {
			if r.App == SharedScope && r.Key == key {
				return true
			}
		}
		return false
	}
	apps, err := s.q.ListAppsInProject(ctx, dbgen.ListAppsInProjectParams{
		OwnerID: ownerID, ProjectID: pgUUID(projectID),
	})
	if err != nil {
		return nil, fmt.Errorf("app: list the project's apps: %w", err)
	}
	var out []string
	for _, a := range apps {
		vars, err := s.desiredVariablesFor(ctx, s.q, ownerID, a.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range vars {
			if v.Reference && uses(v.Value) {
				out = append(out, a.Name)
				break
			}
		}
	}
	shared, err := s.projectVariables(ctx, ownerID, projectID)
	if err != nil {
		return nil, err
	}
	for _, v := range shared {
		if v.Key != key && v.Reference && uses(v.Value) {
			out = append(out, SharedScope+"."+v.Key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// RevealSharedVariable opens one shared variable's value for the person
// asking. See RevealVariable.
func (s *Service) RevealSharedVariable(ctx context.Context, ownerID, projectSlug, key string) (string, error) {
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return "", err
	}
	row, err := s.q.GetProjectVariable(ctx, dbgen.GetProjectVariableParams{
		OwnerID: ownerID, ProjectID: p.ID, Key: key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrVariableNotFound
	}
	if err != nil {
		return "", fmt.Errorf("app: read shared variable: %w", err)
	}
	if !row.Secret {
		return row.Value, nil
	}
	value, err := s.keeper.Open(row.Sealed)
	if err != nil {
		return "", fmt.Errorf("app: opening %s: %w", key, err)
	}
	return value, nil
}

// sharedReferenceValue is ${{shared.KEY}}, read from the project's current
// shared variables at the moment of the deploy.
func (s *Service) sharedReferenceValue(
	ctx context.Context, q *dbgen.Queries, ownerID string, scope refScope, r Reference, depth int,
) (string, error) {
	project, err := s.projectFor(ctx, q, ownerID, scope.Project)
	if err != nil {
		return "", fmt.Errorf("read the project for %s: %w", r, err)
	}
	row, err := q.GetProjectVariable(ctx, dbgen.GetProjectVariableParams{
		OwnerID: ownerID, ProjectID: project.ID, Key: r.Key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%s refers to %s, which %s no longer shares", r, r.Key, project.Name)
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", r, err)
	}
	value := row.Value
	if row.Secret {
		if value, err = s.keeper.Open(row.Sealed); err != nil {
			return "", fmt.Errorf("opening %s: %w", r, err)
		}
	}
	if HasReference(value) {
		return s.resolveReferences(ctx, q, ownerID, refScope{Project: project.ID, Env: scope.Env}, r.String(), value, depth+1)
	}
	return value, nil
}
