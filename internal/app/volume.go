package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrVolumeShrink means a resize asked for less than the volume already has.
//
// Kubernetes cannot shrink a claim, and nothing else can either: the filesystem
// on it may be full. Refusing is the honest answer, and refusing before
// anything reaches the cluster keeps the database and the cluster agreeing.
var ErrVolumeShrink = errors.New("app: a volume can grow but never shrink")

// ErrVolumeAttached means the volume is still mounted by its app.
var ErrVolumeAttached = errors.New("app: the volume is still attached to the app")

// ErrVolumeNotFound means the app has no such volume.
var ErrVolumeNotFound = errors.New("app: no such volume")

// Volume is storage attached to an app.
type Volume struct {
	ID        uuid.UUID
	AppID     uuid.UUID
	Name      string
	MountPath string
	SizeBytes int64
	Class     string
}

// VolumeInput describes storage to attach.
type VolumeInput struct {
	Name      string
	MountPath string
	SizeBytes int64
	Class     string
}

// Validate checks the input before anything is written or applied.
//
// The orchestrator validates too, and deliberately: this is the message a
// person reads, that one is the invariant the cluster is protected by. Neither
// is redundant — the first can be improved without weakening the second.
func (in VolumeInput) Validate() error {
	switch {
	case in.Name == "":
		return errors.New("a volume needs a name")
	case len(in.Name) > 40:
		return errors.New("volume name must be at most 40 characters")
	case !nameRE.MatchString(in.Name):
		return errors.New("volume name must be lowercase letters, numbers and dashes")
	case in.SizeBytes <= 0:
		return errors.New("a volume needs a size")
	case !path.IsAbs(in.MountPath):
		return errors.New("mount path must be absolute, like /var/lib/data")
	case in.MountPath == "/":
		return errors.New("a volume cannot be mounted at /")
	case path.Clean(in.MountPath) != in.MountPath:
		return fmt.Errorf("mount path %q is not a clean path", in.MountPath)
	}
	return nil
}

// AttachVolume creates storage for an app and applies it.
func (s *Service) AttachVolume(
	ctx context.Context, ownerID, appName string, in VolumeInput,
) (Volume, error) {
	if err := in.Validate(); err != nil {
		return Volume{}, err
	}

	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return Volume{}, err
	}

	// One replica is the condition of having storage at all. Said here as well
	// as in the orchestrator because this is where a person can act on it.
	if a.Replicas > 1 {
		return Volume{}, fmt.Errorf(
			"app: %s runs %d replicas — scale it to one before attaching storage, "+
				"because a volume can only be mounted by one pod at a time",
			appName, a.Replicas)
	}

	row, err := s.q.CreateVolumeAndBump(ctx, dbgen.CreateVolumeAndBumpParams{
		OwnerID: ownerID, AppID: a.ID, Name: in.Name,
		MountPath: in.MountPath, SizeBytes: in.SizeBytes, Class: in.Class,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Volume{}, fmt.Errorf(
				"app: %s already has a volume named %q or mounted at %q",
				appName, in.Name, in.MountPath)
		}
		return Volume{}, fmt.Errorf("app: attach volume: %w", err)
	}
	a.ConfigVersion = row.ConfigVersion

	s.log.Info("volume attached",
		slog.String("app", appName), slog.String("volume", in.Name),
		slog.String("mount", in.MountPath))
	return Volume{
		ID: row.ID, AppID: row.AppID, Name: row.Name,
		MountPath: row.MountPath, SizeBytes: row.SizeBytes, Class: row.Class,
	}, nil
}

// ResizeVolume grows a volume. It cannot shrink one.
func (s *Service) ResizeVolume(
	ctx context.Context, ownerID, appName, volumeName string, sizeBytes int64,
) error {
	if sizeBytes <= 0 {
		return errors.New("app: a volume needs a size")
	}

	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}

	// The comparison lives in the UPDATE's WHERE clause, so a resize that would
	// shrink matches no row rather than relying on a check here. Zero rows
	// means either "no such volume" or "not larger", which the read below tells
	// apart — and the read is only reached on the failure path.
	row, err := s.q.GrowVolumeAndBump(ctx, dbgen.GrowVolumeAndBumpParams{
		OwnerID: ownerID, AppID: a.ID, Name: volumeName, SizeBytes: sizeBytes,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("app: resize volume: %w", err)
		}
		current, readErr := s.q.GetVolume(ctx, dbgen.GetVolumeParams{
			OwnerID: ownerID, AppID: a.ID, Name: volumeName,
		})
		if readErr != nil {
			return ErrVolumeNotFound
		}
		if current.SizeBytes == sizeBytes {
			return nil
		}
		return ErrVolumeShrink
	}
	a.ConfigVersion = row.ConfigVersion

	s.log.Info("volume resized",
		slog.String("app", appName), slog.String("volume", volumeName),
		slog.Int64("bytes", sizeBytes))
	return nil
}

// DeleteVolume removes storage from an app.
//
// Refused while the workload still mounts it unless detach is set, because
// deleting storage is not something to do as a side effect of tidying up. With
// detach, the desired attachment is removed and the reconciler converges the
// workload without it. The persistent claim itself is never deleted here.
func (s *Service) DeleteVolume(
	ctx context.Context, ownerID, appName, volumeName string, detach bool,
) error {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return err
	}

	attached := false
	for _, v := range a.Volumes {
		if v.Name == volumeName {
			attached = true
		}
	}
	if !attached {
		return ErrVolumeNotFound
	}
	if !detach {
		return fmt.Errorf("%w: %s", ErrVolumeAttached, appName)
	}

	row, err := s.q.DeleteVolumeAndBump(ctx, dbgen.DeleteVolumeAndBumpParams{
		OwnerID: ownerID, AppID: a.ID, Name: volumeName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrVolumeNotFound
		}
		return fmt.Errorf("app: delete volume: %w", err)
	}
	a.ConfigVersion = row.ConfigVersion

	s.log.Info("volume deleted",
		slog.String("app", appName), slog.String("volume", volumeName))
	return nil
}

// volumesFor reads an app's storage.
func (s *Service) volumesFor(
	ctx context.Context, q *dbgen.Queries, appID uuid.UUID,
) ([]Volume, error) {
	rows, err := q.ListVolumesForApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("app: list volumes: %w", err)
	}
	out := make([]Volume, 0, len(rows))
	for _, row := range rows {
		out = append(out, toVolume(row))
	}
	return out, nil
}

// volumeSpecs converts stored volumes into what the orchestrator takes.
func volumeSpecs(vols []Volume) []orchestrator.VolumeSpec {
	out := make([]orchestrator.VolumeSpec, 0, len(vols))
	for _, v := range vols {
		out = append(out, orchestrator.VolumeSpec{
			Name: v.Name, MountPath: v.MountPath,
			SizeBytes: v.SizeBytes, Class: v.Class,
		})
	}
	return out
}

func toVolume(row dbgen.Volume) Volume {
	return Volume{
		ID: row.ID, AppID: row.AppID, Name: row.Name,
		MountPath: row.MountPath, SizeBytes: row.SizeBytes, Class: row.Class,
	}
}
