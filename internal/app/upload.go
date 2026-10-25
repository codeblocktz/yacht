package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// UploadTriggerPrefix begins the revision of a deploy built from uploaded
// source: "upload:<id>". The deployments list reads it as "Deploy via CLI",
// which is what Railway calls `railway up`.
const UploadTriggerPrefix = "upload:"

// DefaultMaxUploadBytes bounds an uploaded archive, compressed. The archive
// is held in the database until its build has read it, so this is a bound on
// a row, and a directory bigger than this is almost always one whose
// .gitignore is missing node_modules.
const DefaultMaxUploadBytes = 64 << 20

// maxUploadExpanded and maxUploadEntries bound what the archive unpacks to.
// Checked here, before anything is stored, because a small archive that
// expands to fill a build pod's disk is a way to fail a build slowly that
// can be refused quickly.
const (
	maxUploadExpanded = 1 << 30
	maxUploadEntries  = 100_000
)

// uploadFetchTTL is how long a build may take to fetch its source once it
// has started: the builder's own cap, with room for a pod to be scheduled.
const uploadFetchTTL = 40 * time.Minute

// uploadKeep is how long an archive is held when nothing purged it — a build
// that never ran, or a process that stopped before purging.
const uploadKeep = 2 * time.Hour

var (
	// ErrUploadTooLarge is an archive over the limit, compressed or expanded.
	ErrUploadTooLarge = errors.New("app: the upload is too large")

	// ErrUploadNotSource means the app does not build from source, so there
	// is nothing an upload could replace. An image-sourced app runs what it
	// was told to; a database runs its engine.
	ErrUploadNotSource = errors.New(
		"app: this app runs an image rather than building from source, so there is nothing to upload to")

	// ErrUploadUnfetchable means the install has no address a build can
	// fetch uploaded source from: it needs its base URL.
	ErrUploadUnfetchable = errors.New(
		"app: this install cannot build uploaded source — it has no base URL a build can fetch it from")

	// ErrUploadFetchDenied is every fetch that is not allowed, for whatever
	// reason: one answer, so a probe learns nothing about which part was
	// wrong.
	ErrUploadFetchDenied = errors.New("app: source not available")
)

// UploadError is an archive that cannot be built, with the reason in words.
type UploadError struct{ Reason string }

func (e UploadError) Error() string { return "app: the upload cannot be used: " + e.Reason }

// Upload is one archive the CLI sent.
type Upload struct {
	ID           uuid.UUID
	DeploymentID uuid.UUID
	SizeBytes    int64
	SHA256       string
	CreatedAt    time.Time
}

// maxUploadBytes is the configured limit, or the default.
func (s *Service) maxUploadBytes() int64 {
	if s.opts.MaxUploadBytes > 0 {
		return s.opts.MaxUploadBytes
	}
	return DefaultMaxUploadBytes
}

// MaxUploadBytes is the largest archive DeployUpload accepts, for a handler
// to bound what it reads before handing it over.
func (s *Service) MaxUploadBytes() int64 { return s.maxUploadBytes() }

// CanBuildUploads reports whether this install can build source somebody
// uploaded: everything a repository needs, and an address the build can
// fetch the archive from.
func (s *Service) CanBuildUploads(ctx context.Context) bool {
	return s.CanBuild(ctx) && s.opts.SourceFetchURL != ""
}

