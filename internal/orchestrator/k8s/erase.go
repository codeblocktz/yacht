package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Compile-time check that the cluster can erase a volume.
var _ orchestrator.VolumeEraser = (*Orchestrator)(nil)

// eraseWait is how long EraseVolume waits for a deleted claim to go. With no
// pod mounting it, its protection finalizer is lifted at once; longer than
// this is a cluster in trouble, and the call can simply be made again.
var eraseWait = time.Minute

// EraseVolume deletes the claim behind one of an app's volumes, and with it,
// through its class's Delete reclaim policy, the data. The app's next start
// makes a new, empty claim of the same size (see applyVolumes); a stopped app
// is not given one in the meantime.
//
// Refused while any pod mounts the claim — the app's own, a pre-deploy
// command's, anything — since deleting it then would leave the data in use
// and the claim stuck behind its finalizer. Refused too when the claim's
// volume is kept on deletion (Retain): deleting the claim would erase nothing
// but the claim, and say it had erased the data.
func (o *Orchestrator) EraseVolume(ctx context.Context, ref orchestrator.Ref, volume string) error {
	name := claimName(ref.Name, volume)
	claims := o.client.CoreV1().PersistentVolumeClaims(ref.Namespace)
	claim, err := claims.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("k8s: read volume %s/%s: %w", ref, volume, err)
	}

	if claim.DeletionTimestamp == nil {
		pods, err := o.client.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("k8s: list pods mounting %s/%s: %w", ref, volume, err)
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			for _, v := range p.Spec.Volumes {
				if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == name {
					return fmt.Errorf("%w: pod %s", orchestrator.ErrVolumeInUse, p.Name)
				}
			}
		}
		if pvName := claim.Spec.VolumeName; pvName != "" {
			pv, err := o.client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			switch {
			case err == nil && pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain:
				return fmt.Errorf("%w: %s", orchestrator.ErrVolumeRetained, pvName)
			case err != nil && !apierrors.IsNotFound(err):
				return fmt.Errorf("k8s: read the volume behind %s/%s: %w", ref, volume, err)
			}
		}
		// This claim, and not one made since under the same name.
		opts := metav1.DeleteOptions{}
		if uid := claim.UID; uid != "" {
			opts.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		if err := claims.Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: delete volume %s/%s: %w", ref, volume, err)
		}
	}

	// Gone, not only asked to go: until it is, a start would mount the old
	// data again.
	wait, cancel := context.WithTimeout(ctx, eraseWait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		_, err := claims.Get(wait, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil && wait.Err() == nil {
			return fmt.Errorf("k8s: read volume %s/%s: %w", ref, volume, err)
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("k8s: volume %s/%s is still being deleted; erase it again in a moment", ref, volume)
		case <-tick.C:
		}
	}
}
