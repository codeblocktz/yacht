// Package app is the engine's workload lifecycle: the layer that keeps the
// database and the cluster agreeing with each other.
//
// The database is the source of truth for which apps exist; the cluster is the
// source of truth for how they are doing. Neither is asked the other's
// question, which is why listing apps never enumerates Deployments and status
// is never cached in a column that can go stale.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrNotFound is returned when an app does not exist for the given owner.
var ErrNotFound = errors.New("app: not found")

// ErrNameTaken is returned when an owner already has an app with that name.
var ErrNameTaken = errors.New("app: name already in use")

// App is a workload as the engine sees it: the stored record plus whatever the
// cluster currently reports.
type App struct {
	ID        uuid.UUID
	OwnerID   string
	Name      string
	Namespace string
	Image     string
	Replicas  int32
	Port      int32

	// Source is what this app was created from, and Internal keeps it off the
	// public internet.
	Source   Source
	Internal bool

	// HealthPath is an HTTP path reporting whether the app is serving. Empty
	// means no probe. Liveness lets the same path also restart the container.
	HealthPath string
	Liveness   bool

	// Variables are this app's environment. A secret's value is not carried
	// here — see Variable.
	Variables []Variable

	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string

	CreatedAt time.Time
	UpdatedAt time.Time

	// ProjectID is the canvas this app belongs to.
	ProjectID uuid.UUID

	// X and Y are where somebody dragged the card to, nil when nobody has.
	// Nil rather than zero because (0,0) is a position a person can choose,
	// and an app pinned to the corner must not be re-laid-out on every render.
	X, Y *int32

	// Status is read live from the cluster. Zero value means the cluster did
	// not answer — which is different from "not running" and is rendered
	// differently.
	Status      orchestrator.AppStatus
	StatusKnown bool

	// Host is the platform-issued hostname, empty when no app domain is
	// configured. Read from the domains table rather than stored on the app,
	// so there is one place a hostname lives.
	Host string

	// Volumes is the storage attached to this app, read alongside it.
	Volumes []Volume

	// TLS reports whether the platform serves Host over TLS. Populated on
	// read from configuration rather than stored, because it is a property of
	// the install and not of the app.
	TLS bool
}

// Deployment is one recorded attempt to run a version of an app.
type Deployment struct {
	ID         uuid.UUID
	AppID      uuid.UUID
	Image      string
	Revision   string
	Status     string
	Message    string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// Source describes where a deployment was triggered from.
//
// Only "image" exists today. It is a method rather than a stored column
// because the answer is derived from the revision, and a column would need a
// migration the first time a build pipeline is added.
func (d Deployment) Source() string {
	switch {
	case strings.HasPrefix(d.Revision, "git:"):
		return "Git"
	case d.Revision == "initial", d.Revision == "":
		return "image"
	}
	return "image"
}

// Ref converts an App to an orchestrator reference.
func (a App) Ref() orchestrator.Ref {
	return orchestrator.Ref{
		Owner:     orchestrator.OwnerID(a.OwnerID),
		Namespace: a.Namespace,
		Name:      a.Name,
	}
}

// URLScheme is https when the platform serves TLS and http otherwise, so the
// dashboard never offers a link that cannot connect.
func (a App) URLScheme() string {
	if a.TLS {
		return "https"
	}
	return "http"
}

// Options are the settings the service needs beyond its dependencies.
type Options struct {
	// Keeper seals secret variables. Left nil, secrets are refused rather than
	// stored readable — a protection somebody asked for and did not get is
	// worse than one they were told they could not have.
	Keeper *secret.Keeper

	// AppDomain is the platform domain apps get hostnames under. Empty
	// switches per-app hostnames off.
	AppDomain string

	// WildcardTLS serves those hostnames over TLS from the ingress
	// controller's default certificate.
	WildcardTLS bool
}

// Service manages app lifecycle.
type Service struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	orch orchestrator.Orchestrator
	log  *slog.Logger
	opts Options

	// keeper seals secret variables. Nil when no key is configured, which is
	// why every use goes through Configured() rather than a nil check.
	keeper *secret.Keeper
}

// NewService wires the store and the orchestrator together.
func NewService(
	pool *pgxpool.Pool, orch orchestrator.Orchestrator, log *slog.Logger, opts Options,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, q: dbgen.New(pool), orch: orch, log: log, opts: opts, keeper: opts.Keeper}
}

