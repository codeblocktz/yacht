package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

	existing, getErr := s.q.GetVariable(ctx, dbgen.GetVariableParams{
		OwnerID: ownerID, AppID: a.ID, Key: in.Key,
	})
	exists := getErr == nil
	if getErr != nil && !errors.Is(getErr, pgx.ErrNoRows) {
		return fmt.Errorf("app: read variable: %w", getErr)
	}
	if exists && existing.Secret == in.Secret {
		if !in.Secret && existing.Value == in.Value {
			return nil
		}
		if in.Secret {
			value, err := s.keeper.Open(existing.Sealed)
			if err != nil {
				return fmt.Errorf("app: opening %s: %w", in.Key, err)
			}
			if value == in.Value {
				return nil
			}
		}
	}

	params := dbgen.UpsertVariableAndBumpParams{
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

	row, err := s.q.UpsertVariableAndBump(ctx, params)
	if err != nil {
		return fmt.Errorf("app: set variable: %w", err)
	}
	a.ConfigVersion = row.ConfigVersion
	if err := s.recordLinks(ctx, s.q, ownerID, a, in.Key, in.Value); err != nil {
		return err
	}

	// Plain environment is release-owned. Changing a plain value into a secret
	// also removes it from the immutable environment, so that transition gets a
	// release even though its new value lives only in the current overlay.
	releaseOwned := !in.Secret || (exists && !existing.Secret)
	if releaseOwned {
		release, err := s.createRelease(ctx, s.q, a, "")
		if err != nil {
			return err
		}
		if err := s.deployReleaseOperation(
			ctx, ownerID, a, release, "variable:"+in.Key,
		); err != nil {
			return err
		}
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

	row, err := s.q.DeleteVariableAndBump(ctx, dbgen.DeleteVariableAndBumpParams{
		OwnerID: ownerID, AppID: a.ID, Key: key,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrVariableNotFound
		}
		return fmt.Errorf("app: delete variable: %w", err)
	}
	a.ConfigVersion = row.ConfigVersion
	// The edge goes with the variable that carried it.
	if err := s.q.ReplaceAppLinks(ctx, dbgen.ReplaceAppLinksParams{
		FromAppID: a.ID, ViaKey: key,
	}); err != nil {
		return fmt.Errorf("app: clear links: %w", err)
	}
	if !row.Secret {
		release, err := s.createRelease(ctx, s.q, a, "")
		if err != nil {
			return err
		}
		if err := s.deployReleaseOperation(
			ctx, ownerID, a, release, "variable-delete:"+key,
		); err != nil {
			return err
		}
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
			// is what an operator needs to fix it. The reason comes from the
			// wrapped error, which says whether the key that sealed it is
			// missing or the bytes no longer authenticate — those have
			// different answers, and the whole apply stops either way rather
			// than deploying an app with a variable silently absent.
			return nil, nil, fmt.Errorf("app: opening %s: %w", row.Key, err)
		}
		secrets[row.Key] = value
	}
	return plain, secrets, nil
}

// recordLinks notes which other apps a variable's value refers to.
//
// Done here, at the moment the value is written, because this is the only
// place it is readable: a secret is sealed at rest, and the page that draws
// the graph cannot open it. The alternative — decrypt everything on every
// render — would mean opening every secret in the install to draw a picture.
//
// A reference is another app's in-cluster address appearing anywhere in the
// value. That catches a connection string without needing to parse one, and
// without guessing at formats the engine does not control.
func (s *Service) recordLinks(
	ctx context.Context, q *dbgen.Queries, ownerID string, from App, key, value string,
) error {
	// Replaced rather than added to: editing a variable that used to point at a
	// database and now points elsewhere must not leave the old edge behind.
	if err := q.ReplaceAppLinks(ctx, dbgen.ReplaceAppLinksParams{
		FromAppID: from.ID, ViaKey: key,
	}); err != nil {
		return fmt.Errorf("app: clear links: %w", err)
	}
	if value == "" {
		return nil
	}

	peers, err := s.q.ListApps(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("app: list apps for links: %w", err)
	}
	for _, peer := range peers {
		if peer.ID == from.ID {
			continue
		}
		// The name a workload is reachable by inside the cluster. Matching on
		// the qualified host rather than the bare name, so a variable whose
		// value merely contains the word "api" is not a dependency on the app
		// called api.
		host := peer.Name + "." + peer.Namespace
		if !strings.Contains(value, host) {
			continue
		}
		if err := q.CreateAppLink(ctx, dbgen.CreateAppLinkParams{
			OwnerID: ownerID, FromAppID: from.ID, ToAppID: peer.ID, ViaKey: key,
		}); err != nil {
			return fmt.Errorf("app: record link: %w", err)
		}
	}
	return nil
}

// Link is one app's dependency on another, as the graph needs it.
type Link struct {
	From string
	To   string
	Via  string
}

// Links returns every recorded dependency between an owner's apps.
func (s *Service) Links(ctx context.Context, ownerID string) ([]Link, error) {
	rows, err := s.q.ListAppLinks(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("app: list links: %w", err)
	}
	out := make([]Link, 0, len(rows))
	for _, r := range rows {
		out = append(out, Link{From: r.FromName, To: r.ToName, Via: r.ViaKey})
	}
	return out, nil
}
