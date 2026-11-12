package orchestrator

import (
	"context"
	"errors"
)

// VolumeEraser is an orchestrator that can delete the data behind one of an
// app's volumes while the volume stays the app's.
//
// Optional, like Builder: an orchestrator that cannot is still a working one,
// and the engine says so rather than pretending.
type VolumeEraser interface {
	// EraseVolume deletes the storage behind one of an app's volumes and
	// returns once it is gone. The volume itself — its name, size and mount
	// path — is the engine's to keep; the app's next start is given a new,
	// empty one. Storage already gone is erased. ErrVolumeInUse while a pod
	// still mounts it; ErrVolumeRetained when deleting it would not delete
	// the data.
	EraseVolume(ctx context.Context, ref Ref, volume string) error
}

// ErrVolumeInUse means a volume's storage is still mounted by a pod, which
// would keep the data — and a claim the cluster will not let go of.
var ErrVolumeInUse = errors.New("orchestrator: the volume is still mounted")

// ErrVolumeRetained means the storage behind a volume outlives its claim:
// its class keeps the data when the claim is deleted, so deleting it would
// erase nothing but the claim.
var ErrVolumeRetained = errors.New("orchestrator: the volume's storage class keeps its data when the claim is deleted")
