package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrNoSecretKey means a secret was asked for with no key configured to seal it.
//
// Refusing is the point. Storing it readable instead would give the person the
// protection they asked for in name only, and they would have no way to tell.
var ErrNoSecretKey = errors.New(
	"app: no YACHT_SECRET_KEY is configured, so a secret cannot be stored safely")

// ErrVariableNotFound means the app has no variable with that key.
var ErrVariableNotFound = errors.New("app: no such variable")

// A name a shell will export. A pod spec accepts more than this, and a
// variable a process cannot read is a bug that takes a long time to find.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Variable is one environment entry.
//
// Value is empty for a secret. Nothing that reads a list of variables — a
// page, a log line, an error — can print one by accident, because the type it
// is handed does not carry it.
type Variable struct {
	Key    string
	Value  string
	Secret bool
}

// VariableInput sets or replaces a variable.
type VariableInput struct {
	Key    string
	Value  string
	Secret bool
}

// Validate checks the input before anything is written.
func (in VariableInput) Validate() error {
	switch {
	case in.Key == "":
		return errors.New("a variable needs a name")
	case len(in.Key) > 128:
		return errors.New("variable name must be at most 128 characters")
	case !envKeyRE.MatchString(in.Key):
		return errors.New(
			"variable name must start with a letter or underscore and contain only " +
				"letters, numbers and underscores — a shell will not export anything else")
	}
	return nil
}

// SetVariable stores a variable and applies the app.
func (s *Service) SetVariable(
	ctx context.Context, ownerID, appName string, in VariableInput,
) error {
	in.Key = strings.TrimSpace(in.Key)
	if err := in.Validate(); err != nil {
		return err
	}

	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}

	params := dbgen.UpsertVariableParams{
		OwnerID: ownerID, AppID: a.ID, Key: in.Key, Secret: in.Secret,
	}
	if in.Secret {
		if !s.keeper.Configured() {
			return ErrNoSecretKey
		}
		sealed, err := s.keeper.Seal(in.Value)
		if err != nil {
			return fmt.Errorf("app: seal %s: %w", in.Key, err)
		}
		params.Sealed = sealed
	} else {
		params.Value = in.Value
	}

	if _, err := s.q.UpsertVariable(ctx, params); err != nil {
		return fmt.Errorf("app: set variable: %w", err)
	}

	if err := s.apply(ctx, s.q, a); err != nil {
		return err
	}

	// The key is logged, never the value — including for a variable that is not
	// marked secret, because what somebody put in one is not ours to decide.
	s.log.Info("variable set",
		slog.String("app", appName), slog.String("key", in.Key),
		slog.Bool("secret", in.Secret))
	return nil
}

// DeleteVariable removes a variable and applies the app.
func (s *Service) DeleteVariable(ctx context.Context, ownerID, appName, key string) error {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}

	n, err := s.q.DeleteVariable(ctx, dbgen.DeleteVariableParams{
		OwnerID: ownerID, AppID: a.ID, Key: key,
	})
	if err != nil {
		return fmt.Errorf("app: delete variable: %w", err)
	}
	if n == 0 {
		return ErrVariableNotFound
	}

	if err := s.apply(ctx, s.q, a); err != nil {
		return err
	}

	s.log.Info("variable deleted",
		slog.String("app", appName), slog.String("key", key))
	return nil
}

// variablesFor reads an app's variables without opening any secret.
//
// This is what a page gets. Opening happens only in envFor, on the path to the
// cluster, so a value cannot reach a template by being carried somewhere it was
// not needed.
func (s *Service) variablesFor(
	ctx context.Context, q *dbgen.Queries, appID uuid.UUID,
) ([]Variable, error) {
	rows, err := q.ListVariablesForApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("app: list variables: %w", err)
	}
	out := make([]Variable, 0, len(rows))
	for _, row := range rows {
		v := Variable{Key: row.Key, Secret: row.Secret}
		if !row.Secret {
			v.Value = row.Value
		}
		out = append(out, v)
	}
	return out, nil
}

// envFor returns the readable and sealed variables an app deploys with.
//
// Secrets are opened here and nowhere else. They go to the orchestrator, which
// puts them in a Kubernetes Secret rather than in the pod template — so they
// are absent from `kubectl get deploy -o yaml`, which is the copy people
// actually read and paste into issues.
func (s *Service) envFor(
	ctx context.Context, q *dbgen.Queries, appID uuid.UUID,
) (plain, secrets map[string]string, err error) {
	rows, err := q.ListVariablesForApp(ctx, appID)
	if err != nil {
		return nil, nil, fmt.Errorf("app: list variables: %w", err)
	}

	plain = make(map[string]string)
	secrets = make(map[string]string)
	for _, row := range rows {
		if !row.Secret {
			plain[row.Key] = row.Value
			continue
		}
		value, err := s.keeper.Open(row.Sealed)
		if err != nil {
			// Naming the key and not the value: which secret cannot be opened
			// is what an operator needs to fix it, and the reason is almost
			// always that the key changed.
			return nil, nil, fmt.Errorf("app: opening %s: %w", row.Key, err)
		}
		secrets[row.Key] = value
	}
	return plain, secrets, nil
}
