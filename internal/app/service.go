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

	"github.com/codeblocktz/yacht/internal/orchestrator"
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
	Env       map[string]string

	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string

	CreatedAt time.Time
	UpdatedAt time.Time

	// Status is read live from the cluster. Zero value means the cluster did
	// not answer — which is different from "not running" and is rendered
	// differently.
	Status      orchestrator.AppStatus
	StatusKnown bool
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

// Service manages app lifecycle.
type Service struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	orch orchestrator.Orchestrator
	log  *slog.Logger
}

// NewService wires the store and the orchestrator together.
func NewService(
	pool *pgxpool.Pool, orch orchestrator.Orchestrator, log *slog.Logger,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, q: dbgen.New(pool), orch: orch, log: log}
}

// EnsureOwner makes sure the owner row exists, so app inserts have a parent.
func (s *Service) EnsureOwner(ctx context.Context, id, displayName, email string) error {
	_, err := s.q.CreateOwner(ctx, dbgen.CreateOwnerParams{
		ID: id, DisplayName: displayName, Email: email,
	})
	if err != nil {
		return fmt.Errorf("app: ensure owner: %w", err)
	}
	return nil
}

// CreateInput describes a new workload.
type CreateInput struct {
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
	if err := in.Validate(); err != nil {
		return App{}, err
	}

	namespace := Namespace(ownerID, in.Name)
	envJSON, err := marshalEnv(in.Env)
	if err != nil {
		return App{}, err
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
		Env:           envJSON,
		CpuRequest:    in.CPURequest,
		CpuLimit:      in.CPULimit,
		MemoryRequest: in.MemoryRequest,
		MemoryLimit:   in.MemoryLimit,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return App{}, ErrNameTaken
		}
		return App{}, fmt.Errorf("app: create: %w", err)
	}

	created := toApp(row)
	if err := s.apply(ctx, created); err != nil {
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
func (s *Service) apply(ctx context.Context, a App) error {
	if err := s.orch.EnsureNamespace(ctx, orchestrator.NamespaceSpec{
		Owner: orchestrator.OwnerID(a.OwnerID),
		Name:  a.Namespace,
	}); err != nil {
		return err
	}
	return s.orch.ApplyApp(ctx, orchestrator.AppSpec{
		Ref:           a.Ref(),
		Image:         a.Image,
		Replicas:      a.Replicas,
		Port:          a.Port,
		Env:           a.Env,
		CPURequest:    a.CPURequest,
		CPULimit:      a.CPULimit,
		MemoryRequest: a.MemoryRequest,
		MemoryLimit:   a.MemoryLimit,
	})
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
	return a, nil
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

	row, err := s.q.SetAppReplicas(ctx, dbgen.SetAppReplicasParams{
		OwnerID: ownerID, ID: a.ID, Replicas: replicas,
	})
	if err != nil {
		return App{}, fmt.Errorf("app: scale: %w", err)
	}

	updated := toApp(row)
	if err := s.apply(ctx, updated); err != nil {
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
	if err := s.apply(ctx, a); err != nil {
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
		Env:           unmarshalEnv(row.Env),
		CPURequest:    row.CpuRequest,
		CPULimit:      row.CpuLimit,
		MemoryRequest: row.MemoryRequest,
		MemoryLimit:   row.MemoryLimit,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
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
