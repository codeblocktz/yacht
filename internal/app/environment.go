package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Environments, as Railway has them: a project's services, copied.
//
// Production is every project's and is not stored; an environment made beside
// it is a row, and its services are apps of their own — web in staging is the
// app web-staging, whose service is web. Nothing that addresses an app by its
// name changes: its URL, its namespace and its in-cluster host are the copy's
// own, which is what keeps staging from ever reaching production by accident.

// ErrEnvironmentNotFound means the project has no environment by that name.
var ErrEnvironmentNotFound = errors.New("app: no such environment")

// Environment is one of a project's environments.
type Environment struct {
	Slug       string
	Name       string
	CopiedFrom string
	CreatedAt  time.Time

	// Apps is how many services it holds.
	Apps int

	// Production is the environment every project has. It cannot be deleted
	// and is where an app goes when nobody chose.
	Production bool
}

// Environments lists a project's environments, production first.
func (s *Service) Environments(ctx context.Context, ownerID, projectSlug string) ([]Environment, error) {
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListEnvironments(ctx, dbgen.ListEnvironmentsParams{OwnerID: ownerID, ProjectID: p.ID})
	if err != nil {
		return nil, fmt.Errorf("app: list environments: %w", err)
	}
	apps, err := s.q.ListAppsInProject(ctx, dbgen.ListAppsInProjectParams{OwnerID: ownerID, ProjectID: pgUUID(p.ID)})
	if err != nil {
		return nil, fmt.Errorf("app: list the project's apps: %w", err)
	}
	count := map[string]int{}
	for _, a := range apps {
		count[a.Environment]++
	}
	out := []Environment{{Slug: DefaultEnvironment, Name: DefaultEnvironment, Production: true, Apps: count[DefaultEnvironment]}}
	for _, r := range rows {
		out = append(out, Environment{
			Slug: r.Slug, Name: r.Name, CopiedFrom: r.CopiedFrom,
			CreatedAt: r.CreatedAt, Apps: count[r.Slug],
		})
	}
	return out, nil
}

