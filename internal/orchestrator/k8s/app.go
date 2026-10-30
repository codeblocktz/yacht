package k8s

import (
	"cmp"
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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
const servicePort = orchestrator.PublicServicePort

// deploymentProgressDeadline is also the upper bound used by the application
// worker. Setting it on the object lets the Deployment controller supply the
// useful ProgressDeadlineExceeded reason instead of Yacht inventing one from
// replica counts alone.
const deploymentProgressDeadline int32 = 600

// deploymentMinReadySeconds is how long a new pod must stay ready before it
// counts as available, and so before the rolling update stops an old one in
// its place. A container that dies the moment it starts is ready for an
// instant, between starting and crashing; without this the old version was
// stopped on the strength of that instant, and the deploy passed with
// nothing left serving. Not in the pod template, so setting it rolls nothing.
const deploymentMinReadySeconds int32 = 5

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

	// A workload on a schedule is a CronJob, and everything that makes an
	// always-running one reachable goes.
	if spec.Schedule != "" {
		return o.applyScheduled(ctx, spec)
	}

	// Going to sleep, the route moves to the waker before the pods go away,
	// so a request arriving in between is held by the waker rather than
	// answered by a controller with nothing to send it to.
	if spec.Waker != nil {
		if err := o.applyWakerRoute(ctx, spec); err != nil {
			return err
		}
		if err := o.applyIngress(ctx, spec); err != nil {
			return err
		}
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

	// Applied or pruned on every apply, like the Service: turning a proxy off
	// has to take the port off every machine, not merely stop renewing it.
	if err := o.applyTCPProxy(ctx, spec); err != nil {
		return err
	}

	switch {
	case spec.Waker != nil:
		// Already pointed at the waker, above.
	case spec.Port > 0 && len(spec.Hosts) > 0:
		if err := o.applyIngress(ctx, spec); err != nil {
			return err
		}
	default:
		if err := o.deleteIngress(ctx, spec.Ref); err != nil {
			return err
		}
	}

	// Awake, the waker's route goes once the Ingress no longer names it.
	if spec.Waker == nil {
		if err := o.deleteWakerRoute(ctx, spec.Ref); err != nil {
			return err
		}
	}

	// A workload that ran on a schedule and no longer does: its CronJob
	// goes once the Deployment replacing it is in place, so the switch never
	// leaves it with neither.
	if err := o.deleteCronJob(ctx, spec.Ref); err != nil {
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
	// The pod template carries what the pods are given and nothing else, so
	// that only a change to it starts new ones. Not the config version: that
	// also moves for what lives outside the pods — a domain, the install's
	// CNAME target, a TCP proxy — and a restart for those is downtime for a
	// database and a dropped connection for everything else.
	podAnnotations := map[string]string{
		orchestrator.AnnotationRevision:  specHash(spec),
		orchestrator.AnnotationReleaseID: spec.ReleaseID,
	}
	if len(spec.Secrets) > 0 {
		// Secret values reach the pods through a Secret, so the template has
		// to change when they do or the pods keep the old ones.
		podAnnotations[orchestrator.AnnotationSecrets] = secretsHash(spec.Secrets)
	}
	podReleaseID, err := o.podReleaseID(ctx, spec, podAnnotations)
	if err != nil {
		return err
	}
	podAnnotations[orchestrator.AnnotationReleaseID] = podReleaseID
	pendingAnnotations := map[string]string{
		orchestrator.AnnotationRevision:      specHash(spec),
		orchestrator.AnnotationReleaseID:     "",
		orchestrator.AnnotationConfigVersion: "-1",
		// On the Deployment rather than its pods, so changing the policy
		// restarts nothing: it decides how AppStatus reads a crash, which is
		// read from here.
		annotationCrashLimit: strconv.Itoa(int(spec.CrashLimit())),
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
	podSpec = placeInPool(podSpec, spec.Pool)
	podSpec = placeInRegion(podSpec, spec.Region)

	// A rolling update starts the replacement before the original stops. With a
	// ReadWriteOnce claim the replacement cannot mount what the original still
	// holds, so it waits forever and the deploy never completes — no error, no
	// timeout, just a pod that never becomes ready.
	//
	// Recreating costs downtime on every deploy, which is why it is applied
	// only when there is storage to justify it, and said out loud in the
	// dashboard rather than changed underneath the operator.
	//
	// Rolling means one extra pod and none missing. Kubernetes' default is a
	// quarter of each, which rounds to the same thing for one to three replicas
	// and from four lets one go unready — and the replacement being ready before the
	// original stops is the whole promise of a restart that moves an app off a
	// machine being retired, so it is stated rather than left to rounding. One
	// surge pod rather than a quarter is also the least spare room a rollout
	// can need, which on a cluster of one small machine is the room there is.
	strategy := appsv1ac.DeploymentStrategy().
		WithType(appsv1.RollingUpdateDeploymentStrategyType).
		WithRollingUpdate(appsv1ac.RollingUpdateDeployment().
			WithMaxSurge(intstr.FromInt32(1)).
			WithMaxUnavailable(intstr.FromInt32(0)))
	if len(spec.Volumes) > 0 {
		strategy = appsv1ac.DeploymentStrategy().
			WithType(appsv1.RecreateDeploymentStrategyType)
	} else {
		podSpec.WithTopologySpreadConstraints(o.spreadConstraints(ctx, spec)...)
	}

	dep := appsv1ac.Deployment(spec.Name, spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithAnnotations(pendingAnnotations).
		WithSpec(appsv1ac.DeploymentSpec().
			WithStrategy(strategy).
			WithProgressDeadlineSeconds(progressDeadline(spec)).
			WithMinReadySeconds(deploymentMinReadySeconds).
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

// podReleaseID is the release the pod template is stamped with: this one,
// unless all this release changes is how many pods there are.
//
// Scaling is a release, because a release records its replica count and a
// rollback restores it. Stamping the template with it, though, makes a new
// ReplicaSet, and Kubernetes then replaces every running pod to add one: a
// scale from five to six dropped five pods' connections. So a release whose
// pods would be what is already running, at a different count, keeps the
// stamp the running pods have, and only the count changes.
//
// The same count is not carried, on purpose. A release that changes nothing
// at all is a Redeploy, and restarting the pods is what it is for.
func (o *Orchestrator) podReleaseID(
	ctx context.Context, spec orchestrator.AppSpec, podAnnotations map[string]string,
) (string, error) {
	cur, err := o.client.AppsV1().Deployments(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return spec.ReleaseID, nil
	}
	if err != nil {
		return "", fmt.Errorf("k8s: get deployment %s: %w", spec.Ref, err)
	}
	running := cur.Spec.Template.Annotations
	stamp := running[orchestrator.AnnotationReleaseID]
	if stamp == "" || cur.Spec.Replicas == nil || *cur.Spec.Replicas == spec.Replicas ||
		running[orchestrator.AnnotationRevision] != podAnnotations[orchestrator.AnnotationRevision] ||
		running[orchestrator.AnnotationSecrets] != podAnnotations[orchestrator.AnnotationSecrets] {
		return spec.ReleaseID, nil
	}
	return stamp, nil
}

// placeInPool keeps a workload's pods on its pool's nodes: a node selector on
// the pool's label, which is what places them, and a toleration of the pool's
// taint, which is what lets them onto a node dedicated to the pool. Nothing
// for no pool.
//
// Both rather than either. The selector alone could not use a dedicated node,
// and the toleration alone would only permit the pool, never require it — the
// pods would land wherever there was room, the pool's nodes among them.
//
// Removing the pool removes both: they are owned by this field manager, so an
// apply that omits them takes them off, and the pods roll back to anywhere.
func placeInPool(pod *corev1ac.PodSpecApplyConfiguration, pool string) *corev1ac.PodSpecApplyConfiguration {
	if pool == "" {
		return pod
	}
	return pod.
		WithNodeSelector(map[string]string{orchestrator.PoolLabel: pool}).
		WithTolerations(corev1ac.Toleration().
			WithKey(orchestrator.PoolLabel).
			WithOperator(corev1.TolerationOpEqual).
			WithValue(pool).
			WithEffect(corev1.TaintEffectNoSchedule))
}

// placeInRegion requires a workload's pods to run on its region's nodes: a
// required node affinity on the region label. Nothing for no region.
//
// Required rather than preferred. A preference would let a full region spill
// an app into another one, quietly moving its traffic and its data across the
// world; a pod that stays Pending, with the scheduler saying why, is the
// honest result and the one an operator can act on.
//
// An affinity rather than a second node selector entry, so the region and the
// pool stay separate fields with separate owners. The scheduler requires both:
// a node selector and a required node affinity must each be satisfied.
//
// Removing the region removes the affinity, like the pool: it is owned by this
// field manager, and an apply that omits it takes it off.
func placeInRegion(pod *corev1ac.PodSpecApplyConfiguration, region string) *corev1ac.PodSpecApplyConfiguration {
	if region == "" {
		return pod
	}
	return pod.WithAffinity(corev1ac.Affinity().
		WithNodeAffinity(corev1ac.NodeAffinity().
			WithRequiredDuringSchedulingIgnoredDuringExecution(corev1ac.NodeSelector().
				WithNodeSelectorTerms(corev1ac.NodeSelectorTerm().
					WithMatchExpressions(corev1ac.NodeSelectorRequirement().
						WithKey(orchestrator.RegionLabel).
						WithOperator(corev1.NodeSelectorOpIn).
						WithValues(region))))))
}

// spreadConstraints asks the scheduler to keep an app's replicas apart: on
// different machines, and on different sites once the install has any, so
// losing one machine — or every machine in one place — takes some of an app's
// replicas rather than all of them.
//
// Both are preferences (ScheduleAnyway), never requirements. An install on one
// machine, or a site that is full, still runs every replica; it just cannot
// keep them apart. Apps with volumes get none: they run one pod, which has to
// go where its volume can be mounted, so there is nothing to spread — and a
// constraint in the template would only restart the app, with the gap Recreate
// means, on its next apply.
//
// The site constraint is only added once some node carries a site. A node
// without every key a pod's constraints name is left out of spreading
// altogether, so a site constraint on an install that has none would switch
// off the machine spreading too. It does mean the pod template changes when
// the first site appears, and each app rolls onto it at its next apply — one
// surge pod at a time, as any rollout.
//
// matchLabelKeys keeps a rolling update from counting the pods it is about to
// replace, which would otherwise pull the new ones toward wherever the old
// ones are not.
func (o *Orchestrator) spreadConstraints(
	ctx context.Context, spec orchestrator.AppSpec,
) []*corev1ac.TopologySpreadConstraintApplyConfiguration {
	spread := func(key string) *corev1ac.TopologySpreadConstraintApplyConfiguration {
		return corev1ac.TopologySpreadConstraint().
			WithMaxSkew(1).
			WithTopologyKey(key).
			WithWhenUnsatisfiable(corev1.ScheduleAnyway).
			WithLabelSelector(metav1ac.LabelSelector().
				WithMatchLabels(orchestrator.SelectorLabels(spec.Name))).
			WithMatchLabelKeys(appsv1.DefaultDeploymentUniqueLabelKey)
	}
	constraints := []*corev1ac.TopologySpreadConstraintApplyConfiguration{
		spread(corev1.LabelHostname),
	}
	if o.hasSites(ctx) {
		constraints = append(constraints, spread(corev1.LabelTopologyZone))
	}
	return constraints
}

// hasSites reports whether any node carries a site.
//
// A cluster that cannot be read is treated as having none rather than failing
// the deploy: the answer only decides a preference, and an app that deploys
// spread by machine alone is better than one that does not deploy.
func (o *Orchestrator) hasSites(ctx context.Context) bool {
	list, err := o.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: corev1.LabelTopologyZone, Limit: 1,
	})
	if err != nil {
		o.log.Warn("could not read node sites; spreading by machine only",
			slog.String("error", err.Error()))
		return false
	}
	return len(list.Items) > 0
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

	// A start command replaces the image's ENTRYPOINT and CMD both: with a
	// command and no args, Kubernetes runs exactly the command. Left off
	// when empty, rather than set to nothing, so the image decides — and so
	// clearing one hands the container back to its image on the next apply,
	// since this field manager owns the field and an apply without it
	// removes it.
	if cmd := startCommand(spec); cmd != nil {
		c = c.WithCommand(cmd...)
	}
	if len(spec.Args) > 0 {
		c = c.WithArgs(spec.Args...)
	}

	// Readiness first: it is what keeps traffic away from a container that is
	// not serving yet, which is the whole of what most workloads need.
	if spec.HealthPath != "" {
		c = c.WithReadinessProbe(httpProbe(spec, readinessFailures, 0))
		if spec.Liveness {
			c = c.WithLivenessProbe(
				httpProbe(spec, livenessFailures, livenessInitialDelaySeconds))
		}
	} else if spec.Port > 0 {
		// No health check, and a port traffic is sent to: ready is listening
		// on it. Without any probe a container is ready the instant it
		// starts, before its process has opened its port, or between
		// starting and crashing. A deploy was passed on that, and its old
		// version stopped for it, and a woken app was handed requests it
		// refused.
		c = c.WithReadinessProbe(tcpProbe(spec))
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
	// PORT is the port traffic is sent to, as Heroku and Railway set it:
	// Phoenix, Rails, Express and most frameworks listen where it says. Not
	// when the app sets PORT itself, as a variable or a secret — a literal
	// here would win over the secret's.
	if port, ok := portEnv(spec); ok {
		c = c.WithEnv(corev1ac.EnvVar().WithName("PORT").WithValue(port))
	}

	res, err := resourceRequirements(spec)
	if err != nil {
		return nil, err
	}
	c = c.WithResources(res)
	return c, nil
}

func resourceRequirements(
	spec orchestrator.AppSpec,
) (*corev1ac.ResourceRequirementsApplyConfiguration, error) {
	d := orchestrator.DefaultLimits
	cpuRequest, cpuLimit := effective(spec.CPURequest, spec.CPULimit, d.DefaultCPURequest, d.DefaultCPU)
	memRequest, memLimit := effective(spec.MemoryRequest, spec.MemoryLimit, d.DefaultMemoryRequest, d.DefaultMemory)

	requests, err := resourceList(cpuRequest, memRequest)
	if err != nil {
		return nil, fmt.Errorf("k8s: requests for %s: %w", spec.Ref, err)
	}
	limits, err := resourceList(cpuLimit, memLimit)
	if err != nil {
		return nil, fmt.Errorf("k8s: limits for %s: %w", spec.Ref, err)
	}
	return corev1ac.ResourceRequirements().WithRequests(requests).WithLimits(limits), nil
}

// effective is what a container asking for request and limit (either possibly
// empty) is given: the setting where there is one, the default where not.
//
// Written onto the pod rather than left to the namespace LimitRange, for two
// reasons. A LimitRange is read when a pod is admitted, so a namespace made
// before the defaults changed would go on handing out the old ones; and the
// pod is then the one place that says what the app has, which the dashboard
// repeats rather than guesses at.
//
// A request above the default limit lifts the limit to it, and a default
// request above the limit comes down to it: Kubernetes refuses a pod whose
// request is above its limit, and nobody chose the half that was defaulted.
func effective(request, limit, defRequest, defLimit string) (string, string) {
	switch {
	case request == "" && limit == "":
		return defRequest, defLimit
	case limit == "":
		return request, larger(request, defLimit)
	case request == "":
		return smaller(defRequest, limit), limit
	}
	return request, limit
}

// larger and smaller compare two quantity strings and return one as written.
// One that does not parse is kept, for resourceList to refuse by name.
func larger(a, b string) string {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil || qa.Cmp(qb) >= 0 {
		return a
	}
	return b
}

func smaller(a, b string) string {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil || qa.Cmp(qb) <= 0 {
		return a
	}
	return b
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

	if err := o.deleteCronJob(ctx, ref); err != nil {
		return err
	}

	if err := o.deleteTCPProxy(ctx, ref); err != nil {
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
			// No Deployment is also what a workload on a schedule has.
			return o.scheduledStatus(ctx, ref)
		}
		return orchestrator.AppStatus{}, fmt.Errorf("k8s: get deployment %s: %w", ref, err)
	}

	status := deploymentStatus(dep)
	if needsPodCheck(status) {
		pods, err := o.client.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(orchestrator.ObjectLabels(ref)).String(),
		})
		if err == nil {
			applyPods(&status, pods.Items, podRelease(dep, status), crashLimit(dep.Annotations))
		}
	}
	return finishStatus(status), nil
}

// deploymentStatus reads a workload's state from its Deployment, before its
// pods are looked at: see needsPodCheck.
func deploymentStatus(dep *appsv1.Deployment) orchestrator.AppStatus {
	var desired int32
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}

	status := orchestrator.AppStatus{
		ReleaseID:  dep.Annotations[orchestrator.AnnotationReleaseID],
		Generation: dep.Generation, ObservedGeneration: dep.Status.ObservedGeneration,
		Desired: desired, Pods: dep.Status.Replicas, Updated: dep.Status.UpdatedReplicas,
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

	return status
}

// needsPodCheck is whether a workload's pods have to be read to know its
// state: any with a release, whose pods say how often it has restarted and
// whether its container cannot start. Not only one short of ready: ready and
// available count an old revision's pods too, so one still serving hid a new
// one crash-looping, and a container that crashes on every start is ready
// for a moment each time. Read for AppStatus and AppStatuses alike, so the
// two agree.
func needsPodCheck(status orchestrator.AppStatus) bool {
	return status.ReleaseID != ""
}

// applyPods completes a status from its workload's pods, those stamped with
// podRelease (see podRelease): how often they have restarted and how the
// last restart ended, which a deploy watches its new version for; and a
// container that cannot start, which fails the rollout now rather than at
// the progress deadline ten minutes on: its start script not executable,
// its config missing, a crash on every start. An old revision's pods are
// not looked at; their trouble is not this deploy's.
func applyPods(status *orchestrator.AppStatus, pods []corev1.Pod, podRelease string, limit int32) {
	status.Restarts, status.LastExit = restarts(releasePods(pods, podRelease))
	if status.Terminal {
		return
	}
	if reason, message, ok := unstartable(pods, podRelease, limit); ok {
		status.Terminal = true
		status.Reason, status.Message = reason, message
	}
}

// releasePods is those of pods stamped with a release.
func releasePods(pods []corev1.Pod, releaseID string) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.Annotations[orchestrator.AnnotationReleaseID] == releaseID {
			out = append(out, p)
		}
	}
	return out
}

