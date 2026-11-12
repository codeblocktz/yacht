package orchestrator

import (
	"context"
	"errors"
)

// VolumeEraser is an orchestrator that can delete the storage behind one of
// an app's volumes while the volume stays the app's.
//
// Optional, like Builder: an orchestrator that cannot is still a working one,
// and the engine says so rather than pretending.
//
// In two steps, so the caller can hold its own lock only while it decides:
// EraseVolume checks that nothing uses the storage and asks for it to be
// deleted; WaitVolumeGone waits, as long as that takes, until it is.
type VolumeEraser interface {
	// EraseVolume deletes the storage behind one of an app's volumes. The
	// volume itself — its name, size and mount path — is the engine's to
	// keep, and the app's next start is given a new, empty one. Storage
	// already gone is no error: Deleted says whether there was any.
	// ErrVolumeInUse while anything runs, or may run, that mounts it;
	// ErrVolumeRetained when deleting it would not delete the data.
	EraseVolume(ctx context.Context, ref Ref, volume string) (VolumeErasure, error)

	// WaitVolumeGone returns once the storage EraseVolume deleted is gone.
	WaitVolumeGone(ctx context.Context, ref Ref, volume string) error
}

// VolumeErasure is what EraseVolume deleted: the claim, by name and uid, and
// the persistent volume it was bound to, "" when it was bound to none.
// Deleted is false when there was nothing to delete.
type VolumeErasure struct {
	Claim            string
	UID              string
	PersistentVolume string
	Deleted          bool
}

// ErrVolumeInUse means a volume's storage is mounted, or would be: by a pod,
// or by a workload that runs or may start one.
var ErrVolumeInUse = errors.New("orchestrator: the volume is in use")

// ErrVolumeRetained means the storage behind a volume outlives its claim:
// its class keeps the data when the claim is deleted, so deleting it would
// erase nothing but the claim.
var ErrVolumeRetained = errors.New("orchestrator: the volume's storage class keeps its data when the claim is deleted")

// ErrVolumeDeleting means a volume's storage is still being deleted, so a
// workload cannot mount it yet. Try again once it is gone; a new one is made
// then.
var ErrVolumeDeleting = errors.New("orchestrator: the volume is still being deleted")