// HasEnvironment reports whether a project has an environment by that slug.
func (s *Service) HasEnvironment(ctx context.Context, ownerID string, p Project, slug string) (bool, error) {
	if slug == "" || slug == DefaultEnvironment {
		return true, nil
	}
	_, err := s.q.GetEnvironment(ctx, dbgen.GetEnvironmentParams{OwnerID: ownerID, ProjectID: p.ID, Slug: slug})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ListInEnvironment returns the apps of one of a project's environments,
// with cluster status, as ListInProject does for all of them.
func (s *Service) ListInEnvironment(ctx context.Context, ownerID string, p Project, env string) ([]App, error) {
	if env == "" {
		env = DefaultEnvironment
	}
	rows, err := s.q.ListAppsInEnvironment(ctx, dbgen.ListAppsInEnvironmentParams{
		OwnerID: ownerID, ProjectID: pgUUID(p.ID), Environment: env,
	})
	if err != nil {
		return nil, fmt.Errorf("app: list the environment's apps: %w", err)
	}
	out := make([]App, 0, len(rows))
	for _, row := range rows {
		out = append(out, toApp(row))
	}
	s.attachListed(ctx, out, false)
	return out, nil
}

// EnvironmentCopy is what creating an environment did with one service.
type EnvironmentCopy struct {
	Service string
	App     string
	// Note is anything somebody should know about the copy: a variable that
	// still names production, a volume that starts empty.
	Note string
	Err  error
}

// CreateEnvironment makes an environment beside production, empty or a copy
// of another of the project's environments — every service in it, as
// Railway's "duplicate" does.
//
// Each copy is a new app, deployed:
//   - a database starts empty, with credentials of its own, because a copy
//     that shared production's data would make staging a way to change it;
//   - any other service starts from the original's source, settings and
//     variables. A variable that holds a database's own connection string
//     becomes a reference to the copy's, and any other in-cluster address of
//     a service in the original environment is rewritten to its copy's, so
//     nothing in staging is left pointing at production;
//   - a volume is attached empty, and a service with a public domain gets
//     its own (<service>-<environment>.up.…).
//
// A copy that fails is reported and the rest go on: half an environment is
// something to look at and fix, and all of it torn down is not.
func (s *Service) CreateEnvironment(
	ctx context.Context, ownerID, projectSlug, name, from string,
) (Environment, []EnvironmentCopy, error) {
	return s.createEnvironment(ctx, ownerID, projectSlug, name, from, envOptions{})
}

// envOptions is how an environment made for a pull request differs from one
// made by hand: its number, and the branch the repository's services build.
type envOptions struct {
	pr     int
	branch func(App) string
}

func (s *Service) createEnvironment(
	ctx context.Context, ownerID, projectSlug, name, from string, opts envOptions,
) (Environment, []EnvironmentCopy, error) {
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return Environment{}, nil, err
	}
	name = strings.TrimSpace(name)
	slug := Slugify(name)
	switch {
	case slug == "":
		return Environment{}, nil, errors.New("an environment needs a name")
	case len(slug) > 30:
		return Environment{}, nil, errors.New("an environment's name is at most 30 characters")
	case slug == DefaultEnvironment:
		return Environment{}, nil, errors.New("every project already has production")
	}
	if from != "" {
		ok, err := s.HasEnvironment(ctx, ownerID, p, from)
		if err != nil {
			return Environment{}, nil, err
		}
		if !ok {
			return Environment{}, nil, fmt.Errorf("%w: %s", ErrEnvironmentNotFound, from)
		}
	}
	row, err := s.q.CreateEnvironment(ctx, dbgen.CreateEnvironmentParams{
		OwnerID: ownerID, ProjectID: p.ID, Slug: slug, Name: name, CopiedFrom: from,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Environment{}, nil, fmt.Errorf("this project already has an environment called %s", slug)
		}
		return Environment{}, nil, fmt.Errorf("app: create environment: %w", err)
	}
	env := Environment{Slug: row.Slug, Name: row.Name, CopiedFrom: row.CopiedFrom, CreatedAt: row.CreatedAt}
	if opts.pr > 0 {
		n := int32(opts.pr)
		if err := s.q.SetEnvironmentPR(ctx, dbgen.SetEnvironmentPRParams{
			OwnerID: ownerID, ProjectID: p.ID, Slug: slug, PrNumber: &n,
		}); err != nil {
			return env, nil, fmt.Errorf("app: record the pull request: %w", err)
		}
	}
	if from == "" {
		return env, nil, nil
	}
	copies, err := s.copyEnvironment(ctx, ownerID, p, from, slug, opts)
	env.Apps = 0
	for _, c := range copies {
		if c.Err == nil {
			env.Apps++
		}
	}
	return env, copies, err
}

// copyEnvironment copies every service of one environment into another.
func (s *Service) copyEnvironment(
	ctx context.Context, ownerID string, p Project, from, to string, opts envOptions,
) ([]EnvironmentCopy, error) {
	sources, err := s.ListInEnvironment(ctx, ownerID, p, from)
	if err != nil {
		return nil, err
	}
	// Databases first: the services that use them are written as references
	// to them, and a reference is to an app that exists.
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].Source.Database() && !sources[j].Source.Database()
	})

	// Every copy's name decided before any is made, so a variable can be
	// rewritten to the address of a copy that has not been created yet.
	names := map[string]string{}
	taken := map[string]bool{}
	for _, src := range sources {
		name, err := s.freeName(ctx, ownerID, copyName(src.ServiceName(), to, taken))
		if err != nil {
			return nil, err
		}
		names[src.Name], taken[name] = name, true
	}

	// The addresses to rewrite, and the connection strings that become
	// references: an exact copy of a database's own URL is that database,
	// and staging's database has a different password.
	hosts := map[string]string{}
	conns := map[string]Reference{}
	for _, src := range sources {
		hosts[src.Name+"."+src.Namespace] = names[src.Name] + "." + Namespace(ownerID, names[src.Name])
		if !src.Source.Database() {
			continue
		}
		blueprint, err := BlueprintForWith(src.Source, s.Capabilities(ctx))
		if err != nil || blueprint.ConnectionKey == "" {
			continue
		}
		if v, err := s.variableValue(ctx, ownerID, src.ID, blueprint.ConnectionKey); err == nil && v != "" {
			conns[v] = Reference{App: src.ServiceName(), Key: blueprint.ConnectionKey}
		}
	}

	var out []EnvironmentCopy
	for _, src := range sources {
		c := EnvironmentCopy{Service: src.ServiceName(), App: names[src.Name]}
		c.Note, c.Err = s.copyService(ctx, ownerID, p, src, names[src.Name], to, hosts, conns, opts)
		out = append(out, c)
	}
	return out, nil
}