// restarts totals pods' container restarts, and says how the latest of them
// ended.
func restarts(pods []corev1.Pod) (int32, string) {
	var n int32
	var last *corev1.ContainerStateTerminated
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			n += cs.RestartCount
			t := cs.LastTerminationState.Terminated
			if t != nil && (last == nil || t.FinishedAt.After(last.FinishedAt.Time)) {
				last = t
			}
		}
	}
	if last == nil {
		return n, ""
	}
	return n, fmt.Sprintf("code %d (%s)", last.ExitCode, cmp.Or(last.Reason, "no reason given"))
}

// podRelease is the release a Deployment's pods are stamped with: its
// template's, which a scale leaves as the release that made the pods rather
// than the scale's own (see podReleaseID). The Deployment's release, for one
// written before the template carried it.
func podRelease(dep *appsv1.Deployment, status orchestrator.AppStatus) string {
	return cmp.Or(dep.Spec.Template.Annotations[orchestrator.AnnotationReleaseID], status.ReleaseID)
}

// finishStatus settles the phase once everything is known: a rollout that
// has failed is failing, not merely pending.
func finishStatus(status orchestrator.AppStatus) orchestrator.AppStatus {
	if status.Terminal && status.Phase == orchestrator.PhasePending {
		status.Phase = orchestrator.PhaseFailing
	}
	return status
}

