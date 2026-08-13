package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// servicePort is the stable port the in-cluster Service listens on, regardless
// of what port the container uses. Keeping it fixed means ingress and
// service-discovery config does not change when an app changes its port.
const servicePort int32 = 80

// deploymentProgressDeadline is also the upper bound used by the application
// worker. Setting it on the object lets the Deployment controller supply the
// useful ProgressDeadlineExceeded reason instead of Yacht inventing one from
// replica counts alone.
const deploymentProgressDeadline int32 = 600

// ApplyApp converges a workload to the given spec.
//
// Uses server-side apply throughout, so calling it repeatedly with the same
// spec is a no-op rather than a rollout. The revision annotation is a content
// hash for the same reason: a timestamp would restart every workload on every
// reconcile.
func (o *Orchestrator) ApplyApp(ctx context.Context, spec orchestrator.AppSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	// Before the Deployment that mounts them: a pod naming a claim that does
	// not exist yet sits Pending, and the recovery is invisible to whoever is
	// watching the deploy.
	if err := o.applyVolumes(ctx, spec); err != nil {
		return err
	}

	// Before the Deployment that reads it: a pod whose envFrom names a Secret
	// that does not exist yet fails to start, and recovers only once the
	// kubelet retries.
	if err := o.applySecret(ctx, spec); err != nil {
		return err
	}

	if err := o.applyDeployment(ctx, spec); err != nil {
		return err
	}

	// A workload with no port takes no traffic and needs neither a Service nor
	// an Ingress. Prune rather than merely skip: an app that used to have a
	// port would otherwise keep serving through objects nothing maintains.
	if spec.Port > 0 {
		if err := o.applyService(ctx, spec); err != nil {
			return err
		}
	} else if err := o.deleteService(ctx, spec.Ref); err != nil {
		return err
	}

	if spec.Port > 0 && len(spec.Hosts) > 0 {
		if err := o.applyIngress(ctx, spec); err != nil {
			return err
		}
	} else if err := o.deleteIngress(ctx, spec.Ref); err != nil {
		return err
	}

	// The Deployment metadata key is the commit marker for the whole apply,
	// not merely for the Deployment. Writing it last means a Service or
	// Ingress failure remains visibly drifted and the app reconciler retries
	// the complete idempotent apply.
	if err := o.markDeploymentConverged(ctx, spec); err != nil {
		return err
	}

	o.log.Info("app applied",
		slog.String("ref", spec.Ref.String()),
		slog.String("image", spec.Image),
		slog.Int("replicas", int(spec.Replicas)),
	)
	return nil
}

