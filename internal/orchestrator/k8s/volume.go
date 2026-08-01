package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// claimName is the PersistentVolumeClaim for one of an app's volumes.
//
// Prefixed with the app so two apps in one namespace could not collide — they
// cannot today, since each app has its own namespace, but the name outlives
// that arrangement and a claim is not a thing to rename later.
func claimName(appName, volumeName string) string {
	return appName + "-" + volumeName
}

// applyVolumes creates or expands the claims a workload asks for.
//
// Claims are applied before the Deployment that mounts them: a pod referring to
// a claim that does not exist yet stays Pending, and while Kubernetes recovers
// from that on its own, the intermediate state is a workload that looks broken
// for no reason a person could act on.
//
// Nothing here deletes. A claim the spec stopped mentioning has been detached,
// not discarded, and destroying storage as a side effect of an edit is the one
// mistake in this subsystem that cannot be undone.
func (o *Orchestrator) applyVolumes(ctx context.Context, spec orchestrator.AppSpec) error {
	for _, v := range spec.Volumes {
		size, err := resource.ParseQuantity(fmt.Sprintf("%d", v.SizeBytes))
		if err != nil {
			return fmt.Errorf("k8s: volume %s size: %w", v.Name, err)
		}

		claim := corev1ac.PersistentVolumeClaim(claimName(spec.Name, v.Name), spec.Namespace).
			WithLabels(orchestrator.ObjectLabels(spec.Ref)).
			WithSpec(corev1ac.PersistentVolumeClaimSpec().
				WithAccessModes(corev1.ReadWriteOnce).
				WithResources(corev1ac.VolumeResourceRequirements().
					WithRequests(corev1.ResourceList{corev1.ResourceStorage: size})))

		// Empty means the cluster default, which is expressed by saying nothing
		// rather than by naming "". Naming the empty string asks for a claim
		// with no class at all, which binds to nothing.
		if v.Class != "" {
			claim = claim.WithSpec(claim.Spec.WithStorageClassName(v.Class))
		}

		if _, err := o.client.CoreV1().PersistentVolumeClaims(spec.Namespace).
			Apply(ctx, claim, applyOpts()); err != nil {
			return fmt.Errorf("k8s: apply volume %s/%s: %w", spec.Ref, v.Name, err)
		}
	}
	return nil
}

// volumeSources returns the pod volumes backing the spec's claims.
func volumeSources(spec orchestrator.AppSpec) []*corev1ac.VolumeApplyConfiguration {
	out := make([]*corev1ac.VolumeApplyConfiguration, 0, len(spec.Volumes))
	for _, v := range spec.Volumes {
		out = append(out, corev1ac.Volume().
			WithName(v.Name).
			WithPersistentVolumeClaim(corev1ac.PersistentVolumeClaimVolumeSource().
				WithClaimName(claimName(spec.Name, v.Name))))
	}
	return out
}

// volumeMounts returns the container mounts for the spec's claims.
func volumeMounts(spec orchestrator.AppSpec) []*corev1ac.VolumeMountApplyConfiguration {
	out := make([]*corev1ac.VolumeMountApplyConfiguration, 0, len(spec.Volumes))
	for _, v := range spec.Volumes {
		out = append(out, corev1ac.VolumeMount().
			WithName(v.Name).
			WithMountPath(v.MountPath))
	}
	return out
}