// EnsureOwner makes sure the owner row exists, so app inserts have a parent.
func (s *Service) EnsureOwner(ctx context.Context, id, displayName, email string) error {
	_, err := s.q.CreateTeamRow(ctx, dbgen.CreateTeamRowParams{
		ID: id, DisplayName: displayName, Email: email,
	})
	if err != nil {
		return fmt.Errorf("app: ensure owner: %w", err)
	}
	return nil
}

// CreateInput describes a new workload.
type CreateInput struct {
	// Source decides what the app is. Empty means an image the person chose,
	// which is what every app was before sources existed.
	Source Source

	// ProjectID is the canvas this app is drawn on. Zero leaves it unassigned,
	// and the next read of the team's projects adopts it into the default one —
	// which is what a create that never mentioned a project wants.
	ProjectID uuid.UUID

	Name     string
	Image    string
	Replicas int32
	Port     int32
	Env      map[string]string

	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate checks the input before anything is written or applied.
func (in CreateInput) Validate() error {
	switch {
	case in.Name == "":
		return errors.New("name is required")
	case len(in.Name) > 40:
		return errors.New("name must be at most 40 characters")
	case !nameRE.MatchString(in.Name):
		return errors.New("name must be lowercase letters, numbers and dashes")
	case in.Image == "":
		return errors.New("image is required")
	case in.Replicas < 0 || in.Replicas > 50:
		return errors.New("replicas must be between 0 and 50")
	case in.Port < 0 || in.Port > 65535:
		return errors.New("port must be between 0 and 65535")
	}
	return nil
}

// Create stores the app and applies it to the cluster.
//
// The database write and the cluster apply are kept consistent by doing the
// apply inside the transaction and rolling back if it fails. That leaves one
// window — a commit failure after a successful apply — which would orphan
// cluster resources. Orphans are recoverable and idempotent to clean up; a
// database row for a workload that was never applied is not, because nothing
// would ever retry it.
func (s *Service) Create(ctx context.Context, ownerID string, in CreateInput) (App, error) {
	if in.Source == "" {
		in.Source = SourceImage
	}
	blueprint, err := BlueprintFor(in.Source)
	if err != nil {
		return App{}, err
	}

	// A source that names its own image supplies it; only a plain image asks
	// the person for one, which is why validation runs after this rather than
	// before.
	if blueprint.Image != "" {
		in.Image = blueprint.Image
		in.Port = blueprint.Port
	}
	if err := in.Validate(); err != nil {
		return App{}, err
	}

	namespace := Namespace(ownerID, in.Name)

	// Minted before the transaction so a failure here writes nothing. The
	// value exists once, in the Secret it is sealed into — there is no path
	// that shows it again.
	generated := make(map[string]string, len(blueprint.GeneratedSecrets))
	for _, key := range blueprint.GeneratedSecrets {
		value, err := generatedSecret()
		if err != nil {
			return App{}, err
		}
		generated[key] = value
	}
	if len(generated) > 0 && !s.keeper.Configured() {
		return App{}, fmt.Errorf("%w — %s needs one to store its credentials",
			ErrNoSecretKey, blueprint.Label)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return App{}, fmt.Errorf("app: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	q := s.q.WithTx(tx)

	row, err := q.CreateApp(ctx, dbgen.CreateAppParams{
		OwnerID:       ownerID,
		Name:          in.Name,
		Namespace:     namespace,
		Image:         in.Image,
		Replicas:      in.Replicas,
		Port:          in.Port,
		Source:        string(in.Source),
		Internal:      blueprint.Internal,
		CpuRequest:    in.CPURequest,
		CpuLimit:      in.CPULimit,
		MemoryRequest: in.MemoryRequest,
		MemoryLimit:   in.MemoryLimit,
		ProjectID:     pgUUID(in.ProjectID),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return App{}, ErrNameTaken
		}
		return App{}, fmt.Errorf("app: create: %w", err)
	}

	created := toApp(row)

	// The source's own settings first, so anything the person typed with the
	// same name wins rather than being silently overridden by a default.
	for key, value := range blueprint.Env {
		if _, err := q.UpsertVariable(ctx, dbgen.UpsertVariableParams{
			OwnerID: ownerID, AppID: created.ID, Key: key, Value: value,
		}); err != nil {
			return App{}, fmt.Errorf("app: create variable %s: %w", key, err)
		}
	}
	for key, value := range generated {
		sealed, err := s.keeper.Seal(value)
		if err != nil {
			return App{}, err
		}
		if _, err := q.UpsertVariable(ctx, dbgen.UpsertVariableParams{
			OwnerID: ownerID, AppID: created.ID, Key: key, Sealed: sealed, Secret: true,
		}); err != nil {
			return App{}, fmt.Errorf("app: create secret %s: %w", key, err)
		}
	}
	// The address other apps use, sealed because it carries the password.
	if blueprint.ConnectionKey != "" {
		conn := fmt.Sprintf(blueprint.ConnectionTemplate,
			generated[blueprint.GeneratedSecrets[0]],
			created.Name+"."+created.Namespace+".svc.cluster.local")
		sealed, err := s.keeper.Seal(conn)
		if err != nil {
			return App{}, err
		}
		if _, err := q.UpsertVariable(ctx, dbgen.UpsertVariableParams{
			OwnerID: ownerID, AppID: created.ID,
			Key: blueprint.ConnectionKey, Sealed: sealed, Secret: true,
		}); err != nil {
			return App{}, fmt.Errorf("app: create connection string: %w", err)
		}
	}
	// Storage the source brings with it. A database with no data directory is
	// not one somebody has to finish setting up; it is one that has not been
	// deployed.
	if blueprint.Volume != nil {
		if _, err := q.CreateVolume(ctx, dbgen.CreateVolumeParams{
			OwnerID: ownerID, AppID: created.ID,
			Name:      blueprint.Volume.Name,
			MountPath: blueprint.Volume.MountPath,
			SizeBytes: blueprint.Volume.SizeBytes,
		}); err != nil {
			return App{}, fmt.Errorf("app: create volume: %w", err)
		}
	}

	// In the same transaction as the app row. An app whose variables half
	// arrived is one that starts missing configuration it was told to have.
	for key, value := range in.Env {
		v := VariableInput{Key: strings.TrimSpace(key), Value: value}
		if err := v.Validate(); err != nil {
			return App{}, fmt.Errorf("app: variable %q: %w", key, err)
		}
		if _, err := q.UpsertVariable(ctx, dbgen.UpsertVariableParams{
			OwnerID: ownerID, AppID: created.ID, Key: v.Key, Value: v.Value,
		}); err != nil {
			return App{}, fmt.Errorf("app: create variable %s: %w", v.Key, err)
		}
	}

	// Issued inside the same transaction as the app row, so an app cannot
	// exist without its URL and no later step can forget to add one.
	host, err := domain.EnsureManaged(ctx, q, s.managedInput(created))
	if err != nil {
		if errors.Is(err, domain.ErrHostTaken) {
			return App{}, err
		}
		return App{}, fmt.Errorf("app: issue hostname: %w", err)
	}
	created.Host = host
	created.TLS = s.opts.WildcardTLS && host != ""

	if err := s.apply(ctx, q, created); err != nil {
		// Best effort: the workload may be partly applied, and leaving it
		// behind with no record would make it invisible.
		if delErr := s.orch.DeleteApp(ctx, created.Ref()); delErr != nil {
			s.log.Warn("could not clean up after a failed apply",
				slog.String("app", created.Name),
				slog.String("error", delErr.Error()))
		}
		return App{}, err
	}

	if _, err := q.CreateDeployment(ctx, dbgen.CreateDeploymentParams{
		OwnerID: ownerID, AppID: created.ID, Image: created.Image,
		Revision: "initial", Status: "running",
	}); err != nil {
		return App{}, fmt.Errorf("app: record deployment: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return App{}, fmt.Errorf("app: commit: %w", err)
	}

	s.log.Info("app created",
		slog.String("owner", ownerID),
		slog.String("name", created.Name),
		slog.String("namespace", created.Namespace))
	return created, nil
}

// apply ensures the namespace and converges the workload.
//
// The managed hostname is reconciled here rather than only at create, so an
// app domain change moves each app's URL the next time it is applied. Rewriting
// every app at startup would put a config typo in the path of the whole
// install at once.
//
// q is the caller's handle on the store: Create passes its transaction so the
// reconcile reads the app row it has not committed yet, everything else passes
// the pool.
func (s *Service) apply(ctx context.Context, q *dbgen.Queries, a App) error {
	if err := s.orch.EnsureNamespace(ctx, orchestrator.NamespaceSpec{
		Owner: orchestrator.OwnerID(a.OwnerID),
		Name:  a.Namespace,
	}); err != nil {
		return err
	}

	hosts, err := s.reconcileHosts(ctx, q, a)
	if err != nil {
		return err
	}

	// Read here rather than trusted from the caller's copy of the app: an
	// attach writes a row and then applies, and a stale slice would deploy the
	// workload without the storage that was just created for it.
	vols, err := s.volumesFor(ctx, q, a.ID)
	if err != nil {
		return err
	}

	plain, secrets, err := s.envFor(ctx, q, a.ID)
	if err != nil {
		return err
	}

	return s.orch.ApplyApp(ctx, orchestrator.AppSpec{
		Ref:           a.Ref(),
		Image:         a.Image,
		Replicas:      a.Replicas,
		Port:          a.Port,
		Env:           plain,
		Secrets:       secrets,
		CPURequest:    a.CPURequest,
		CPULimit:      a.CPULimit,
		MemoryRequest: a.MemoryRequest,
		MemoryLimit:   a.MemoryLimit,
		Internal:      a.Internal,
		RunAsUser:     runtimeOf(a).RunAsUser,
		FSGroup:       runtimeOf(a).FSGroup,
		ScratchPaths:  runtimeOf(a).ScratchPaths,
		HealthPath:    a.HealthPath,
		Liveness:      a.Liveness,
		Hosts:         hosts,
		TLS:           s.opts.WildcardTLS && len(hosts) > 0,
		Volumes:       volumeSpecs(vols),
	})
}

// reconcileHosts brings the managed hostname in line with current config and
// returns every hostname routed to the app.
// managedInput describes the hostname an app should hold, which is none at all
// when it has no port.
//
// A workload with no port takes no traffic, so a hostname could never reach it
// — and issuing one anyway would both fail validation downstream and hold a
// globally unique name against every other app. Expressing it as an empty app
// domain reuses the existing "feature is off" path, which already retires a
// hostname rather than merely skipping it. That matters: an app is created
// with a port and later has it cleared, and the name has to be released.
func (s *Service) managedInput(a App) domain.ManagedInput {
	appDomain := s.opts.AppDomain
	// A workload with no port cannot be reached, and an internal one speaks a
	// protocol an HTTP hostname cannot carry. Neither gets a name.
	if a.Port == 0 || a.Internal {
		appDomain = ""
	}
	return domain.ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: appDomain, TLS: s.opts.WildcardTLS,
	}
}

func (s *Service) reconcileHosts(
	ctx context.Context, q *dbgen.Queries, a App,
) ([]string, error) {
	if _, err := domain.EnsureManaged(ctx, q, s.managedInput(a)); err != nil {
		return nil, fmt.Errorf("app: reconcile hostname: %w", err)
	}
	return domain.HostsForApp(ctx, q, a.ID)
}

// List returns an owner's apps with live status attached.
func (s *Service) List(ctx context.Context, ownerID string) ([]App, error) {
	rows, err := s.q.ListApps(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("app: list: %w", err)
	}

	out := make([]App, 0, len(rows))
	for _, row := range rows {
		a := toApp(row)
		s.attachStatus(ctx, &a)
		s.attachHost(ctx, &a)
		out = append(out, a)
	}
	return out, nil
}

// Count returns how many apps an owner has.
func (s *Service) Count(ctx context.Context, ownerID string) (int64, error) {
	n, err := s.q.CountApps(ctx, ownerID)
	if err != nil {
		return 0, fmt.Errorf("app: count: %w", err)
	}
	return n, nil
}

// Get returns one app by name, with live status.
func (s *Service) Get(ctx context.Context, ownerID, name string) (App, error) {
	row, err := s.q.GetApp(ctx, dbgen.GetAppParams{OwnerID: ownerID, Name: name})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, fmt.Errorf("app: get: %w", err)
	}
	a := toApp(row)
	s.attachStatus(ctx, &a)
	s.attachHost(ctx, &a)
	return a, nil
}

// attachHost reads the app's hostname, tolerating a store that will not answer.
//
// ListDomainsByApp orders managed first, so index 0 is the platform hostname
// whenever one has been issued.
func (s *Service) attachHost(ctx context.Context, a *App) {
	hosts, err := domain.HostsForApp(ctx, s.q, a.ID)
	if err != nil {
		s.log.Debug("hostname unavailable",
			slog.String("app", a.Name), slog.String("error", err.Error()))
		return
	}
	if len(hosts) > 0 {
		a.Host = hosts[0]
		a.TLS = s.opts.WildcardTLS
	}

	// Read on the same pass. Storage changes what the app page can offer —
	// scaling, and whether a deploy will recreate — so a caller holding an App
	// without it would be deciding from a partial picture.
	if vars, err := s.variablesFor(ctx, s.q, a.ID); err == nil {
		a.Variables = vars
	}
	if vols, err := s.volumesFor(ctx, s.q, a.ID); err == nil {
		a.Volumes = vols
	} else {
		s.log.Debug("volumes unavailable",
			slog.String("app", a.Name), slog.String("error", err.Error()))
	}
}

// attachStatus reads live state, tolerating an unreachable cluster.
//
// A cluster that cannot be reached must not turn a list page into an error
// page: the records are still real and the operator still needs to see them.
func (s *Service) attachStatus(ctx context.Context, a *App) {
	status, err := s.orch.AppStatus(ctx, a.Ref())
	if err != nil {
		if !errors.Is(err, orchestrator.ErrNotFound) {
			s.log.Debug("status unavailable",
				slog.String("app", a.Name), slog.String("error", err.Error()))
		}
		return
	}
	a.Status = status
	a.StatusKnown = true
}

// Deployments returns an app's deployment history, newest first.
func (s *Service) Deployments(
	ctx context.Context, ownerID string, appID uuid.UUID, limit int32,
) ([]Deployment, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.q.ListDeployments(ctx, dbgen.ListDeploymentsParams{
		OwnerID: ownerID, AppID: appID, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("app: list deployments: %w", err)
	}

	out := make([]Deployment, 0, len(rows))
	for _, row := range rows {
		d := Deployment{
			ID:        row.ID,
			AppID:     row.AppID,
			Image:     row.Image,
			Revision:  row.Revision,
			Status:    row.Status,
			Message:   row.Message,
			StartedAt: row.StartedAt,
		}
		if row.FinishedAt.Valid {
			finished := row.FinishedAt.Time
			d.FinishedAt = &finished
		}
		out = append(out, d)
	}
	return out, nil
}

// Activity is a deployment with enough context to render outside an app page.
type Activity struct {
	Deployment
	AppName      string
	AppNamespace string
}

// RecentActivity returns an owner's most recent deployments across every app.
func (s *Service) RecentActivity(
	ctx context.Context, ownerID string, limit int32,
) ([]Activity, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.q.ListRecentDeployments(ctx, dbgen.ListRecentDeploymentsParams{
		OwnerID: ownerID, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("app: recent activity: %w", err)
	}

	out := make([]Activity, 0, len(rows))
	for _, row := range rows {
		a := Activity{
			Deployment: Deployment{
				ID: row.ID, AppID: row.AppID, Image: row.Image,
				Revision: row.Revision, Status: row.Status,
				Message: row.Message, StartedAt: row.StartedAt,
			},
			AppName:      row.AppName,
			AppNamespace: row.AppNamespace,
		}
		if row.FinishedAt.Valid {
			finished := row.FinishedAt.Time
			a.FinishedAt = &finished
		}
		out = append(out, a)
	}
	return out, nil
}

// recordDeployment appends a history entry.
func (s *Service) recordDeployment(
	ctx context.Context, ownerID string, a App, revision, status string,
) {
	if _, err := s.q.CreateDeployment(ctx, dbgen.CreateDeploymentParams{
		OwnerID: ownerID, AppID: a.ID, Image: a.Image,
		Revision: revision, Status: status,
	}); err != nil {
		// History is useful, not load-bearing. Failing a successful deploy
		// because its audit row would not write is the wrong trade.
		s.log.Warn("could not record deployment",
			slog.String("app", a.Name), slog.String("error", err.Error()))
	}
}

// Scale changes the replica count and reapplies.
func (s *Service) Scale(ctx context.Context, ownerID, name string, replicas int32) (App, error) {
	if replicas < 0 || replicas > 50 {
		return App{}, errors.New("replicas must be between 0 and 50")
	}

	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return App{}, err
	}

	// Storage is the reason, not replicas: a volume mounts on one node at a
	// time, so the second pod has nowhere to go. Said here rather than left to
	// the orchestrator, because this is where somebody asked for it.
	if replicas > 1 && len(a.Volumes) > 0 {
		return App{}, fmt.Errorf(
			"app: %s has storage attached, so it runs one replica — "+
				"detach its volumes to scale it", name)
	}

	row, err := s.q.SetAppReplicas(ctx, dbgen.SetAppReplicasParams{
		OwnerID: ownerID, ID: a.ID, Replicas: replicas,
	})
	if err != nil {
		return App{}, fmt.Errorf("app: scale: %w", err)
	}

	updated := toApp(row)
	if err := s.apply(ctx, s.q, updated); err != nil {
		return App{}, err
	}
	s.recordDeployment(ctx, ownerID, updated,
		fmt.Sprintf("scale:%d", replicas), "running")
	return updated, nil
}

// Redeploy reapplies the current spec, which restarts the workload's pods.
func (s *Service) Redeploy(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if err := s.apply(ctx, s.q, a); err != nil {
		return err
	}
	s.recordDeployment(ctx, ownerID, a, "redeploy", "running")
	return nil
}

// Delete removes the workload from the cluster and then the record.
//
// Cluster first: if the record went first and the cluster call failed, the
// workload would keep running with nothing left to describe it.
func (s *Service) Delete(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}

	if err := s.orch.DeleteApp(ctx, a.Ref()); err != nil {
		return err
	}
	if err := s.orch.DeleteNamespace(ctx, a.Namespace); err != nil {
		return err
	}
	if err := s.q.DeleteApp(ctx, dbgen.DeleteAppParams{OwnerID: ownerID, ID: a.ID}); err != nil {
		return fmt.Errorf("app: delete: %w", err)
	}

	s.log.Info("app deleted",
		slog.String("owner", ownerID), slog.String("name", name))
	return nil
}

// Namespace derives a workload's namespace from its owner and name.
//
// A fixed-width hash rather than a slug of the name. Slugging then truncating
// to fit Kubernetes' 63-character limit is how two differently-named apps end
// up sharing a namespace and deleting each other — the truncation silently
// removes the part that made them unique. A hash has no such failure mode, and
// the readable name lives in a label instead.
func Namespace(ownerID, name string) string {
	sum := sha256.Sum256([]byte(ownerID + "/" + name))
	return "yacht-" + hex.EncodeToString(sum[:])[:16]
}

func toApp(row dbgen.App) App {
	return App{
		ID:            row.ID,
		OwnerID:       row.OwnerID,
		Name:          row.Name,
		Namespace:     row.Namespace,
		Image:         row.Image,
		Replicas:      row.Replicas,
		Port:          row.Port,
		Source:        Source(row.Source),
		Internal:      row.Internal,
		HealthPath:    row.HealthPath,
		Liveness:      row.HealthLiveness,
		CPURequest:    row.CpuRequest,
		CPULimit:      row.CpuLimit,
		MemoryRequest: row.MemoryRequest,
		MemoryLimit:   row.MemoryLimit,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
		ProjectID:     row.ProjectID.Bytes,
		X:             row.CanvasX,
		Y:             row.CanvasY,
	}
}

func marshalEnv(env map[string]string) ([]byte, error) {
	if env == nil {
		env = map[string]string{}
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("app: encode env: %w", err)
	}
	return b, nil
}

func unmarshalEnv(b []byte) map[string]string {
	out := map[string]string{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// isUniqueViolation reports whether err is a Postgres unique constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23505")
}

// SetHealth points a probe at a path, or removes it.
//
// Validated through AppSpec before anything is written: the same rules the
// cluster would apply, applied where somebody can read the reason.
func (s *Service) SetHealth(
	ctx context.Context, ownerID, name, healthPath string, liveness bool,
) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}

	probe := a
	probe.HealthPath = strings.TrimSpace(healthPath)
	probe.Liveness = liveness
	if err := (orchestrator.AppSpec{
		Ref: probe.Ref(), Image: probe.Image, Replicas: probe.Replicas,
		Port: probe.Port, HealthPath: probe.HealthPath, Liveness: probe.Liveness,
	}).Validate(); err != nil {
		return err
	}

	row, err := s.q.SetAppHealth(ctx, dbgen.SetAppHealthParams{
		OwnerID: ownerID, ID: a.ID,
		HealthPath: probe.HealthPath, HealthLiveness: probe.Liveness,
	})
	if err != nil {
		return fmt.Errorf("app: set health: %w", err)
	}

	updated := toApp(row)
	if err := s.apply(ctx, s.q, updated); err != nil {
		return err
	}

	s.log.Info("health probe set",
		slog.String("app", name), slog.String("path", probe.HealthPath),
		slog.Bool("liveness", probe.Liveness))
	return nil
}

// runtimeOf returns what the app's source knows about running its image.
//
// Read from the source rather than stored on the app, so a correction to a
// blueprint reaches every app already deployed from it on their next apply.
func runtimeOf(a App) Blueprint {
	b, err := BlueprintFor(a.Source)
	if err != nil {
		return Blueprint{}
	}
	return b
}
