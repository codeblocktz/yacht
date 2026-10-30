package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// scheduleTimeZone is the zone every schedule is read in. Set on the CronJob
// rather than left to the controller, whose own zone is whatever its machine
// was installed with: the same expression would otherwise mean a different
// hour on every install, and the dashboard could not say when the next run is.
const scheduleTimeZone = "Etc/UTC"

// maxCronJobName is the longest name a CronJob may have. Its Jobs are named
// after it with a timestamp appended, and those must still be legal labels.
const maxCronJobName = 52

// applyScheduled converges a workload that runs on a schedule: a CronJob in
// place of the Deployment, and nothing routing to it.
//
// The order is the Deployment's in reverse. The CronJob first, so there is
// never a moment with neither; then the always-running workload and its
// routes go; and the convergence marker last, on the CronJob, so a failure
// part-way stays visibly unconverged and the reconciler applies it all again.
func (o *Orchestrator) applyScheduled(ctx context.Context, spec orchestrator.AppSpec) error {
	if len(spec.Name) > maxCronJobName {
		return fmt.Errorf("k8s: %s cannot run on a schedule — its name is longer than %d characters",
			spec.Ref, maxCronJobName)
	}
	if err := o.ensurePullSecret(ctx, spec); err != nil {
		return err
	}
	cj, err := o.cronJob(spec)
	if err != nil {
		return err
	}
	if _, err := o.client.BatchV1().CronJobs(spec.Namespace).Apply(ctx, cj, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply cron job %s: %w", spec.Ref, err)
	}

	policy := metav1.DeletePropagationBackground
	err = o.client.AppsV1().Deployments(spec.Namespace).
		Delete(ctx, spec.Name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete deployment %s: %w", spec.Ref, err)
	}
	if err := o.deleteService(ctx, spec.Ref); err != nil {
		return err
	}
	if err := o.deleteIngress(ctx, spec.Ref); err != nil {
		return err
	}
	if err := o.deleteWakerRoute(ctx, spec.Ref); err != nil {
		return err
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			orchestrator.AnnotationReleaseID:     spec.ReleaseID,
			orchestrator.AnnotationConfigVersion: strconv.FormatInt(spec.ConfigVersion, 10),
		}},
	})
	if err != nil {
		return fmt.Errorf("k8s: encode convergence marker: %w", err)
	}
	if _, err := o.client.BatchV1().CronJobs(spec.Namespace).Patch(
		ctx, spec.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("k8s: mark cron job %s converged: %w", spec.Ref, err)
	}
	return nil
}