// DeployUpload builds an archive of somebody's working directory and deploys
// it: `up`. The app must already build from source; the upload stands in for
// the clone for this one deploy, and the next push or redeploy builds the
// repository again, as Railway's does.
//
// The archive, its deploy and its queue record are written in one
// transaction, so an upload that is accepted is one a worker will build, and
// one that is refused leaves nothing behind.
func (s *Service) DeployUpload(
	ctx context.Context, ownerID, appName string, archive []byte,
) (Deployment, Upload, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return Deployment{}, Upload{}, err
	}
	if a.Source != SourceGit {
		return Deployment{}, Upload{}, ErrUploadNotSource
	}
	if !s.CanBuild(ctx) {
		return Deployment{}, Upload{}, ErrNoBuilder
	}
	if s.opts.SourceFetchURL == "" {
		return Deployment{}, Upload{}, ErrUploadUnfetchable
	}
	if int64(len(archive)) > s.maxUploadBytes() {
		return Deployment{}, Upload{}, ErrUploadTooLarge
	}
	if err := checkUploadArchive(archive); err != nil {
		return Deployment{}, Upload{}, err
	}
	sum := sha256.Sum256(archive)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deployment{}, Upload{}, fmt.Errorf("app: begin upload: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	row, err := q.CreateSourceUpload(ctx, dbgen.CreateSourceUploadParams{
		OwnerID: ownerID, AppID: a.ID, SizeBytes: int64(len(archive)),
		Sha256: hex.EncodeToString(sum[:]), Archive: archive,
	})
	if err != nil {
		return Deployment{}, Upload{}, fmt.Errorf("app: store upload: %w", err)
	}
	op, err := s.admitDeploymentTx(ctx, q, ownerID, a, UploadTriggerPrefix+row.ID.String(),
		uuid.Nil, true, "user")
	if err != nil {
		return Deployment{}, Upload{}, err
	}
	if n, err := q.AttachSourceUpload(ctx, dbgen.AttachSourceUploadParams{
		DeploymentID: pgUUID(op.DeploymentID), OwnerID: ownerID, ID: row.ID,
	}); err != nil || n != 1 {
		if err == nil {
			err = fmt.Errorf("attached %d rows", n)
		}
		return Deployment{}, Upload{}, fmt.Errorf("app: attach upload to its deploy: %w", err)
	}
	dep, err := q.GetDeployment(ctx, dbgen.GetDeploymentParams{OwnerID: ownerID, ID: op.DeploymentID})
	if err != nil {
		return Deployment{}, Upload{}, fmt.Errorf("app: read the upload's deploy: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, Upload{}, fmt.Errorf("app: commit upload: %w", err)
	}
	s.log.Info("source uploaded",
		slog.String("app", a.Name), slog.String("upload", row.ID.String()),
		slog.Int64("bytes", row.SizeBytes))
	return toDeployment(dep), Upload{
		ID: row.ID, DeploymentID: op.DeploymentID, SizeBytes: row.SizeBytes,
		SHA256: row.Sha256, CreatedAt: row.CreatedAt,
	}, nil
}

// UploadForDeployment is the upload a deploy was built from, ErrNotFound for
// a deploy that was not.
func (s *Service) UploadForDeployment(ctx context.Context, ownerID string, deployID uuid.UUID) (Upload, error) {
	row, err := s.q.GetSourceUploadForDeployment(ctx, dbgen.GetSourceUploadForDeploymentParams{
		OwnerID: ownerID, DeploymentID: pgUUID(deployID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Upload{}, ErrNotFound
		}
		return Upload{}, fmt.Errorf("app: read upload: %w", err)
	}
	return Upload{
		ID: row.ID, DeploymentID: deployID, SizeBytes: row.SizeBytes,
		SHA256: row.Sha256, CreatedAt: row.CreatedAt,
	}, nil
}

// sourceFetch mints the credential a build fetches its upload with, and the
// URL it fetches from.
//
// Minted when the build starts rather than when the upload arrives: the
// build may start on another replica, minutes later, and the raw value must
// never be stored to wait for it. Each start mints a new one, which replaces
// the last, so a retried build does not leave an older credential working.
func (s *Service) sourceFetch(ctx context.Context, ownerID string, upload Upload) (string, string, error) {
	if s.opts.SourceFetchURL == "" {
		return "", "", ErrUploadUnfetchable
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("app: mint source credential: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	n, err := s.q.SetSourceUploadFetch(ctx, dbgen.SetSourceUploadFetchParams{
		FetchHash:      sum[:],
		FetchExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(uploadFetchTTL), Valid: true},
		OwnerID:        ownerID, ID: upload.ID,
	})
	if err != nil {
		return "", "", fmt.Errorf("app: record source credential: %w", err)
	}
	if n != 1 {
		// Purged: the archive went with a build that finished, or was kept
		// past the point anything could build it.
		return "", "", errors.New("app: the uploaded source is no longer held — run up again")
	}
	return strings.TrimRight(s.opts.SourceFetchURL, "/") + "/" + upload.ID.String(), token, nil
}

