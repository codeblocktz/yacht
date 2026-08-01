package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ErrTemplateNotFound means no template goes by that slug.
var ErrTemplateNotFound = errors.New("app: no such template")

// Template is a stack: several apps deployed together and already wired up.
//
// Data rather than code, for the reason Blueprint is: adding one is filling
// this in, and nothing that consumes a template needs to know the new one
// exists. A template composes blueprints rather than restating them, so a
// change to how Postgres is deployed reaches every template that includes it.
type Template struct {
	Slug        string
	Label       string
	Description string

	// Apps are deployed in order, because a later one may take a variable from
	// an earlier one. Order is the dependency; there is no graph to resolve
	// because a stack that needed one would be past what this is for.
	Apps []TemplateApp
}

// TemplateApp is one app in a stack.
type TemplateApp struct {
	// Name is the suffix. The app is called <stack>-<name>, so deploying the
	// same template twice does not collide and every name says what it belongs
	// to.
	Name   string
	Source Source

	// Image and Port apply only to a plain image; a source that names its own
	// supplies both.
	Image string
	Port  int32

	Env map[string]string

	// NeedsFrom names an earlier app in the stack whose connection variable
	// this one should receive. The variable is copied under the providing
	// blueprint's own ConnectionKey, so the consumer sees exactly the name that
	// blueprint documents.
	NeedsFrom string
}

// Templates returns every stack, in the order a picker should show them.
func Templates() []Template {
	return []Template{
		{
			Slug:  "postgres-app",
			Label: "Web service and Postgres",
			Description: "A container image with a Postgres database beside it, " +
				"already holding the connection string.",
			Apps: []TemplateApp{
				{Name: "db", Source: SourcePostgres},
				{
					Name:   "web",
					Source: SourceImage,
					// Unprivileged on purpose. The security posture refuses a
					// container that would run as root, so a template shipping
					// a root image would deploy, wire up correctly, and then
					// never start — which is the worst of both. This one
					// declares a non-root user and listens above 1024.
					Image: "nginxinc/nginx-unprivileged:1.27-alpine",
					Port:  8080,
					// The consumer is named after what it consumes rather than
					// after the template, so a person reading the canvas sees
					// which app the edge comes from.
					NeedsFrom: "db",
				},
			},
		},
	}
}

// TemplateFor returns one template by slug.
func TemplateFor(slug string) (Template, error) {
	for _, t := range Templates() {
		if t.Slug == slug {
			return t, nil
		}
	}
	return Template{}, ErrTemplateNotFound
}

// DeployTemplate creates a project and the stack's apps inside it.
//
// Not a single transaction, and deliberately so. Each app is created by the
// same Create that a person uses, which applies to the cluster as it goes — a
// transaction spanning all of them would either hold one open across several
// cluster calls or roll back database rows for workloads that already exist.
// A stack that fails halfway leaves what it made, on a canvas that shows
// exactly which parts arrived.
func (s *Service) DeployTemplate(
	ctx context.Context, ownerID, slug, stack string,
) (Project, error) {
	tmpl, err := TemplateFor(slug)
	if err != nil {
		return Project{}, err
	}

	stack = strings.TrimSpace(stack)
	if err := ValidateStackName(stack); err != nil {
		return Project{}, err
	}

	project, err := s.CreateProject(ctx, ownerID, stack)
	if err != nil {
		return Project{}, err
	}

	// What each app in the stack is reachable at, so a later one can be given
	// an earlier one's connection string.
	made := make(map[string]App, len(tmpl.Apps))

	for _, spec := range tmpl.Apps {
		in := CreateInput{
			Source:    spec.Source,
			ProjectID: project.ID,
			Name:      stack + "-" + spec.Name,
			Image:     spec.Image,
			Port:      spec.Port,
			Replicas:  1,
			Env:       spec.Env,
		}

		created, err := s.Create(ctx, ownerID, in)
		if err != nil {
			return project, fmt.Errorf("app: %s in stack %s: %w", spec.Name, stack, err)
		}
		made[spec.Name] = created

		if spec.NeedsFrom == "" {
			continue
		}
		from, ok := made[spec.NeedsFrom]
		if !ok {
			// A template naming an app that is not earlier in its own list.
			// Refused rather than skipped: the stack would deploy looking
			// complete and be missing the wiring that is the point of it.
			return project, fmt.Errorf(
				"app: template %s wires %s to %s, which it does not create first",
				slug, spec.Name, spec.NeedsFrom)
		}
		if err := s.copyConnection(ctx, ownerID, from, created); err != nil {
			return project, err
		}
	}

	s.log.Info("template deployed",
		slog.String("template", slug), slog.String("stack", stack),
		slog.Int("apps", len(tmpl.Apps)))
	return project, nil
}

// copyConnection gives one app another's connection string.
//
// Opened and re-sealed rather than shared: a variable belongs to the app it is
// set on, and there is no path here that hands a value between apps without
// the keeper. Writing it through SetVariable rather than the store directly is
// what records the link — the edge on the canvas exists because the value
// names the other app, and that is noticed at the moment it is written.
func (s *Service) copyConnection(ctx context.Context, ownerID string, from, to App) error {
	blueprint, err := BlueprintFor(from.Source)
	if err != nil {
		return err
	}
	if blueprint.ConnectionKey == "" {
		return fmt.Errorf("app: %s offers no connection string to give %s", from.Name, to.Name)
	}

	_, secrets, err := s.envFor(ctx, s.q, from.ID)
	if err != nil {
		return fmt.Errorf("app: read %s connection: %w", from.Name, err)
	}
	value, ok := secrets[blueprint.ConnectionKey]
	if !ok {
		return fmt.Errorf("app: %s has no %s to give", from.Name, blueprint.ConnectionKey)
	}

	return s.SetVariable(ctx, ownerID, to.Name, VariableInput{
		Key: blueprint.ConnectionKey, Value: value, Secret: true,
	})
}

// ValidateStackName checks the prefix every app in a stack is named with.
//
// Stricter than an app name by exactly one character: a stack name becomes
// "<stack>-<app>", so it has to leave room and it cannot end in the dash that
// joins them.
func ValidateStackName(stack string) error {
	switch {
	case stack == "":
		return errors.New("a stack needs a name")
	case len(stack) > 30:
		return errors.New("stack name must be at most 30 characters, to leave room for the app names")
	case !nameRE.MatchString(stack):
		return errors.New("stack name must be lowercase letters, numbers and dashes")
	}
	return nil
}