// crashesBeforeFailing is how many times a release's container may crash
// and be restarted before its rollout is given up, for a Deployment written
// before its restart policy was recorded on it: the default policy's.
const crashesBeforeFailing = orchestrator.DefaultMaxRestarts

// annotationCrashLimit records a workload's restart policy, as the number of
// restarts its container may have before it is failing; zero is no limit.
const annotationCrashLimit = "yacht/crash-limit"

// crashLimit reads it back.
func crashLimit(annotations map[string]string) int32 {
	n, err := strconv.ParseInt(annotations[annotationCrashLimit], 10, 32)
	if err != nil || n < 0 {
		return crashesBeforeFailing
	}
	return int32(n)
}

// progressDeadline is how long Kubernetes lets a rollout go without progress
// before it reports ProgressDeadlineExceeded: the app's healthcheck timeout
// when it has one, so the cluster gives up when the deploy does and says why
// in its own words, and the engine's budget otherwise.
func progressDeadline(spec orchestrator.AppSpec) int32 {
	if spec.HealthTimeoutSeconds > 0 {
		return spec.HealthTimeoutSeconds
	}
	return deploymentProgressDeadline
}

// unstartable reports why a release's container cannot start, if it
// cannot: a reason Kubernetes will not get past by trying again, or more
// crashes than the restart policy allows.
func unstartable(pods []corev1.Pod, releaseID string, limit int32) (string, string, bool) {
	for _, p := range pods {
		if p.Annotations[orchestrator.AnnotationReleaseID] != releaseID {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if reason, message, ok := containerCannotStart(cs, limit); ok {
				return reason, message, true
			}
		}
	}
	return "", "", false
}