// SourceArchive is what a build fetches: the archive, for the holder of the
// credential minted for it. Every refusal is the same ErrUploadFetchDenied.
func (s *Service) SourceArchive(ctx context.Context, id uuid.UUID, token string) ([]byte, error) {
	if token == "" {
		return nil, ErrUploadFetchDenied
	}
	row, err := s.q.GetSourceUploadArchive(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUploadFetchDenied
		}
		return nil, fmt.Errorf("app: read upload: %w", err)
	}
	sum := sha256.Sum256([]byte(token))
	// Constant time, though both sides are digests: comparing them any other
	// way is a habit that is one refactor from comparing raw tokens.
	if len(row.FetchHash) == 0 || subtle.ConstantTimeCompare(sum[:], row.FetchHash) != 1 {
		return nil, ErrUploadFetchDenied
	}
	if !row.FetchExpiresAt.Valid || time.Now().After(row.FetchExpiresAt.Time) {
		return nil, ErrUploadFetchDenied
	}
	if err := s.q.MarkSourceUploadFetched(ctx, id); err != nil {
		s.log.Warn("record source fetched", slog.String("error", err.Error()))
	}
	return row.Archive, nil
}

// purgeUpload drops an archive its build is done with, whichever way it went.
func (s *Service) purgeUpload(ctx context.Context, ownerID string, upload Upload) {
	if err := s.q.PurgeSourceUpload(context.WithoutCancel(ctx), dbgen.PurgeSourceUploadParams{
		OwnerID: ownerID, ID: upload.ID,
	}); err != nil {
		s.log.Warn("drop an uploaded archive", slog.String("error", err.Error()))
	}
}

// RunUploadJanitor drops archives nothing will build any more, for the
// uploads whose build never purged them. Safe on every replica: it is one
// UPDATE, and the second replica to run it finds nothing.
func (s *Service) RunUploadJanitor(ctx context.Context) {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		n, err := s.q.PurgeStaleSourceUploads(ctx, time.Now().Add(-uploadKeep))
		if err != nil && ctx.Err() == nil {
			s.log.Warn("drop stale uploads", slog.String("error", err.Error()))
		} else if n > 0 {
			s.log.Info("dropped uploaded archives nothing built", slog.Int64("count", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// checkUploadArchive refuses an archive a build should not unpack: not a
// gzipped tar, a path that leaves the directory it is unpacked into, a
// device, or more than the limits. Said here, at upload, because the same
// archive refused by tar in a build pod is a failed deploy with a worse
// message several minutes later.
func checkUploadArchive(archive []byte) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return UploadError{"it is not a gzipped tar archive"}
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var expanded int64
	entries, files := 0, 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return UploadError{"the archive is damaged: " + err.Error()}
		}
		entries++
		if entries > maxUploadEntries {
			return UploadError{fmt.Sprintf("it has more than %d entries", maxUploadEntries)}
		}
		if !safeArchivePath(h.Name) {
			return UploadError{fmt.Sprintf("%q is outside the directory it would unpack into", h.Name)}
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			files++
			expanded += h.Size
			if expanded > maxUploadExpanded {
				return ErrUploadTooLarge
			}
		case tar.TypeDir, tar.TypeXGlobalHeader, tar.TypeXHeader:
		case tar.TypeSymlink, tar.TypeLink:
			// A link is fine inside the tree and nowhere else: one to
			// /etc/passwd, or to ../../, is how an archive reaches outside it.
			target := h.Linkname
			if h.Typeflag == tar.TypeSymlink && !strings.HasPrefix(target, "/") {
				target = path.Join(path.Dir(h.Name), target)
			}
			if !safeArchivePath(target) {
				return UploadError{fmt.Sprintf("%q links outside the directory", h.Name)}
			}
		default:
			return UploadError{fmt.Sprintf("%q is not a file, directory or link", h.Name)}
		}
	}
	if files == 0 {
		return UploadError{"it has no files in it"}
	}
	return nil
}

// safeArchivePath reports whether a path stays inside the directory it is
// unpacked into.
func safeArchivePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	clean := path.Clean(name)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

// uploadFor is the upload an operation's deploy builds, if it was made by up.
// A read that fails is treated as no upload only when there is none; any
// other failure would build the repository in place of what was uploaded,
// so it is logged and the deploy builds nothing it was not asked to.
func (s *Service) uploadFor(ctx context.Context, op Operation) (Upload, bool) {
	u, err := s.UploadForDeployment(ctx, op.OwnerID, op.DeploymentID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.log.Warn("read a deploy's upload", slog.String("error", err.Error()))
		}
		return Upload{}, false
	}
	return u, true
}
