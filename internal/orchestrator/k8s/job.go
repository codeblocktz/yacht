package k8s

import (
	"context"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

var _ orchestrator.JobRunner = (*Orchestrator)(nil)

// jobLabel marks the one-off jobs the engine runs beside an app, so they can
// be told from anything else in the namespace.
const jobLabel = "yacht/job"

// jobScratchVolume is the directory a job's steps hand files through.
const jobScratchVolume = "work"

// jobSecretName is the Secret a job's own secret environment is in.
func jobSecretName(job string) string { return job + "-env" }

// RunJob starts a one-off Job in an app's namespace.
//
// Built from the same pieces as the app's own pod: the restricted security
// context, a read-only root filesystem with a writable /tmp, no service
// account token, the app's pool and region. A job is as constrained as the
// workload it works on, and admitted by the same Pod Security level.
//
// Its secret environment is a Secret of its own, owned by the Job once the
// Job exists, so deleting the Job deletes it — and a Job cleaned up by its
// TTL takes it along too.
func (o *Orchestrator) RunJob(ctx context.Context, spec orchestrator.JobSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	ns := spec.Ref.Namespace

	if _, err := o.client.BatchV1().Jobs(ns).Get(ctx, spec.Name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: read job %s/%s: %w", ns, spec.Name, err)
	}

	if spec.Volume != nil {
		if err := o.applyVolumes(ctx, orchestrator.AppSpec{
			Ref: spec.Ref, Volumes: []orchestrator.VolumeSpec{*spec.Volume},
		}); err != nil {
			return err
		}
	}

	labels := map[string]string{
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		orchestrator.LabelOwner:     string(spec.Ref.Owner),
		orchestrator.LabelName:      spec.Ref.Name,
		jobLabel:                    "true",
	}

	if len(spec.SecretEnv) > 0 {
		sec := corev1ac.Secret(jobSecretName(spec.Name), ns).
			WithLabels(labels).
			WithType(corev1.SecretTypeOpaque).
			WithStringData(spec.SecretEnv)
		if _, err := o.client.CoreV1().Secrets(ns).Apply(ctx, sec, applyOpts()); err != nil {
			return fmt.Errorf("k8s: store the job's secrets: %w", err)
		}
	}

	job, err := o.client.BatchV1().Jobs(ns).Apply(ctx, jobApply(spec, labels), applyOpts())
	if err != nil {
		if len(spec.SecretEnv) > 0 {
			_ = o.client.CoreV1().Secrets(ns).Delete(ctx, jobSecretName(spec.Name), metav1.DeleteOptions{})
		}
		return fmt.Errorf("k8s: start job %s/%s: %w", ns, spec.Name, err)
	}

	if len(spec.SecretEnv) > 0 {
		owner := metav1ac.OwnerReference().
			WithAPIVersion("batch/v1").WithKind("Job").
			WithName(job.Name).WithUID(job.UID)
		sec := corev1ac.Secret(jobSecretName(spec.Name), ns).
			WithLabels(labels).
			WithType(corev1.SecretTypeOpaque).
			WithStringData(spec.SecretEnv).
			WithOwnerReferences(owner)
		// Best effort: the Job runs either way, and DeleteJob removes the
		// Secret by name as well.
		_, _ = o.client.CoreV1().Secrets(ns).Apply(ctx, sec, applyOpts())
	}
	return nil
}

func jobApply(spec orchestrator.JobSpec, labels map[string]string) *batchv1ac.JobApplyConfiguration {
	uid := spec.RunAsUser

	env := make([]*corev1ac.EnvVarApplyConfiguration, 0, len(spec.Env)+len(spec.AppSecretEnv))
	for _, k := range slices.Sorted(maps(spec.Env)) {
		env = append(env, corev1ac.EnvVar().WithName(k).WithValue(spec.Env[k]))
	}
	for _, k := range slices.Sorted(maps(spec.AppSecretEnv)) {
		env = append(env, corev1ac.EnvVar().WithName(k).WithValueFrom(
			corev1ac.EnvVarSource().WithSecretKeyRef(corev1ac.SecretKeySelector().
				WithName(secretName(spec.Ref.Name)).WithKey(spec.AppSecretEnv[k]))))
	}

	mounts := []*corev1ac.VolumeMountApplyConfiguration{
		corev1ac.VolumeMount().WithName(tmpVolumeName).WithMountPath("/tmp"),
	}
	volumes := []*corev1ac.VolumeApplyConfiguration{
		corev1ac.Volume().WithName(tmpVolumeName).WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
	}
	if spec.ScratchPath != "" {
		mounts = append(mounts, corev1ac.VolumeMount().WithName(jobScratchVolume).WithMountPath(spec.ScratchPath))
		volumes = append(volumes, corev1ac.Volume().WithName(jobScratchVolume).
			WithEmptyDir(corev1ac.EmptyDirVolumeSource()))
	}
	if v := spec.Volume; v != nil {
		mounts = append(mounts, corev1ac.VolumeMount().WithName(v.Name).WithMountPath(v.MountPath))
		volumes = append(volumes, corev1ac.Volume().WithName(v.Name).
			WithPersistentVolumeClaim(corev1ac.PersistentVolumeClaimVolumeSource().
				WithClaimName(claimName(spec.Ref.Name, v.Name))))
	}

	containers := make([]*corev1ac.ContainerApplyConfiguration, 0, len(spec.Steps))
	for _, st := range spec.Steps {
		c := corev1ac.Container().
			WithName(st.Name).
			WithImage(st.Image).
			WithImagePullPolicy(corev1.PullIfNotPresent).
			WithCommand(st.Command...).
			WithSecurityContext(restrictedContainerSecurityContext(false).
				WithRunAsUser(uid).WithRunAsGroup(uid)).
			// The end of a failed step's output is its termination message,
			// which is where JobState reads why it failed: the job's own
			// condition says only that it did.
			WithTerminationMessagePolicy(corev1.TerminationMessageFallbackToLogsOnError).
			WithEnv(env...).
			WithVolumeMounts(mounts...)
		if len(spec.SecretEnv) > 0 {
			c = c.WithEnvFrom(corev1ac.EnvFromSource().
				WithSecretRef(corev1ac.SecretEnvSource().WithName(jobSecretName(spec.Name))))
		}
		containers = append(containers, c)
	}

	pod := corev1ac.PodSpec().
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithAutomountServiceAccountToken(false).
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithRunAsNonRoot(true).
			WithRunAsUser(uid).WithRunAsGroup(uid).WithFSGroup(uid).
			WithSeccompProfile(corev1ac.SeccompProfile().
				WithType(corev1.SeccompProfileTypeRuntimeDefault))).
		WithImagePullSecrets(corev1ac.LocalObjectReference().WithName(orchestrator.PullSecretName)).
		WithVolumes(volumes...)
	// Init containers run in order and each must succeed before the next
	// starts, which is the sequence the steps are; the last is the job's
	// container proper.
	if len(containers) > 1 {
		pod = pod.WithInitContainers(containers[:len(containers)-1]...)
	}
	pod = pod.WithContainers(containers[len(containers)-1])
	pod = placeInPool(pod, spec.Pool)
	pod = placeInRegion(pod, spec.Region)

	// The pod is not the engine's workload: without the managed-by label it
	// is left out of the app's pods, of what a waking app waits for, and of
	// what retiring a node moves. And without the app's selector label it is
	// not behind the app's Service.
	podLabels := map[string]string{
		orchestrator.LabelOwner: labels[orchestrator.LabelOwner],
		orchestrator.LabelName:  labels[orchestrator.LabelName],
		jobLabel:                "true",
	}

	// Never retried. A dump that failed fails again the same way, and a
	// restore retried after a partial failure is the one outcome worse than
	// the failure itself. A day after it ends, the Job goes on its own; the
	// engine records the outcome long before that and deletes it sooner.
	var backoff int32
	ttl := int32(24 * 60 * 60)
	js := batchv1ac.JobSpec().
		WithBackoffLimit(backoff).
		WithTTLSecondsAfterFinished(ttl).
		WithTemplate(corev1ac.PodTemplateSpec().WithLabels(podLabels).WithSpec(pod))
	if spec.Deadline > 0 {
		js = js.WithActiveDeadlineSeconds(int64(spec.Deadline.Seconds()))
	}
	return batchv1ac.Job(spec.Name, spec.Ref.Namespace).WithLabels(labels).WithSpec(js)
}

