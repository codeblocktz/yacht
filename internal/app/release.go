package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrNoManifestResolver means this install cannot turn a tag into an immutable
// image identity. Nothing may be applied in this state: doing so would create
// a release whose meaning can change underneath it.
var ErrNoManifestResolver = errors.New(
	"app: image manifests cannot be resolved on this install")

// Release is the immutable, non-secret runtime state behind one deployment.
// Mutable overlays are deliberately absent; ReleaseOverlays supplies the
// current values when this becomes an AppSpec.
type Release struct {
	ID      uuid.UUID
	OwnerID string
	AppID   uuid.UUID

	ImageRef       string
	ImageDigest    string
	Source         Source
	SourceRevision string

	Replicas int32
	Port     int32

	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
	Internal      bool

	RunAsUser              int64
	FSGroup                int64
	ScratchPaths           []string
	WritableRootFilesystem bool

	HealthPath string
	Liveness   bool

	Env        map[string]string
	SecretKeys []string

	ConfigVersion int64
	CreatedAt     time.Time
	Origin        string
}

// ReleaseOverlays are the fields intentionally kept current across rollback.
// They are named in one type so every AppSpec field belongs visibly either to
// immutable release history or to current external/security state.
type ReleaseOverlays struct {
	Ref           orchestrator.Ref
	ConfigVersion int64
	RegistryAuth  []byte
	Secrets       map[string]string
	Volumes       []orchestrator.VolumeSpec
	Hosts         []string
	TLSHosts      []string
	HTTPSOnly     bool
	CNAMETarget   string
}

// AppSpec reconstructs the exact orchestrator input from a release and the
// current overlays. The image is pinned even though ImageRef retains the tag a
// person recognizes in history.
func (r Release) AppSpec(overlays ReleaseOverlays) orchestrator.AppSpec {
	return orchestrator.AppSpec{
		Ref:                    overlays.Ref,
		ReleaseID:              r.ID.String(),
		ConfigVersion:          overlays.ConfigVersion,
		Image:                  pinImage(r.ImageRef, r.ImageDigest),
		Replicas:               r.Replicas,
		Port:                   r.Port,
		Env:                    cloneStrings(r.Env),
		CPURequest:             r.CPURequest,
		CPULimit:               r.CPULimit,
		MemoryRequest:          r.MemoryRequest,
		MemoryLimit:            r.MemoryLimit,
		WritableRootFilesystem: r.WritableRootFilesystem,
		Hosts:                  append([]string(nil), overlays.Hosts...),
		Internal:               r.Internal,
		RunAsUser:              r.RunAsUser,
		FSGroup:                r.FSGroup,
		ScratchPaths:           append([]string(nil), r.ScratchPaths...),
		HealthPath:             r.HealthPath,
		Liveness:               r.Liveness,
		Secrets:                cloneStrings(overlays.Secrets),
		Volumes:                append([]orchestrator.VolumeSpec(nil), overlays.Volumes...),
		TLSHosts:               append([]string(nil), overlays.TLSHosts...),
		CNAMETarget:            overlays.CNAMETarget,
		HTTPSOnly:              overlays.HTTPSOnly,
		RegistryAuth:           append([]byte(nil), overlays.RegistryAuth...),
	}
}

func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// pinImage replaces a tag with the digest while retaining the spelling of the
// repository. The last colon is a tag separator only when it follows the last
// slash; a registry port such as localhost:5000 must remain intact.
func pinImage(ref, digest string) string {
	ref = strings.TrimSpace(ref)
	if before, _, found := strings.Cut(ref, "@"); found {
		ref = before
	}
	if colon, slash := strings.LastIndexByte(ref, ':'), strings.LastIndexByte(ref, '/'); colon > slash {
		ref = ref[:colon]
	}
	return ref + "@" + digest
}

// createRelease resolves the current image before writing an immutable row.
// Callers invoke it before any cluster apply. Configuration may already be
// saved — a failed release-owned edit remains visible desired state — but a
// floating image never crosses the orchestrator seam.
func (s *Service) createRelease(
	ctx context.Context, q *dbgen.Queries, a App, sourceRevision string,
) (Release, error) {
	if s.manifests == nil {
		return Release{}, ErrNoManifestResolver
	}
	if a.Image == PendingImage {
		return Release{}, fmt.Errorf("%w: %s has not produced an image yet",
			ErrNoManifestResolver, a.Name)
	}
	digest, err := s.manifests.ResolveDigest(ctx, a.Image)
	if err != nil {
		return Release{}, fmt.Errorf("app: resolve %s for release: %w", a.Image, err)
	}
	return s.recordRelease(ctx, q, a, sourceRevision, digest, "deployment")
}

