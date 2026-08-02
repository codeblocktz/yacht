package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// PendingImage stands in until the first build produces one.
//
// A real reference rather than an empty string, because the app row's image is
// not nullable and every read of it assumes something is there. This one is
// chosen to be obviously not a thing to run: if it ever reaches a cluster, the
// pull fails with the name on the screen rather than starting something.
const PendingImage = "yacht.invalid/not-built-yet:pending"

// Build statuses.
const (
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
)

// ErrNoBuilder means this install cannot build from source.
//
// Its own error rather than a generic failure, because it is a configuration
// answer and not a fault: an install with no registry has nowhere to push, and
// telling somebody that is different from telling them their build broke.
var ErrNoBuilder = errors.New(
	"app: this install cannot build from a repository — no image registry is configured")

// Builder turns a repository into an image.
//
// A seam for the reason the orchestrator is one: what runs the build is a
// Kubernetes Job here and could be something else, and nothing above this line
// should have to know which. It is also what lets the rest of the pipeline be
// tested without a cluster.
//
// The request and result types live in the orchestrator package rather than
// here, so a Kubernetes implementation can satisfy this without importing the
// service that calls it.
type Builder = orchestrator.Builder

// Images says where a build's result goes.
//
// Narrower than the whole registry: the app service needs to name an image and
// to know whether naming one is possible, and nothing else about the
// credential.
type Images interface {
	// ImageFor returns the full reference a build should push to.
	ImageFor(ctx context.Context, ownerID, app, revision string) (string, error)

	// Configured reports whether there is a registry at all.
	Configured(ctx context.Context) bool

	// DockerConfig is the credential, in the form a builder and a kubelet
	// both read. Fetched per use rather than held, so it is unsealed only
	// when something is about to push or pull.
	DockerConfig(ctx context.Context) ([]byte, error)
}

// Build is a record of one.
type Build struct {
	ID           uuid.UUID
	AppID        uuid.UUID
	DeploymentID uuid.UUID

	RepoURL   string
	RepoRef   string
	CommitSHA string
	Image     string

	Status  string
	Message string
	Log     string

	StartedAt  time.Time
	FinishedAt *time.Time
}

// Running reports whether this build is still going.
func (b Build) Running() bool { return b.Status == BuildRunning }

// CanBuild reports whether this install can build from a repository.
func (s *Service) CanBuild(ctx context.Context) bool {
	return s.builder != nil && s.images != nil && s.images.Configured(ctx)
}

// Capabilities is what this install can do, for a picker to render honestly.
func (s *Service) Capabilities(ctx context.Context) Capabilities {
	return Capabilities{CanBuild: s.CanBuild(ctx)}
}

// Sources lists what an app can be created from on this install.
func (s *Service) Sources(ctx context.Context) []Blueprint {
	return BlueprintsFor(s.Capabilities(ctx))
}

// BuildForDeployment returns the build behind one deployment, if there was one.
func (s *Service) BuildForDeployment(
	ctx context.Context, ownerID string, deployID uuid.UUID,
) (Build, error) {
	row, err := s.q.GetBuildForDeployment(ctx, dbgen.GetBuildForDeploymentParams{
		OwnerID: ownerID, DeploymentID: deployID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Build{}, ErrNotFound
		}
		return Build{}, fmt.Errorf("app: read build: %w", err)
	}
	return buildFrom(row), nil
}

// runBuild builds an app's repository and returns the image it produced.
//
// The build log is streamed into the row as it arrives rather than written at
// the end, because the log is most worth reading while the build is running —
// and because a build that is killed halfway would otherwise leave a row with
// nothing in it and no way to find out what happened.
func (s *Service) runBuild(
	ctx context.Context, ownerID string, a App, deployID uuid.UUID, revision string,
) (string, error) {
	if !s.CanBuild(ctx) {
		return "", ErrNoBuilder
	}

	image, err := s.images.ImageFor(ctx, ownerID, a.Name, revision)
	if err != nil {
		return "", err
	}

	row, err := s.q.CreateBuild(ctx, dbgen.CreateBuildParams{
		OwnerID: ownerID, AppID: a.ID, DeploymentID: deployID,
		RepoUrl: a.Repo.URL, RepoRef: a.Repo.Ref(),
	})
	if err != nil {
		return "", fmt.Errorf("app: record build: %w", err)
	}

	auth, err := s.images.DockerConfig(ctx)
	if err != nil {
		return "", err
	}

	result, buildErr := s.builder.Build(ctx, orchestrator.BuildRequest{
		Owner:   orchestrator.OwnerID(ownerID),
		App:     a.Name,
		Image:   image,
		RepoURL: a.Repo.URL,
		Ref:     a.Repo.Ref(),
		Subdir:  a.Repo.Subdir,

		RegistryAuth: auth,
		Log:          s.buildLogger(ctx, row.ID),
	})

	status, message, pushed := BuildSucceeded, "", image
	if buildErr != nil {
		status, message, pushed = BuildFailed, buildErr.Error(), ""
	}
	if _, err := s.q.FinishBuild(ctx, dbgen.FinishBuildParams{
		ID: row.ID, Status: status, Message: message,
		Image: pushed, CommitSha: result.CommitSHA,
	}); err != nil {
		// Logged rather than returned: the build itself is what the caller
		// asked about, and losing the record of a build that worked should not
		// turn it into a build that failed.
		s.log.Error("record build result", "error", err)
	}
	if buildErr != nil {
		return "", buildErr
	}
	return image, nil
}

// buildLogger appends output to the build row as it arrives.
//
// It takes its own context rather than the build's. When a build is cancelled
// the last thing written is usually the reason, and a logger cancelled with it
// would drop exactly that line.
func (s *Service) buildLogger(ctx context.Context, id uuid.UUID) func(string) {
	return func(chunk string) {
		if chunk == "" {
			return
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.q.AppendBuildLog(writeCtx, dbgen.AppendBuildLogParams{
			ID: id, Chunk: chunk,
		}); err != nil {
			s.log.Warn("append build log", "error", err)
		}
	}
}

func buildFrom(row dbgen.Build) Build {
	b := Build{
		ID: row.ID, AppID: row.AppID, DeploymentID: row.DeploymentID,
		RepoURL: row.RepoUrl, RepoRef: row.RepoRef, CommitSHA: row.CommitSha,
		Image: row.Image, Status: row.Status, Message: row.Message,
		Log: row.Log, StartedAt: row.StartedAt,
	}
	if row.FinishedAt.Valid {
		finished := row.FinishedAt.Time
		b.FinishedAt = &finished
	}
	return b
}

// pullAuth is the credential this app needs to pull its own image.
//
// Only for an app whose image this install built. A public image needs no
// credential, and putting the registry password in every tenant namespace to
// pull nginx would spread it for nothing.
//
// A failure here returns no credential rather than failing the deploy. The
// alternative is that a registry problem stops apps that do not use the
// registry from deploying at all.
func (s *Service) pullAuth(ctx context.Context, a App) []byte {
	if a.Source != SourceGit || s.images == nil {
		return nil
	}
	auth, err := s.images.DockerConfig(ctx)
	if err != nil {
		s.log.Warn("no pull credential for a built image",
			"app", a.Name, "error", err)
		return nil
	}
	return auth
}