// JobState reads a job's outcome from the cluster.
func (o *Orchestrator) JobState(ctx context.Context, namespace, name string) (orchestrator.JobState, error) {
	job, err := o.client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return orchestrator.JobState{}, nil
		}
		return orchestrator.JobState{}, fmt.Errorf("k8s: read job %s/%s: %w", namespace, name, err)
	}
	state := orchestrator.JobState{Found: true}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			state.Done = true
		case batchv1.JobFailed:
			state.Done, state.Failed = true, true
			state.Reason = c.Message
			if state.Reason == "" {
				state.Reason = c.Reason
			}
		}
	}
	if !state.Done {
		return state, nil
	}
	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + name,
	})
	if err != nil {
		return state, nil
	}
	for _, p := range pods.Items {
		steps := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...),
			p.Status.ContainerStatuses...)
		for _, cs := range steps {
			t := cs.State.Terminated
			if t == nil {
				continue
			}
			if state.Failed && t.ExitCode != 0 {
				msg := strings.TrimSpace(t.Message)
				if msg == "" {
					msg = t.Reason
				}
				state.Reason = fmt.Sprintf("the %s step exited %d: %s", cs.Name, t.ExitCode, lastLines(msg, 6))
				return state, nil
			}
			if !state.Failed {
				state.Message = strings.TrimSpace(t.Message)
			}
		}
	}
	return state, nil
}

// lastLines keeps the end of a log, where a program says why it stopped.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// DeleteJob removes a job, its pods, and its Secret.
func (o *Orchestrator) DeleteJob(ctx context.Context, namespace, name string) error {
	policy := metav1.DeletePropagationBackground
	err := o.client.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete job %s/%s: %w", namespace, name, err)
	}
	err = o.client.CoreV1().Secrets(namespace).Delete(ctx, jobSecretName(name), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete job secret %s/%s: %w", namespace, name, err)
	}
	return nil
}
