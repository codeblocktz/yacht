package k8s

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Compile-time check that the cluster can run a pre-deploy command.
var _ orchestrator.PreDeployer = (*Orchestrator)(nil)

// preDeployContainer names the one container a pre-deploy pod has, so its
// log can be asked for by name.
const preDeployContainer = "pre-deploy"

// LabelPreDeploy marks a pre-deploy Job and its pod with the app they ran
// for. Its own key rather than the app's selector labels: a pod carrying
// those would be one the app's Service routes traffic to, and one the
// rollout's own checks count as the app's.
const LabelPreDeploy = "yacht/pre-deploy"

// preDeployOutputLines is how much of a command's output is kept: enough for
// a stack trace, not so much that a migration printing a line per row turns a
// deployment's failure message into the migration's log.
const preDeployOutputLines = 50

// preDeployGrace is how much longer than its own deadline a run is waited
// for. The deadline is the Job's to enforce, which it does by failing the Job
// — waiting a little past it means that failure is what gets reported, with
// whatever the command printed, rather than the wait giving up first and
// saying nothing about the command at all.
const preDeployGrace = 30 * time.Second

// PreDeploy runs a release's pre-deploy command to completion and reports how
// it ended.
//
// A Job in the app's own namespace, from the release's own image, with the
// release's environment and the app's pod security, pull secret, limits and
// placement. Everything that decides whether a migration works — which
// database it reaches, as whom, from where, with which variables — is the same
// as it is about to be for the app, and that sameness is the point of running
// it here rather than anywhere more convenient.
//
// Never retried. A migration that failed once fails the same way the second
// time, and one that failed halfway through is not one to start again
// unattended; the deploy fails instead, and the person reading why decides.
func (o *Orchestrator) PreDeploy(
	ctx context.Context, req orchestrator.PreDeployRequest,
) (orchestrator.PreDeployResult, error) {
	spec := req.App
	if err := spec.Validate(); err != nil {
		return orchestrator.PreDeployResult{}, err
	}
	if strings.TrimSpace(req.Command) == "" {
		return orchestrator.PreDeployResult{}, errors.New("k8s: a pre-deploy run needs a command")
	}
	timeout := cmp.Or(req.Timeout, orchestrator.DefaultPreDeployTimeout)

	// The pull secret the app's pods use, written now rather than at apply:
	// for a first deploy this is the first pod in the namespace to pull the
	// image, and without it the pull is refused as ImagePullBackOff.
	if err := o.ensurePullSecret(ctx, spec); err != nil {
		return orchestrator.PreDeployResult{}, err
	}

	name := preDeployJobName(spec)

	// Removed first, as a build's Job is: a run that was interrupted — the
	// engine restarted, its lease moved to another replica — left a Job under
	// this name, and Kubernetes refuses a second rather than replacing it.
	if err := o.deleteJob(ctx, spec.Namespace, name); err != nil {
		return orchestrator.PreDeployResult{}, fmt.Errorf("k8s: clear the last pre-deploy run: %w", err)
	}

	// The release's secrets in a Secret of the run's own, rather than the
	// app's: the app's Secret is what its running pods restart with, and
	// writing the new release's values into it before the new release has
	// been allowed to start would hand them to the old one on its next
	// restart — and leave them there if the command fails.
	var secret string
	if len(spec.Secrets) > 0 {
		secret = name + "-env"
		if err := o.applyPreDeploySecret(ctx, spec, secret, nil); err != nil {
			return orchestrator.PreDeployResult{}, err
		}
		defer func() { o.deletePreDeploySecret(context.WithoutCancel(ctx), spec.Namespace, secret) }()
	}

	desired, err := preDeployJob(name, req, secret, timeout)
	if err != nil {
		return orchestrator.PreDeployResult{}, err
	}
	job, err := o.client.BatchV1().Jobs(spec.Namespace).Apply(ctx, desired, applyOpts())
	if err != nil {
		return orchestrator.PreDeployResult{}, fmt.Errorf("k8s: start the pre-deploy command: %w", err)
	}
	// Removed when this returns, whatever the outcome: its output is in the
	// result, and a finished pod left in a tenant's namespace is a copy of
	// its environment nobody is looking after. The TTL is the backstop for a
	// process that stops before it gets here.
	defer func() { _ = o.deleteJob(context.WithoutCancel(ctx), spec.Namespace, name) }()
	// And the Secret goes with the Job, by Kubernetes' own garbage
	// collection, should this process stop before its deferred delete.
	if secret != "" {
		owner := metav1ac.OwnerReference().
			WithAPIVersion("batch/v1").WithKind("Job").
			WithName(job.Name).WithUID(job.UID)
		if err := o.applyPreDeploySecret(ctx, spec, secret, owner); err != nil {
			o.log.Warn("could not tie the pre-deploy secret to its job",
				slog.String("ref", spec.Ref.String()), slog.String("error", err.Error()))
		}
	}

	result, err := o.waitForPreDeploy(ctx, spec.Namespace, name, timeout)
	if err != nil {
		return orchestrator.PreDeployResult{}, err
	}
	result.Output = o.preDeployOutput(ctx, spec.Namespace, name)

	o.log.Info("pre-deploy command finished",
		slog.String("ref", spec.Ref.String()),
		slog.Bool("succeeded", result.Succeeded),
		slog.Int("exit_code", int(result.ExitCode)))
	return result, nil
}