// copyName is <service>-<environment>, shortened to fit an app's name.
func copyName(service, env string, taken map[string]bool) string {
	name := service + "-" + env
	if len(name) > maxNameLen {
		keep := maxNameLen - len(env) - 1
		if keep < 1 {
			keep = 1
		}
		name = strings.Trim(service[:min(keep, len(service))], "-") + "-" + env
	}
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", strings.TrimSuffix(name, fmt.Sprintf("-%d", i-1)), i)
	}
	return name
}

// copyService makes one service's copy in the new environment.
func (s *Service) copyService(
	ctx context.Context, ownerID string, p Project, src App, name, env string,
	hosts map[string]string, conns map[string]Reference, opts envOptions,
) (string, error) {
	var notes []string
	in := CreateInput{
		Source: src.Source, ProjectID: p.ID, Region: src.Region,
		Name: name, Environment: env, Service: src.ServiceName(),
		Replicas: max(src.Replicas, 1), Port: src.Port,
		CPURequest: src.CPURequest, CPULimit: src.CPULimit,
		MemoryRequest: src.MemoryRequest, MemoryLimit: src.MemoryLimit,
	}
	if src.Source.Database() {
		// Its own data and its own credentials: the blueprint makes both.
		notes = append(notes, "starts empty, with its own credentials")
	} else if !src.Source.Workload() {
		// A bucket of its own, with its own key: an environment's apps never
		// reach production's files.
		notes = append(notes, "a bucket of its own, starting empty")
	} else if src.Source == SourceGit && !src.Repo.Set() {
		// An empty service is built from what is uploaded to it, and nothing
		// was uploaded to the copy: it waits for its own up.
		in.Empty, in.Settings = true, &src
		notes = append(notes, "waits for its source to be uploaded")
		vars, rewritten, err := s.copyVariables(ctx, ownerID, src, hosts, conns)
		if err != nil {
			return "", err
		}
		in.Variables = vars
		notes = append(notes, rewritten...)
	} else {
		in.Image, in.Repo, in.Settings = src.Image, src.Repo, &src
		if opts.branch != nil {
			if branch := opts.branch(src); branch != "" && branch != in.Repo.Branch {
				in.Repo.Branch = branch
				notes = append(notes, "builds "+branch)
			}
		}
		vars, rewritten, err := s.copyVariables(ctx, ownerID, src, hosts, conns)
		if err != nil {
			return "", err
		}
		in.Variables = vars
		notes = append(notes, rewritten...)
	}
	if _, err := s.Create(ctx, ownerID, in); err != nil {
		return strings.Join(notes, "; "), err
	}

	// A volume the original has and its source did not bring: attached empty.
	if !src.Source.Database() {
		vols, err := s.volumesFor(ctx, s.q, src.ID)
		if err != nil {
			notes = append(notes, "volumes not copied: "+err.Error())
		}
		for _, v := range vols {
			if _, err := s.AttachVolume(ctx, ownerID, name, VolumeInput{
				Name: v.Name, MountPath: v.MountPath, SizeBytes: v.SizeBytes, Class: v.Class,
			}); err != nil {
				notes = append(notes, "volume "+v.Name+" not attached: "+err.Error())
			} else {
				notes = append(notes, "volume "+v.Name+" starts empty")
			}
		}
	}
	// A public service is public in its copy too, at its own address.
	if src.Host != "" && !src.Internal && src.Port > 0 {
		if host, err := s.GenerateDomain(ctx, ownerID, name, 0); err == nil {
			notes = append(notes, "reachable at "+host)
		} else {
			notes = append(notes, "no public domain: "+err.Error())
		}
	}
	return strings.Join(notes, "; "), nil
}