func (o *Orchestrator) applyDeployment(ctx context.Context, spec orchestrator.AppSpec) error {
	container, err := o.container(spec)
	if err != nil {
		return err
	}

	// Before the workload, so the kubelet has the credential by the time it
	// tries the pull. The other order produces one failed pull per app on
	// every first deploy, reported as ImagePullBackOff, which reads as a bad
	// image name.
	if err := o.ensurePullSecret(ctx, spec); err != nil {
		return err
	}

	podLabels := orchestrator.ObjectLabels(spec.Ref)
	podAnnotations := map[string]string{
		orchestrator.AnnotationRevision:      specHash(spec),
		orchestrator.AnnotationReleaseID:     spec.ReleaseID,
		orchestrator.AnnotationConfigVersion: strconv.FormatInt(spec.ConfigVersion, 10),
	}
	pendingAnnotations := map[string]string{
		orchestrator.AnnotationRevision:      specHash(spec),
		orchestrator.AnnotationReleaseID:     "",
		orchestrator.AnnotationConfigVersion: "-1",
	}

	podSpec := corev1ac.PodSpec().
		// Nothing the engine deploys talks to the Kubernetes API, so nothing
		// needs a token. Mounting one by default is how a container escape
		// becomes a cluster escape.
		WithAutomountServiceAccountToken(false).
		WithSecurityContext(restrictedPodSecurityContext(spec)).
		WithContainers(container).
		// An image this install built lives in a private registry, so the
		// kubelet needs a credential to pull it. Referenced unconditionally
		// and created only when there is one: a pull secret that does not
		// exist is ignored, whereas a missing reference means an app that
		// deploys and then cannot start, reported as ImagePullBackOff.
		WithImagePullSecrets(corev1ac.LocalObjectReference().
			WithName(orchestrator.PullSecretName)).
		WithVolumes(append(
			[]*corev1ac.VolumeApplyConfiguration{
				corev1ac.Volume().
					WithName(tmpVolumeName).
					WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
			},
			volumeSources(spec)...,
		)...)

	// A rolling update starts the replacement before the original stops. With a
	// ReadWriteOnce claim the replacement cannot mount what the original still
	// holds, so it waits forever and the deploy never completes — no error, no
	// timeout, just a pod that never becomes ready.
	//
	// Recreating costs downtime on every deploy, which is why it is applied
	// only when there is storage to justify it, and said out loud in the
	// dashboard rather than changed underneath the operator.
	strategy := appsv1ac.DeploymentStrategy().
		WithType(appsv1.RollingUpdateDeploymentStrategyType)
	if len(spec.Volumes) > 0 {
		strategy = appsv1ac.DeploymentStrategy().
			WithType(appsv1.RecreateDeploymentStrategyType)
	}

	dep := appsv1ac.Deployment(spec.Name, spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithAnnotations(pendingAnnotations).
		WithSpec(appsv1ac.DeploymentSpec().
			WithStrategy(strategy).
			WithProgressDeadlineSeconds(deploymentProgressDeadline).
			WithReplicas(spec.Replicas).
			WithSelector(metav1ac.LabelSelector().
				WithMatchLabels(orchestrator.SelectorLabels(spec.Name))).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(podLabels).
				WithAnnotations(podAnnotations).
				WithSpec(podSpec)))

	// The API server defaults spec.strategy.rollingUpdate whenever the strategy
	// is RollingUpdate, and then refuses type=Recreate while that block is
	// still present. Those defaulted fields belong to the defaulter rather than
	// to this field manager, so an apply that simply omits them leaves them in
	// place and every subsequent deploy fails validation — with the workload
	// carrying on unchanged, which is what makes it easy to miss.
	//
	// Removing them explicitly is the only way to change the strategy of a
	// Deployment that already exists. A fake clientset does not validate, so
	// this is invisible to every test that does not touch a real API server.
	if len(spec.Volumes) > 0 {
		// Type and block change together. Removing rollingUpdate on its own is
		// undone immediately: while the type is still RollingUpdate the API
		// server defaults the block straight back, and the apply that follows
		// then fails on exactly the field just deleted.
		const switchToRecreate = `{"spec":{"strategy":{"type":"Recreate","rollingUpdate":null}}}`
		_, err := o.client.AppsV1().Deployments(spec.Namespace).
			Patch(ctx, spec.Name, types.MergePatchType,
				[]byte(switchToRecreate), metav1.PatchOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: clear rolling update for %s: %w", spec.Ref, err)
		}
	}

	if _, err := o.client.AppsV1().Deployments(spec.Namespace).
		Apply(ctx, dep, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply deployment %s: %w", spec.Ref, err)
	}
	return nil
}

func (o *Orchestrator) markDeploymentConverged(
	ctx context.Context, spec orchestrator.AppSpec,
) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			orchestrator.AnnotationReleaseID:     spec.ReleaseID,
			orchestrator.AnnotationConfigVersion: strconv.FormatInt(spec.ConfigVersion, 10),
		}},
	})
	if err != nil {
		return fmt.Errorf("k8s: encode convergence marker: %w", err)
	}
	if _, err := o.client.AppsV1().Deployments(spec.Namespace).Patch(
		ctx, spec.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("k8s: mark deployment %s converged: %w", spec.Ref, err)
	}
	return nil
}