// preDeployJobName is the Job a release's pre-deploy command runs as.
//
// Derived from the release, so the run for one release has one name: a retry
// replaces the run before it rather than accumulating beside it. The app's
// name leads, so `kubectl get jobs` says whose it is, and is shortened to keep
// the whole a legal label value — a Job's name is copied into its pods'.
func preDeployJobName(spec orchestrator.AppSpec) string {
	sum := sha256.Sum256([]byte(spec.ReleaseID + "\n" + spec.Image))
	suffix := "-predeploy-" + hex.EncodeToString(sum[:])[:8]
	base := spec.Name
	if len(base)+len(suffix) > 63 {
		base = strings.TrimRight(base[:63-len(suffix)], "-")
	}
	return base + suffix
}

// preDeployLabels mark a run as the engine's and the app's, without the
// selector labels that would make it part of the app.
func preDeployLabels(spec orchestrator.AppSpec) map[string]string {
	return map[string]string{
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		orchestrator.LabelOwner:     string(spec.Owner),
		LabelPreDeploy:              spec.Name,
	}
}

// preDeployJob describes one run.
//
// Built from the same pieces as the app's own Deployment — its pod security,
// its container hardening, its resources, its pool and region — so the two
// cannot drift apart: a hardening added to the app reaches its pre-deploy
// command with it, and the namespace's "restricted" admission never sees a
// pod it would refuse.
func preDeployJob(
	name string, req orchestrator.PreDeployRequest, secret string, timeout time.Duration,
) (*batchv1ac.JobApplyConfiguration, error) {
	spec := req.App
	// No volumes. A claim is ReadWriteOnce and the running version holds it,
	// so a run that mounted it would wait for a node it could never have —
	// and a command that writes to the app's storage before the app has
	// started is not a migration but a second copy of the app. Railway's
	// pre-deploy has no volumes either, for the same reason.
	spec.Volumes = nil

	labels := preDeployLabels(spec)
	container, err := preDeployContainerSpec(spec, req.Command, secret)
	if err != nil {
		return nil, err
	}

	podSpec := corev1ac.PodSpec().
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithAutomountServiceAccountToken(false).
		WithSecurityContext(restrictedPodSecurityContext(spec)).
		WithImagePullSecrets(corev1ac.LocalObjectReference().
			WithName(orchestrator.PullSecretName)).
		WithContainers(container).
		WithVolumes(append(
			[]*corev1ac.VolumeApplyConfiguration{
				corev1ac.Volume().
					WithName(tmpVolumeName).
					WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
			},
			volumeSources(spec)...,
		)...)
	// Where the app runs, because that is where its database is reachable
	// from: a migration run in another region would cross the world, or a
	// network policy, to reach it.
	podSpec = placeInPool(podSpec, spec.Pool)
	podSpec = placeInRegion(podSpec, spec.Region)

	return batchv1ac.Job(name, spec.Namespace).
		WithLabels(labels).
		WithSpec(batchv1ac.JobSpec().
			// Never retried: see PreDeploy. Kubernetes' own default is six.
			WithBackoffLimit(0).
			WithActiveDeadlineSeconds(int64(timeout / time.Second)).
			WithTTLSecondsAfterFinished(3600).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(labels).
				WithSpec(podSpec))), nil
}

