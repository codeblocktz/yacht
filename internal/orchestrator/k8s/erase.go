package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Compile-time check that the cluster can erase a volume.
var _ orchestrator.VolumeEraser = (*Orchestrator)(nil)

// eraseWait is how long WaitVolumeGone waits for a deleted claim to go. With
// no pod mounting it, its protection finalizer is lifted at once; longer
// than this is a cluster in trouble, and the call can simply be made again.
var eraseWait = time.Minute

// EraseVolume deletes the claim behind one of an app's volumes. The data
// goes with it only as its storage class decides: with reclaim policy Delete
// the provisioner deletes the volume once the claim is gone. A claim whose
// volume is Retain is refused, since deleting it would erase nothing. Other
// storage can keep data whatever the policy says — local-path with its node
// down, a statically provisioned volume, NFS with archiveOnDelete — and
// nothing here can tell.
//
// Refused while anything uses the claim, or may: a pod that mounts it, in
// whatever phase short of finished — pending, running, terminating, a Job's
// retry — and a workload that would start one: the app's Deployment at any
// replicas, its CronJob unless suspended, a Job not yet finished. Pods are
// looked at again once the delete is issued, for one that started in
// between: its claim is then Terminating, and goes when the pod does.
//
// A stopped app is not given a new claim (see applyVolumes); its next start
// is, empty.
func (o *Orchestrator) EraseVolume(
	ctx context.Context, ref orchestrator.Ref, volume string,
) (orchestrator.VolumeErasure, error) {
	name := claimName(ref.Name, volume)
	erasure := orchestrator.VolumeErasure{Claim: name}
	claims := o.client.CoreV1().PersistentVolumeClaims(ref.Namespace)
	claim, err := claims.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return erasure, nil
	}
	if err != nil {
		return erasure, fmt.Errorf("k8s: read volume %s/%s: %w", ref, volume, err)
	}
	erasure.UID, erasure.PersistentVolume = string(claim.UID), claim.Spec.VolumeName

	if claim.DeletionTimestamp == nil {
		if user, err := o.claimUser(ctx, ref, name); err != nil {
			return erasure, err
		} else if user != "" {
			return erasure, fmt.Errorf("%w: %s mounts it", orchestrator.ErrVolumeInUse, user)
		}
		if pvName := claim.Spec.VolumeName; pvName != "" {
			pv, err := o.client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			switch {
			case err == nil && pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain:
				return erasure, fmt.Errorf("%w: %s", orchestrator.ErrVolumeRetained, pvName)
			case err != nil && !apierrors.IsNotFound(err):
				return erasure, fmt.Errorf("k8s: read the volume behind %s/%s: %w", ref, volume, err)
			}
		}

		o.log.Warn("erasing a volume: deleting its claim",
			slog.String("owner", string(ref.Owner)), slog.String("app", ref.Name),
			slog.String("volume", volume), slog.String("claim", name),
			slog.String("uid", erasure.UID), slog.String("persistent_volume", erasure.PersistentVolume))
		// This claim, and not one made since under the same name.
		opts := metav1.DeleteOptions{}
		if uid := claim.UID; uid != "" {
			opts.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		if err := claims.Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
			if apierrors.IsConflict(err) {
				return erasure, fmt.Errorf("k8s: volume %s/%s was replaced while it was being erased; erase it again: %w",
					ref, volume, err)
			}
			return erasure, fmt.Errorf("k8s: delete volume %s/%s: %w", ref, volume, err)
		}
	}
	erasure.Deleted = true

	// A pod that started between the look above and the delete keeps the
	// claim, Terminating, for as long as it runs.
	if pod, err := o.podMounting(ctx, ref.Namespace, name); err != nil {
		return erasure, err
	} else if pod != "" {
		return erasure, fmt.Errorf("%w: pod %s mounts it, so its claim is Terminating and goes once the pod does",
			orchestrator.ErrVolumeInUse, pod)
	}
	return erasure, nil
}

// WaitVolumeGone returns once the claim is gone, not only asked to go: until
// it is, a start would find it Terminating and fail to mount it.
func (o *Orchestrator) WaitVolumeGone(ctx context.Context, ref orchestrator.Ref, volume string) error {
	name := claimName(ref.Name, volume)
	claims := o.client.CoreV1().PersistentVolumeClaims(ref.Namespace)
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
			return fmt.Errorf("%w: %s/%s; erase it again in a moment", orchestrator.ErrVolumeDeleting, ref, volume)
		case <-tick.C:
		}
	}
}

// claimUser is what uses a claim, or may: "" for nothing.
func (o *Orchestrator) claimUser(ctx context.Context, ref orchestrator.Ref, claim string) (string, error) {
	dep, err := o.client.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	switch {
	case err == nil && (dep.Spec.Replicas == nil || *dep.Spec.Replicas > 0) && podSpecMounts(dep.Spec.Template.Spec, claim):
		return "deployment " + dep.Name, nil
	case err != nil && !apierrors.IsNotFound(err):
		return "", fmt.Errorf("k8s: read deployment %s: %w", ref, err)
	}
	cj, err := o.client.BatchV1().CronJobs(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	switch {
	case err == nil && (cj.Spec.Suspend == nil || !*cj.Spec.Suspend) &&
		podSpecMounts(cj.Spec.JobTemplate.Spec.Template.Spec, claim):
		return "cronjob " + cj.Name, nil
	case err != nil && !apierrors.IsNotFound(err):
		return "", fmt.Errorf("k8s: read cronjob %s: %w", ref, err)
	}
	jobs, err := o.client.BatchV1().Jobs(ref.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("k8s: list jobs in %s: %w", ref.Namespace, err)
	}
	for _, j := range jobs.Items {
		if !jobFinished(j) && podSpecMounts(j.Spec.Template.Spec, claim) {
			return "job " + j.Name, nil
		}
	}
	pod, err := o.podMounting(ctx, ref.Namespace, claim)
	if pod != "" {
		pod = "pod " + pod
	}
	return pod, err
}

// podMounting is a pod not yet finished that mounts the claim, "" for none.
func (o *Orchestrator) podMounting(ctx context.Context, namespace, claim string) (string, error) {
	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("k8s: list pods in %s: %w", namespace, err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if podSpecMounts(p.Spec, claim) {
			return p.Name, nil
		}
	}
	return "", nil
}

func podSpecMounts(spec corev1.PodSpec, claim string) bool {
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// jobFinished reports whether a Job has completed or failed for good: a Job
// short of that may start another pod.
func jobFinished(j batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