// copyVariables is a service's variables for its copy: references as
// written (they resolve in the copy's environment), connection strings of a
// database as references to its copy, and every other value with the
// original environment's addresses replaced by the copies'.
func (s *Service) copyVariables(
	ctx context.Context, ownerID string, src App, hosts map[string]string, conns map[string]Reference,
) ([]VariableInput, []string, error) {
	rows, err := s.q.ListVariablesForApp(ctx, src.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("app: list %s's variables: %w", src.Name, err)
	}
	var out []VariableInput
	var notes []string
	for _, row := range rows {
		value, secret := row.Value, row.Secret
		if secret {
			if value, err = s.keeper.Open(row.Sealed); err != nil {
				return nil, nil, fmt.Errorf("app: opening %s: %w", row.Key, err)
			}
		}
		if ref, ok := conns[value]; ok {
			out = append(out, VariableInput{Key: row.Key, Value: ref.String()})
			notes = append(notes, row.Key+" now refers to "+ref.App+"'s copy")
			continue
		}
		rewritten := value
		for from, to := range hosts {
			rewritten = strings.ReplaceAll(rewritten, from, to)
		}
		if rewritten != value {
			notes = append(notes, row.Key+" points at the copies")
		}
		out = append(out, VariableInput{Key: row.Key, Value: rewritten, Secret: secret})
	}
	return out, notes, nil
}

// variableValue opens one of an app's variables.
func (s *Service) variableValue(ctx context.Context, ownerID string, appID uuid.UUID, key string) (string, error) {
	row, err := s.q.GetVariable(ctx, dbgen.GetVariableParams{OwnerID: ownerID, AppID: appID, Key: key})
	if err != nil {
		return "", err
	}
	if !row.Secret {
		return row.Value, nil
	}
	return s.keeper.Open(row.Sealed)
}

// copySettings starts a new app with another's runtime settings, in the
// transaction that creates it, so its first release has them.
func (s *Service) copySettings(ctx context.Context, q *dbgen.Queries, row dbgen.App, from App) (dbgen.App, error) {
	updated, err := q.UpdateApp(ctx, dbgen.UpdateAppParams{
		OwnerID: row.OwnerID, ID: row.ID,
		Image: row.Image, Port: row.Port,
		CpuRequest: row.CpuRequest, CpuLimit: row.CpuLimit,
		MemoryRequest: row.MemoryRequest, MemoryLimit: row.MemoryLimit,
		Internal: from.Internal,
		RepoUrl:  row.RepoUrl, RepoBranch: row.RepoBranch, RepoSubdir: row.RepoSubdir,
		StartCommand: from.StartCommand, PreDeployCommand: from.PreDeployCommand,
		CronSchedule: from.CronSchedule, RestartPolicy: string(from.RestartPolicy),
		RestartMaxRetries: from.RestartMaxRetries, HealthTimeoutSeconds: from.HealthTimeoutSeconds,
		DockerfilePath: from.DockerfilePath, WatchPaths: from.WatchPaths,
	})
	switch {
	case err == nil:
		row = updated
	case !errors.Is(err, pgx.ErrNoRows):
		return row, fmt.Errorf("app: copy settings: %w", err)
	}
	if from.HealthPath != "" {
		if row, err = q.SetAppHealth(ctx, dbgen.SetAppHealthParams{
			OwnerID: row.OwnerID, ID: row.ID, HealthPath: from.HealthPath, HealthLiveness: from.Liveness,
		}); err != nil {
			return row, fmt.Errorf("app: copy health check: %w", err)
		}
	}
	return row, nil
}