// preDeployContainerSpec is the app's container, running the command.
//
// Not the app's container itself: that one carries a port, probes and the
// app's start command, none of which mean anything for a command that runs
// once and exits.
func preDeployContainerSpec(
	spec orchestrator.AppSpec, command, secret string,
) (*corev1ac.ContainerApplyConfiguration, error) {
	c := corev1ac.Container().
		WithName(preDeployContainer).
		WithImage(spec.Image).
		WithImagePullPolicy(corev1.PullIfNotPresent).
		WithSecurityContext(restrictedContainerSecurityContext(spec.WritableRootFilesystem)).
		WithCommand(shellCommand(command)...).
		WithVolumeMounts(append(
			[]*corev1ac.VolumeMountApplyConfiguration{
				corev1ac.VolumeMount().WithName(tmpVolumeName).WithMountPath("/tmp"),
			},
			volumeMounts(spec)...,
		)...)
	if secret != "" {
		c = c.WithEnvFrom(corev1ac.EnvFromSource().
			WithSecretRef(corev1ac.SecretEnvSource().WithName(secret)))
	}
	for _, k := range slices.Sorted(maps(spec.Env)) {
		c = c.WithEnv(corev1ac.EnvVar().WithName(k).WithValue(spec.Env[k]))
	}
	// PORT as the app gets it, so a command that reads its configuration the
	// way the app does finds the same thing there.
	if port, ok := portEnv(spec); ok {
		c = c.WithEnv(corev1ac.EnvVar().WithName("PORT").WithValue(port))
	}
	// The app's own limits, so a migration that needs more memory than the
	// app is given fails here, where it is a migration's problem, rather
	// than passing here and the app then failing the same way in production.
	res, err := resourceRequirements(spec)
	if err != nil {
		return nil, err
	}
	if res != nil {
		c = c.WithResources(res)
	}
	return c, nil
}

// applyPreDeploySecret writes a run's secrets, owned by its Job once there
// is one.
func (o *Orchestrator) applyPreDeploySecret(
	ctx context.Context, spec orchestrator.AppSpec, name string,
	owner *metav1ac.OwnerReferenceApplyConfiguration,
) error {
	sec := corev1ac.Secret(name, spec.Namespace).
		WithLabels(preDeployLabels(spec)).
		WithType(corev1.SecretTypeOpaque).
		WithStringData(spec.Secrets)
	if owner != nil {
		sec = sec.WithOwnerReferences(owner)
	}
	if _, err := o.client.CoreV1().Secrets(spec.Namespace).Apply(ctx, sec, applyOpts()); err != nil {
		return fmt.Errorf("k8s: store the pre-deploy command's secrets: %w", err)
	}
	return nil
}

func (o *Orchestrator) deletePreDeploySecret(ctx context.Context, namespace, name string) {
	err := o.client.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		o.log.Warn("could not remove the pre-deploy secret",
			slog.String("namespace", namespace), slog.String("error", err.Error()))
	}
}