func (o *Orchestrator) container(
	spec orchestrator.AppSpec,
) (*corev1ac.ContainerApplyConfiguration, error) {
	c := corev1ac.Container().
		WithName(spec.Name).
		WithImage(spec.Image).
		WithImagePullPolicy(corev1.PullIfNotPresent).
		WithSecurityContext(restrictedContainerSecurityContext(spec.WritableRootFilesystem)).
		WithVolumeMounts(append(
			[]*corev1ac.VolumeMountApplyConfiguration{
				corev1ac.VolumeMount().
					WithName(tmpVolumeName).
					WithMountPath("/tmp"),
			},
			volumeMounts(spec)...,
		)...)

	// Readiness first: it is what keeps traffic away from a container that is
	// not serving yet, which is the whole of what most workloads need.
	if spec.HealthPath != "" {
		c = c.WithReadinessProbe(httpProbe(spec, readinessFailures, 0))
		if spec.Liveness {
			c = c.WithLivenessProbe(
				httpProbe(spec, livenessFailures, livenessInitialDelaySeconds))
		}
	}

	// envFrom rather than literals: the values stay in the Secret and out of
	// the pod template.
	if len(spec.Secrets) > 0 {
		c = c.WithEnvFrom(corev1ac.EnvFromSource().
			WithSecretRef(corev1ac.SecretEnvSource().
				WithName(secretName(spec.Name))))
	}

	if spec.Port > 0 {
		c = c.WithPorts(
			corev1ac.ContainerPort().
				WithName("http").
				WithContainerPort(spec.Port).
				WithProtocol(corev1.ProtocolTCP),
		)
	}

	// Sort environment keys. Map iteration order is random in Go, and an
	// unsorted list produces a different pod template on every apply, which
	// would restart the workload each time it reconciles.
	for _, k := range slices.Sorted(maps(spec.Env)) {
		c = c.WithEnv(corev1ac.EnvVar().WithName(k).WithValue(spec.Env[k]))
	}

	res, err := resourceRequirements(spec)
	if err != nil {
		return nil, err
	}
	if res != nil {
		c = c.WithResources(res)
	}
	return c, nil
}

func resourceRequirements(
	spec orchestrator.AppSpec,
) (*corev1ac.ResourceRequirementsApplyConfiguration, error) {
	requests, err := resourceList(spec.CPURequest, spec.MemoryRequest)
	if err != nil {
		return nil, fmt.Errorf("k8s: requests for %s: %w", spec.Ref, err)
	}
	limits, err := resourceList(spec.CPULimit, spec.MemoryLimit)
	if err != nil {
		return nil, fmt.Errorf("k8s: limits for %s: %w", spec.Ref, err)
	}

	// Nothing specified: fall through to the namespace LimitRange rather than
	// pinning empty values.
	if len(requests) == 0 && len(limits) == 0 {
		return nil, nil
	}

	req := corev1ac.ResourceRequirements()
	if len(requests) > 0 {
		req = req.WithRequests(requests)
	}
	if len(limits) > 0 {
		req = req.WithLimits(limits)
	}
	return req, nil
}

func (o *Orchestrator) applyService(ctx context.Context, spec orchestrator.AppSpec) error {
	svc := corev1ac.Service(spec.Name, spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			WithSelector(orchestrator.SelectorLabels(spec.Name)).
			WithPorts(corev1ac.ServicePort().
				WithName(servicePortName(spec)).
				WithPort(exposedPort(spec)).
				WithProtocol(corev1.ProtocolTCP).
				WithTargetPort(intstr.FromInt32(spec.Port))))

	if _, err := o.client.CoreV1().Services(spec.Namespace).
		Apply(ctx, svc, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply service %s: %w", spec.Ref, err)
	}
	return nil
}

// DeleteApp removes a workload's Deployment, Service and Ingress.
func (o *Orchestrator) DeleteApp(ctx context.Context, ref orchestrator.Ref) error {
	if err := ref.Validate(); err != nil {
		return err
	}

	err := o.client.AppsV1().Deployments(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete deployment %s: %w", ref, err)
	}

	err = o.client.CoreV1().Services(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete service %s: %w", ref, err)
	}

	if err := o.deleteIngress(ctx, ref); err != nil {
		return err
	}

	if err := o.deleteSecret(ctx, ref); err != nil {
		return err
	}

	o.log.Info("app deleted", slog.String("ref", ref.String()))
	return nil
}