// DeleteEnvironment removes an environment and every service in it. Their
// deploys are stopped first, as deleting a project does. Production is not
// deleted: it is where the project is.
func (s *Service) DeleteEnvironment(ctx context.Context, ownerID, projectSlug, slug string) error {
	if slug == "" || slug == DefaultEnvironment {
		return errors.New("production cannot be deleted; delete the project instead")
	}
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return err
	}
	if ok, err := s.HasEnvironment(ctx, ownerID, p, slug); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w: %s", ErrEnvironmentNotFound, slug)
	}
	apps, err := s.q.ListAppsInEnvironment(ctx, dbgen.ListAppsInEnvironmentParams{
		OwnerID: ownerID, ProjectID: pgUUID(p.ID), Environment: slug,
	})
	if err != nil {
		return fmt.Errorf("app: list the environment's apps: %w", err)
	}
	for _, a := range apps {
		if err := s.CancelLiveDeployment(ctx, ownerID, a.Name); err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("app: stop %s's deploy: %w", a.Name, err)
		}
		if err := s.Delete(ctx, ownerID, a.Name); err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("app: delete %s: %w", a.Name, err)
		}
	}
	if _, err := s.q.DeleteEnvironment(ctx, dbgen.DeleteEnvironmentParams{
		OwnerID: ownerID, ProjectID: p.ID, Slug: slug,
	}); err != nil {
		return fmt.Errorf("app: delete environment: %w", err)
	}
	return nil
}

type environmentKey struct{}

// WithEnvironment is a request's environment for whatever it creates: an app
// added on staging's canvas is staging's, by whichever path it was added —
// a form, Add › Database, a one-click GitHub deploy — without each of them
// having to carry it.
func WithEnvironment(ctx context.Context, env string) context.Context {
	return context.WithValue(ctx, environmentKey{}, env)
}

func environmentFrom(ctx context.Context) string {
	env, _ := ctx.Value(environmentKey{}).(string)
	return env
}

// placeInEnvironment settles which environment a new app is in and what it
// is called there: a service added outside production is that environment's
// alone, named <service>-<environment> so it cannot take the name production
// would give the same service.
func (s *Service) placeInEnvironment(ctx context.Context, ownerID string, in *CreateInput) error {
	if in.Environment == "" {
		in.Environment = environmentFrom(ctx)
	}
	if in.Environment == "" || in.Environment == DefaultEnvironment {
		in.Environment = ""
		return nil
	}
	if in.ProjectID == uuid.Nil {
		return errors.New("an app in an environment other than production needs its project")
	}
	p, err := s.ProjectByID(ctx, ownerID, in.ProjectID)
	if err != nil {
		return err
	}
	if ok, err := s.HasEnvironment(ctx, ownerID, p, in.Environment); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w: %s", ErrEnvironmentNotFound, in.Environment)
	}
	if in.Service == "" {
		in.Service = in.Name
		in.Name = copyName(in.Name, in.Environment, nil)
	}
	return nil
}

// SetPREnvironments turns a project's pull-request environments on or off.
// Turning them off leaves any already made; closing their pull requests
// still takes them down.
func (s *Service) SetPREnvironments(ctx context.Context, ownerID, projectSlug string, enabled bool) error {
	p, err := s.Project(ctx, ownerID, projectSlug)
	if err != nil {
		return err
	}
	if _, err := s.q.SetProjectPREnvironments(ctx, dbgen.SetProjectPREnvironmentsParams{
		OwnerID: ownerID, ID: p.ID, Enabled: enabled,
	}); err != nil {
		return fmt.Errorf("app: set PR environments: %w", err)
	}
	return nil
}

// prEnvironmentSlug is the environment a pull request gets: pr-<number>.
func prEnvironmentSlug(number int) string { return fmt.Sprintf("pr-%d", number) }