// containerCannotStart reads one container's state for a failure that
// trying again will not cure, or for more crashes than limit allows. A limit
// of zero is the "always" policy: crashing is never what fails it. One is
// "never": the first crash does, caught as the container exits as well as
// while it waits to be restarted.
func containerCannotStart(cs corev1.ContainerStatus, limit int32) (string, string, bool) {
	if t := cs.State.Terminated; limit == 1 && t != nil && t.ExitCode != 0 {
		return "Crashed", fmt.Sprintf("the container exited with code %d (%s), and the restart policy is never",
			t.ExitCode, cmp.Or(t.Reason, "no reason given")), true
	}
	w := cs.State.Waiting
	if w == nil {
		return "", "", false
	}
	switch w.Reason {
	case "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "InvalidImageName":
		return w.Reason, w.Message, true
	case "CrashLoopBackOff":
		last := cs.LastTerminationState.Terminated
		// Never started at all, which no restart changes.
		if last != nil && last.Reason == "StartError" {
			return "StartError", last.Message, true
		}
		if limit > 0 && (cs.RestartCount >= limit || limit == 1) {
			if last != nil {
				return "CrashLoopBackOff", fmt.Sprintf("the container exited %d times; the last time with code %d (%s)",
					cs.RestartCount, last.ExitCode, cmp.Or(last.Reason, "no reason given")), true
			}
			return "CrashLoopBackOff", fmt.Sprintf("the container exited %d times", cs.RestartCount), true
		}
	}
	return "", "", false
}