// cronJob describes a workload on a schedule.
//
// Each run is the app's own container — its image, its start command, its
// environment and Secret, its user, limits, storage and placement — without
// what only a server needs: no probes, since a run that is not serving is not
// unready, it is working.
func (o *Orchestrator) cronJob(spec orchestrator.AppSpec) (*batchv1ac.CronJobApplyConfiguration, error) {
	run := spec
	run.HealthPath, run.Liveness = "", false
	container, err := o.container(run)
	if err != nil {
		return nil, err
	}
	// Nor the readiness a server gets without a health check: a run that
	// listens on nothing is not unready either.
	container.ReadinessProbe = nil

	// Retried by a new pod rather than restarted in place, so each attempt
	// keeps a log of its own to read. See orchestrator.RestartPolicy for why
	// "always" retries as "on failure" does here.
	backoff := spec.MaxRestarts
	switch {
	case spec.RestartPolicy == orchestrator.RestartNever:
		backoff = 0
	case backoff == 0:
		backoff = orchestrator.DefaultMaxRestarts
	}

	podSpec := corev1ac.PodSpec().
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithAutomountServiceAccountToken(false).
		WithSecurityContext(restrictedPodSecurityContext(spec)).
		WithContainers(container).
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

	// The app's labels on each run's pod, so its logs are where the app's
	// are. Safe here as it would not be on a pre-deploy run: a scheduled
	// workload has no Service to send them traffic.
	labels := orchestrator.ObjectLabels(spec.Ref)
	return batchv1ac.CronJob(spec.Name, spec.Namespace).
		WithLabels(labels).
		WithAnnotations(map[string]string{
			orchestrator.AnnotationRevision:      specHash(spec),
			orchestrator.AnnotationReleaseID:     "",
			orchestrator.AnnotationConfigVersion: "-1",
		}).
		WithSpec(batchv1ac.CronJobSpec().
			WithSchedule(spec.Schedule).
			WithTimeZone(scheduleTimeZone).
			// A run still going when the next is due means the next is
			// skipped, as Railway's are: two copies of a job that was
			// written to run alone is how a report gets sent twice.
			WithConcurrencyPolicy(batchv1.ForbidConcurrent).
			// Zero replicas is a stopped app, and a stopped app on a
			// schedule is one whose schedule is paused.
			WithSuspend(spec.Replicas == 0).
			WithSuccessfulJobsHistoryLimit(3).
			WithFailedJobsHistoryLimit(3).
			WithJobTemplate(batchv1ac.JobTemplateSpec().
				WithLabels(labels).
				WithSpec(batchv1ac.JobSpec().
					WithBackoffLimit(backoff).
					WithTemplate(corev1ac.PodTemplateSpec().
						WithLabels(labels).
						WithAnnotations(map[string]string{
							orchestrator.AnnotationReleaseID:     spec.ReleaseID,
							orchestrator.AnnotationConfigVersion: strconv.FormatInt(spec.ConfigVersion, 10),
						}).
						WithSpec(podSpec))))), nil
}

// scheduledStatus reports a workload on a schedule, or ErrNotFound when there
// is no workload of either kind.
//
// Healthy between runs, which is most of the time: nothing running is what a
// scheduled workload looks like when it is working. So it is converged as soon
// as its CronJob carries the release, with nothing to wait for, and its phase
// says it is scheduled — or stopped, when its schedule is paused.
func (o *Orchestrator) scheduledStatus(
	ctx context.Context, ref orchestrator.Ref,
) (orchestrator.AppStatus, error) {
	cj, err := o.client.BatchV1().CronJobs(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return orchestrator.AppStatus{}, orchestrator.ErrNotFound
		}
		return orchestrator.AppStatus{}, fmt.Errorf("k8s: get cron job %s: %w", ref, err)
	}
	return cronStatus(cj), nil
}

// cronStatus reads a scheduled workload's state from its CronJob.
func cronStatus(cj *batchv1.CronJob) orchestrator.AppStatus {
	status := orchestrator.AppStatus{
		Phase:              orchestrator.PhaseScheduled,
		ReleaseID:          cj.Annotations[orchestrator.AnnotationReleaseID],
		AvailableCondition: true,
		Schedule:           cj.Spec.Schedule,
	}
	if version, err := strconv.ParseInt(
		cj.Annotations[orchestrator.AnnotationConfigVersion], 10, 64); err == nil {
		status.ConfigVersion = version
	}
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		status.Phase = orchestrator.PhaseStopped
	}
	if t := cj.Status.LastScheduleTime; t != nil {
		status.LastRun = t.Time
	}
	// A run that started after the last one to succeed, and is no longer
	// running, is a run that failed. Said rather than made a phase: the
	// schedule itself is fine, and the next run may well succeed.
	if t := cj.Status.LastScheduleTime; t != nil && len(cj.Status.Active) == 0 &&
		(cj.Status.LastSuccessfulTime == nil || cj.Status.LastSuccessfulTime.Before(t)) {
		status.Message = "the last run did not succeed"
	}
	return status
}

// deleteCronJob removes a workload's CronJob and the runs it made, tolerating
// its absence.
func (o *Orchestrator) deleteCronJob(ctx context.Context, ref orchestrator.Ref) error {
	policy := metav1.DeletePropagationBackground
	err := o.client.BatchV1().CronJobs(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete cron job %s: %w", ref, err)
	}
	return nil
}