// waitForPreDeploy waits for a run to end and says how it did.
//
// The Job decides when it has failed, deadline included. The one thing waited
// for here that the Job will not report quickly is a container that cannot
// start at all — a missing secret, an image with no shell — which Kubernetes
// keeps retrying until the deadline: that is read from the pod and answered
// at once, the same way a rollout is.
func (o *Orchestrator) waitForPreDeploy(
	ctx context.Context, namespace, name string, timeout time.Duration,
) (orchestrator.PreDeployResult, error) {
	waiting, cancel := context.WithTimeout(ctx, timeout+preDeployGrace)
	defer cancel()

	var result orchestrator.PreDeployResult
	err := wait.PollUntilContextCancel(waiting, time.Second, true,
		func(ctx context.Context) (bool, error) {
			job, err := o.client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Errorf("k8s: read the pre-deploy job: %w", err)
			}
			for _, c := range job.Status.Conditions {
				if c.Status != corev1.ConditionTrue {
					continue
				}
				switch c.Type {
				case batchv1.JobComplete:
					result = orchestrator.PreDeployResult{Succeeded: true}
					return true, nil
				case batchv1.JobFailed:
					result = o.preDeployFailure(ctx, namespace, name, c, timeout)
					return true, nil
				}
			}
			pod, ok := o.preDeployPod(ctx, namespace, name)
			if !ok {
				return false, nil
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if reason, message, ok := containerCannotStart(cs, 0); ok {
					result = orchestrator.PreDeployResult{
						Reason: "could not start (" + strings.TrimSpace(reason+": "+message) + ")",
					}
					return true, nil
				}
			}
			return false, nil
		})
	switch {
	case err == nil:
		return result, nil
	case ctx.Err() != nil:
		// Cancelled from above — the deploy was, or its lease was lost —
		// which is not the command's result and must not be reported as one.
		return orchestrator.PreDeployResult{}, ctx.Err()
	case waiting.Err() != nil:
		return orchestrator.PreDeployResult{Reason: timedOut(timeout)}, nil
	default:
		return orchestrator.PreDeployResult{}, err
	}
}

// preDeployFailure reads why a failed run failed: its exit code from the pod,
// or its deadline from the Job, which is the one failure that leaves an exit
// code — the one the kill produced — that would mislead.
func (o *Orchestrator) preDeployFailure(
	ctx context.Context, namespace, name string, c batchv1.JobCondition, timeout time.Duration,
) orchestrator.PreDeployResult {
	if c.Reason == batchv1.JobReasonDeadlineExceeded {
		return orchestrator.PreDeployResult{Reason: timedOut(timeout)}
	}
	if pod, ok := o.preDeployPod(ctx, namespace, name); ok {
		for _, cs := range pod.Status.ContainerStatuses {
			t := cs.State.Terminated
			if cs.Name != preDeployContainer || t == nil {
				continue
			}
			out := orchestrator.PreDeployResult{ExitCode: t.ExitCode}
			// "Error" is what Kubernetes says of every non-zero exit, and the
			// exit code already says it. Anything else — OOMKilled above all —
			// is the more useful half of the answer.
			if t.Reason != "" && t.Reason != "Error" {
				out.Reason = t.Reason
			}
			return out
		}
	}
	return orchestrator.PreDeployResult{Reason: cmp.Or(c.Message, c.Reason, "the job failed")}
}

func timedOut(timeout time.Duration) string {
	return "timed out after " + timeout.String()
}

// preDeployPod is the pod a run's Job made, if it has made one yet.
func (o *Orchestrator) preDeployPod(ctx context.Context, namespace, job string) (corev1.Pod, bool) {
	pods, err := o.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job,
	})
	if err != nil || len(pods.Items) == 0 {
		return corev1.Pod{}, false
	}
	return pods.Items[0], true
}

// preDeployOutput is the end of what a finished run printed.
//
// Best effort: the command has already ended, and a log that cannot be read
// changes nothing about how. Read once the run is over rather than followed,
// because the end is what is kept and a finished container's log needs
// nothing of the kubelet but the file.
func (o *Orchestrator) preDeployOutput(ctx context.Context, namespace, job string) []string {
	pod, ok := o.preDeployPod(ctx, namespace, job)
	if !ok {
		return nil
	}
	tail := int64(preDeployOutputLines)
	stream, err := o.client.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: preDeployContainer, TailLines: &tail,
	}).Stream(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = stream.Close() }()

	var lines []string
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
		if len(lines) > preDeployOutputLines {
			lines = lines[1:]
		}
	}
	return lines
}