// specHash is a stable fingerprint of the fields that should trigger a rollout.
func specHash(spec orchestrator.AppSpec) string {
	h := sha256.New()
	fmt.Fprintf(h, "image=%s\n", spec.Image)
	fmt.Fprintf(h, "port=%d\n", spec.Port)
	fmt.Fprintf(h, "cpu=%s/%s\n", spec.CPURequest, spec.CPULimit)
	fmt.Fprintf(h, "mem=%s/%s\n", spec.MemoryRequest, spec.MemoryLimit)
	fmt.Fprintf(h, "rootfs-writable=%s\n", strconv.FormatBool(spec.WritableRootFilesystem))
	// Only when set, so every workload that has never had a pool keeps the
	// fingerprint it had before pools existed and is not rolled for nothing.
	if spec.Pool != "" {
		fmt.Fprintf(h, "pool=%s\n", spec.Pool)
	}
	// The same for a region: an install that has none, and every app recorded
	// before regions existed, keeps its fingerprint.
	if spec.Region != "" {
		fmt.Fprintf(h, "region=%s\n", spec.Region)
	}
	// And a start command: every app that has never set one keeps the
	// fingerprint it had before there was one to set.
	if spec.StartCommand != "" {
		fmt.Fprintf(h, "start=%s\n", spec.StartCommand)
	}
	// Not the restart policy or the healthcheck timeout: neither is in the pod
	// template, and changing them is no reason to restart anything.
	// Only when set, like the pool: every workload started with its image's
	// own command keeps the fingerprint it had before arguments existed.
	if len(spec.Args) > 0 {
		fmt.Fprintf(h, "args=%q\n", spec.Args)
	}
	for _, k := range slices.Sorted(maps(spec.Env)) {
		fmt.Fprintf(h, "env=%s=%s\n", k, spec.Env[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// secretsHash fingerprints an app's secret values: every key and value
// together, so that no one value can be guessed from it.
func secretsHash(secrets map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps(secrets)) {
		fmt.Fprintf(h, "%q=%q\n", k, secrets[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// startCommand is the container command for a start command, nil for none.
//
// Through a shell, because a start command is typed the way it would be at a
// prompt — "bin/server --port $PORT", "npm run start && echo done" — and only
// a shell splits words, expands variables and runs the second half of an &&.
// The cost is that an image with no /bin/sh cannot have one, which is the same
// trade every platform with this setting makes.
func startCommand(spec orchestrator.AppSpec) []string {
	if spec.StartCommand == "" {
		return nil
	}
	return shellCommand(spec.StartCommand)
}

// shellCommand runs a command line through /bin/sh.
func shellCommand(line string) []string { return []string{"/bin/sh", "-c", line} }

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
	return orchestrator.ServicePort(spec.Internal, spec.Port)
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

// portEnv is the PORT an app is given: its port, unless it has none or sets
// PORT itself.
func portEnv(spec orchestrator.AppSpec) (string, bool) {
	if spec.Port <= 0 {
		return "", false
	}
	if _, ok := spec.Env["PORT"]; ok {
		return "", false
	}
	if _, ok := spec.Secrets["PORT"]; ok {
		return "", false
	}
	return strconv.Itoa(int(spec.Port)), true
}