// AppStatus reports the observed state of a workload.
func (o *Orchestrator) AppStatus(
	ctx context.Context, ref orchestrator.Ref,
) (orchestrator.AppStatus, error) {
	if err := ref.Validate(); err != nil {
		return orchestrator.AppStatus{}, err
	}

	dep, err := o.client.AppsV1().Deployments(ref.Namespace).
		Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return orchestrator.AppStatus{}, orchestrator.ErrNotFound
		}
		return orchestrator.AppStatus{}, fmt.Errorf("k8s: get deployment %s: %w", ref, err)
	}

	var desired int32
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}

	status := orchestrator.AppStatus{
		ReleaseID:  dep.Annotations[orchestrator.AnnotationReleaseID],
		Generation: dep.Generation, ObservedGeneration: dep.Status.ObservedGeneration,
		Desired: desired, Updated: dep.Status.UpdatedReplicas,
		Ready: dep.Status.ReadyReplicas, Available: dep.Status.AvailableReplicas,
	}
	if version, err := strconv.ParseInt(
		dep.Annotations[orchestrator.AnnotationConfigVersion], 10, 64); err == nil {
		status.ConfigVersion = version
	}

	switch {
	case desired == 0:
		status.Phase = orchestrator.PhaseStopped
	case dep.Status.ReadyReplicas == 0:
		status.Phase = orchestrator.PhasePending
	case dep.Status.ReadyReplicas < desired:
		status.Phase = orchestrator.PhaseDegraded
		status.Message = fmt.Sprintf("%d/%d replicas ready",
			dep.Status.ReadyReplicas, desired)
	default:
		status.Phase = orchestrator.PhaseRunning
	}

	// Conditions, unlike replica counts, say whether a rollout is merely slow or
	// has crossed Kubernetes' own progress deadline. Preserve the reason and
	// message so the deployment attempt reports what the controller observed.
	for _, cond := range dep.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
			status.AvailableCondition = true
		}
		if (cond.Type == appsv1.DeploymentProgressing &&
			cond.Status == corev1.ConditionFalse &&
			cond.Reason == "ProgressDeadlineExceeded") ||
			(cond.Type == appsv1.DeploymentReplicaFailure && cond.Status == corev1.ConditionTrue) {
			status.Terminal = true
			status.Reason = cond.Reason
			status.Message = cond.Message
		}
		if status.Message == "" && cond.Status != corev1.ConditionTrue && cond.Message != "" {
			status.Reason = cond.Reason
			status.Message = cond.Message
		}
	}

	return status, nil
}

// specHash is a stable fingerprint of the fields that should trigger a rollout.
func specHash(spec orchestrator.AppSpec) string {
	h := sha256.New()
	fmt.Fprintf(h, "image=%s\n", spec.Image)
	fmt.Fprintf(h, "port=%d\n", spec.Port)
	fmt.Fprintf(h, "cpu=%s/%s\n", spec.CPURequest, spec.CPULimit)
	fmt.Fprintf(h, "mem=%s/%s\n", spec.MemoryRequest, spec.MemoryLimit)
	fmt.Fprintf(h, "rootfs-writable=%s\n", strconv.FormatBool(spec.WritableRootFilesystem))
	for _, k := range slices.Sorted(maps(spec.Env)) {
		fmt.Fprintf(h, "env=%s=%s\n", k, spec.Env[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// maps returns an iterator over a map's keys.
func maps[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// exposedPort is the port an app's Service listens on.
//
// Public apps get a fixed port so that ingress configuration does not change
// when the app changes its own. Nothing routes to an internal app through
// ingress, and giving it the same fixed port would mean a connection string
// naming its real port reaches a Service that is not listening there.
func exposedPort(spec orchestrator.AppSpec) int32 {
	if spec.Internal {
		return spec.Port
	}
	return servicePort
}

// servicePortName labels the port. "http" would be a lie on a database.
func servicePortName(spec orchestrator.AppSpec) string {
	if spec.Internal {
		return "tcp"
	}
	return "http"
}

// ensurePullSecret puts a pull credential in the app's own namespace.
//
// A copy per namespace rather than one shared secret, because a Secret is
// namespaced and a pod can only reference one in its own namespace. That is
// also the containment: a tenant who can read secrets in their namespace gets
// the registry credential, which is why the registry account should be one
// that can pull and push this install's images and nothing else.
func (o *Orchestrator) ensurePullSecret(ctx context.Context, spec orchestrator.AppSpec) error {
	if len(spec.RegistryAuth) == 0 {
		return nil
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      orchestrator.PullSecretName,
			Namespace: spec.Namespace,
			Labels: map[string]string{
				orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
				orchestrator.LabelOwner:     string(spec.Owner),
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: spec.RegistryAuth},
	}

	_, err := o.client.CoreV1().Secrets(spec.Namespace).Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = o.client.CoreV1().Secrets(spec.Namespace).Update(ctx, sec, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("k8s: store the pull credential in %s: %w", spec.Namespace, err)
	}
	return nil
}
