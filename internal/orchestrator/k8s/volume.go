package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// secretName is the Secret holding an app's sealed environment.
func secretName(appName string) string { return appName + "-env" }

// applySecret writes the app's secret environment into a Kubernetes Secret.
//
// The container reads it with envFrom rather than having the values inlined as
// literals, which is the whole point: `kubectl get deploy -o yaml` is the copy
// people read, paste into issues and check into repositories, and a password
// in the pod template is a password in all of those.
//
// A Secret is not encryption — it is base64 in etcd unless the cluster
// encrypts at rest. What it buys is that the value stops travelling with the
// object everybody looks at.
func (o *Orchestrator) applySecret(ctx context.Context, spec orchestrator.AppSpec) error {
	if len(spec.Secrets) == 0 {
		// Removed rather than left behind: a Secret nothing references is a
		// copy of values the app no longer uses, sitting where the next person
		// to read the namespace will find it.
		return o.deleteSecret(ctx, spec.Ref)
	}

	sec := corev1ac.Secret(secretName(spec.Name), spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithType(corev1.SecretTypeOpaque).
		WithStringData(spec.Secrets)

	if _, err := o.client.CoreV1().Secrets(spec.Namespace).
		Apply(ctx, sec, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply secret %s: %w", spec.Ref, err)
	}
	return nil
}

// deleteSecret removes an app's environment Secret, tolerating its absence.
func (o *Orchestrator) deleteSecret(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.CoreV1().Secrets(ref.Namespace).
		Delete(ctx, secretName(ref.Name), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete secret %s: %w", ref, err)
	}
	return nil
}

// Probe timings.
//
// Readiness is impatient: three failures and traffic stops arriving, which
// costs nothing but a moment of reduced capacity. Liveness is deliberately
// slower to act, because it kills the container — a probe that gives up at the
// same point as readiness turns a slow start into a restart loop, and that
// presents as the app being broken rather than as the probe being wrong.
const (
	probePeriodSeconds        = 10
	probeTimeoutSeconds       = 3
	readinessFailures   int32 = 3
	livenessFailures    int32 = 6

	// Nothing is probed at all for the first half-minute. An app that needs
	// longer than that to start should say so with a readiness probe that
	// tolerates it, not be killed while it is still opening its database
	// connections.
	livenessInitialDelaySeconds = 30
)

// httpProbe builds a GET against the workload's own service port.
func httpProbe(spec orchestrator.AppSpec, failures int32, initialDelay int32) *corev1ac.ProbeApplyConfiguration {
	return corev1ac.Probe().
		WithHTTPGet(corev1ac.HTTPGetAction().
			WithPath(spec.HealthPath).
			WithPort(intstr.FromInt32(spec.Port))).
		WithInitialDelaySeconds(initialDelay).
		WithPeriodSeconds(probePeriodSeconds).
		WithTimeoutSeconds(probeTimeoutSeconds).
		WithFailureThreshold(failures)
}