func (s *Service) recordRelease(
	ctx context.Context, q *dbgen.Queries, a App, sourceRevision, digest, origin string,
) (Release, error) {
	if a.Source != SourceGit {
		sourceRevision = ""
	} else if sourceRevision == "" {
		build, buildErr := q.GetSuccessfulBuildForImage(ctx,
			dbgen.GetSuccessfulBuildForImageParams{
				OwnerID: a.OwnerID, AppID: a.ID, Image: a.Image,
			})
		if buildErr != nil && !errors.Is(buildErr, pgx.ErrNoRows) {
			return Release{}, fmt.Errorf("app: read release source revision: %w", buildErr)
		}
		if buildErr == nil {
			sourceRevision = build.CommitSha
		}
	}

	plain, secretKeys, err := releaseVariables(ctx, q, a.ID)
	if err != nil {
		return Release{}, err
	}
	env, err := json.Marshal(plain)
	if err != nil {
		return Release{}, fmt.Errorf("app: encode release variables: %w", err)
	}

	runtime := runtimeOf(a)
	row, err := q.CreateAppRelease(ctx, dbgen.CreateAppReleaseParams{
		OwnerID: a.OwnerID, AppID: a.ID,
		ImageRef: a.Image, ImageDigest: digest,
		Source: string(a.Source), SourceRevision: sourceRevision,
		Replicas: a.Replicas, Port: a.Port,
		CpuRequest: a.CPURequest, CpuLimit: a.CPULimit,
		MemoryRequest: a.MemoryRequest, MemoryLimit: a.MemoryLimit,
		Internal:  a.Internal,
		RunAsUser: runtime.RunAsUser, FsGroup: runtime.FSGroup,
		ScratchPaths:           append([]string{}, runtime.ScratchPaths...),
		WritableRootFilesystem: false,
		HealthPath:             a.HealthPath, HealthLiveness: a.Liveness,
		Env: env, SecretKeys: append([]string{}, secretKeys...),
		ConfigVersion: a.ConfigVersion, Origin: origin,
	})
	if err != nil {
		return Release{}, fmt.Errorf("app: record release: %w", err)
	}
	return toRelease(row), nil
}

func releaseVariables(
	ctx context.Context, q *dbgen.Queries, appID uuid.UUID,
) (map[string]string, []string, error) {
	rows, err := q.ListVariablesForApp(ctx, appID)
	if err != nil {
		return nil, nil, fmt.Errorf("app: list release variables: %w", err)
	}
	plain := make(map[string]string)
	var secretKeys []string
	for _, row := range rows {
		if row.Secret {
			secretKeys = append(secretKeys, row.Key)
			continue
		}
		plain[row.Key] = row.Value
	}
	sort.Strings(secretKeys)
	return plain, secretKeys, nil
}

// Releases returns immutable history newest first.
func (s *Service) Releases(
	ctx context.Context, ownerID string, appID uuid.UUID, limit int32,
) ([]Release, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.q.ListAppReleases(ctx, dbgen.ListAppReleasesParams{
		OwnerID: ownerID, AppID: appID, ResultLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("app: list releases: %w", err)
	}
	out := make([]Release, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRelease(row))
	}
	return out, nil
}

// ReleaseByID returns one owner-scoped snapshot.
func (s *Service) ReleaseByID(
	ctx context.Context, ownerID string, appID, releaseID uuid.UUID,
) (Release, error) {
	row, err := s.q.GetAppRelease(ctx, dbgen.GetAppReleaseParams{
		OwnerID: ownerID, AppID: appID, ID: releaseID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Release{}, ErrNotFound
		}
		return Release{}, fmt.Errorf("app: read release: %w", err)
	}
	return toRelease(row), nil
}

func toRelease(row dbgen.AppRelease) Release {
	env := map[string]string{}
	_ = json.Unmarshal(row.Env, &env)
	return Release{
		ID: row.ID, OwnerID: row.OwnerID, AppID: row.AppID,
		ImageRef: row.ImageRef, ImageDigest: row.ImageDigest,
		Source: Source(row.Source), SourceRevision: row.SourceRevision,
		Replicas: row.Replicas, Port: row.Port,
		CPURequest: row.CpuRequest, CPULimit: row.CpuLimit,
		MemoryRequest: row.MemoryRequest, MemoryLimit: row.MemoryLimit,
		Internal:  row.Internal,
		RunAsUser: row.RunAsUser, FSGroup: row.FsGroup,
		ScratchPaths:           append([]string(nil), row.ScratchPaths...),
		WritableRootFilesystem: row.WritableRootFilesystem,
		HealthPath:             row.HealthPath, Liveness: row.HealthLiveness,
		Env: env, SecretKeys: append([]string(nil), row.SecretKeys...),
		ConfigVersion: row.ConfigVersion, CreatedAt: row.CreatedAt,
		Origin: row.Origin,
	}
}
