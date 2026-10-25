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

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// DefaultPlatformVariablePrefix begins the platform's variables when nobody
// chose a prefix. The engine's own name, because it is white-label: a product
// built on it sets Options.PlatformVariablePrefix to its own, and its
// customers never read the engine's name in their environment.
const DefaultPlatformVariablePrefix = "YACHT_"

// The variables every app is given at deploy, after the prefix. Railway's
// set, because an app moving here from there reads RAILWAY_PUBLIC_DOMAIN and
// the rest, and should only have to change the prefix.
const (
	PlatformPublicDomain  = "PUBLIC_DOMAIN"
	PlatformPrivateDomain = "PRIVATE_DOMAIN"
	PlatformServiceName   = "SERVICE_NAME"
	PlatformProjectName   = "PROJECT_NAME"
)

// platformNames is the order they are listed in.
var platformNames = []string{
	PlatformPublicDomain, PlatformPrivateDomain, PlatformServiceName, PlatformProjectName,
}

// ValidatePlatformVariablePrefix refuses a prefix that would make names a
// shell cannot export. Empty is allowed: it means the default.
func ValidatePlatformVariablePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !envKeyRE.MatchString(prefix + PlatformServiceName) {
		return fmt.Errorf("app: platform variable prefix %q would make names a shell cannot export — "+
			"use letters, numbers and underscores, starting with a letter", prefix)
	}
	return nil
}

// PlatformVariablePrefix is what this install's platform variables begin with.
func (s *Service) PlatformVariablePrefix() string { return s.opts.PlatformVariablePrefix }

// isPlatformKey reports whether key is one of the platform's variables.
func (s *Service) isPlatformKey(key string) bool {
	name, ok := strings.CutPrefix(key, s.opts.PlatformVariablePrefix)
	if !ok {
		return false
	}
	for _, n := range platformNames {
		if n == name {
			return true
		}
	}
	return false
}

// platformEnv is what the platform tells an app about itself.
//
// Computed from current state at every apply rather than kept in a release:
// a hostname and a project's name are both things that change without the
// app being redeployed, and an app told the old one would be told something
// false. The public domain is left out, rather than set empty, for an app
// that has none, so a process can test for it the way it would on Railway.
func (s *Service) platformEnv(
	ctx context.Context, q *dbgen.Queries, a App, hosts []string,
) map[string]string {
	p := s.opts.PlatformVariablePrefix
	env := map[string]string{
		p + PlatformPrivateDomain: privateDomain(a),
		p + PlatformServiceName:   a.Name,
	}
	if len(hosts) > 0 {
		env[p+PlatformPublicDomain] = hosts[0]
	}
	project, err := s.projectFor(ctx, q, a.OwnerID, a.ProjectID)
	switch {
	case err == nil:
		env[p+PlatformProjectName] = project.Name
	case errors.Is(err, ErrProjectNotFound) && a.ProjectID == uuid.Nil:
		// Not drawn on a project yet, and the team has never had its default
		// one made: it will be, under this name, the first time anybody looks.
		env[p+PlatformProjectName] = DefaultProjectName
	default:
		// Left out rather than failing the deploy: the name is a convenience,
		// and an app that cannot be told it should still run.
		s.log.Debug("project name unavailable for platform variables",
			slog.String("app", a.Name), slog.String("error", err.Error()))
	}
	return env
}

// addPlatformVariables gives a spec the platform's variables.
//
// A variable the app sets itself wins, whether plain or secret: it is the more
// specific decision, and somebody who set PUBLIC_DOMAIN by hand meant that
// value. The Variables tab says when one is shadowed, so it is never silent.
func (s *Service) addPlatformVariables(
	ctx context.Context, q *dbgen.Queries, a App, hosts []string, spec *orchestrator.AppSpec,
) {
	for key, value := range s.platformEnv(ctx, q, a, hosts) {
		if _, own := spec.Env[key]; own {
			continue
		}
		if _, own := spec.Secrets[key]; own {
			continue
		}
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[key] = value
	}
}

// PlatformVariables lists what the platform gives an app at deploy, for the
// Variables tab: values as the next deploy would set them, read-only.
func (s *Service) PlatformVariables(ctx context.Context, ownerID, appName string) ([]Variable, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return nil, err
	}
	var hosts []string
	// The same address an apply hands the ingress: none for an app kept off
	// the public internet, whatever hostnames it once had.
	if !a.Internal && a.Port != 0 {
		if hosts, err = domain.RoutableHosts(ctx, s.q, a.ID); err != nil {
			return nil, fmt.Errorf("app: read hostnames: %w", err)
		}
	}
	env := s.platformEnv(ctx, s.q, a, hosts)
	out := make([]Variable, 0, len(env))
	for key, value := range env {
		out = append(out, Variable{Key: key, Value: value})
	}
	sort.Slice(out, func(i, j int) bool {
		return platformOrder(out[i].Key) < platformOrder(out[j].Key)
	})
	return out, nil
}

func platformOrder(key string) int {
	for i, n := range platformNames {
		if strings.HasSuffix(key, n) {
			return i
		}
	}
	return len(platformNames)
}

// privateDomain is where an app is reached from inside the cluster.
func privateDomain(a App) string {
	return a.Name + "." + a.Namespace + ".svc.cluster.local"
}

// projectFor reads the project an app is drawn on. An app with none yet is
// on the default one, which is where the next read of the projects puts it;
// read here without creating it, because an apply is no place for that write.
func (s *Service) projectFor(
	ctx context.Context, q *dbgen.Queries, ownerID string, projectID uuid.UUID,
) (dbgen.Project, error) {
	var (
		row dbgen.Project
		err error
	)
	if projectID == uuid.Nil {
		row, err = q.GetProjectBySlug(ctx, dbgen.GetProjectBySlugParams{
			OwnerID: ownerID, Slug: DefaultProjectSlug,
		})
	} else {
		row, err = q.GetProjectByID(ctx, dbgen.GetProjectByIDParams{
			OwnerID: ownerID, ID: projectID,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Project{}, ErrProjectNotFound
	}
	return row, err
}
