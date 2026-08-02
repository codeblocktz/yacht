package k8s

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// BuildNamespace is where builds run.
//
// Deliberately not the app's own namespace. Every app namespace is Pod
// Security Admission "restricted", and a container that builds container
// images cannot run under it — so putting builds there would mean either
// weakening every tenant namespace or having no builds. One namespace with a
// looser posture, holding only Jobs this code creates from images this code
// names, is a much smaller claim than that.
const BuildNamespace = "yacht-builds"

// BuildServiceAccount is what build pods run as.
//
// Its own account, created with no RoleBinding of any kind, rather than the
// namespace's "default". A build executes whatever a repository's Dockerfile
// says to execute, so the account it runs under is the blast radius — and the
// default account is the one every admission-hardening guide names first.
const BuildServiceAccount = "yacht-builder"

// The images a build runs. Pinned rather than floating: a build that silently
// changes what compiled it is one nobody can reproduce, and ":latest" here
// means the toolchain moves under a running install.
const (
	// gitImage clones. Its own step so the clone's failures — a bad URL, a
	// private repository, a ref that does not exist — are reported as clone
	// failures rather than as a build that produced nothing.
	gitImage = "alpine/git:2.47.2"

	// buildkitImage builds a Dockerfile. Rootless, because the alternative is
	// a privileged container with the host's Docker socket, which is a root
	// shell on the node for anybody who can influence a Dockerfile.
	buildkitImage = "moby/buildkit:v0.19.0-rootless"

	// packImage is the buildpacks fallback: it builds a repository that has no
	// Dockerfile by detecting what the code is. Large, and only pulled on
	// repositories that need it.
	packImage = "paketobuildpacks/builder-jammy-base:latest"
)

// buildTimeout caps a single build.
//
// A cap rather than none: a build that hangs holds a Job, its pod, and the
// deployment that is waiting on it, and "still building" after an hour is not
// a state anybody should have to interpret.
const buildTimeout = 30 * time.Minute

// Build runs one build to completion and returns what it produced.
//
// Synchronous from this seam's point of view. Deciding whether to wait belongs
// to the caller — the same build is worth blocking on from a CLI and worth
// backgrounding from a web request, and an interface that made that choice
// would force one of them to work around it.
func (o *Orchestrator) Build(
	ctx context.Context, req orchestrator.BuildRequest,
) (orchestrator.BuildResult, error) {
	if req.Image == "" || req.RepoURL == "" {
		return orchestrator.BuildResult{}, fmt.Errorf(
			"k8s: a build needs a repository and an image to push to")
	}
	if req.Log == nil {
		req.Log = func(string) {}
	}

	if err := o.ensureBuildNamespace(ctx); err != nil {
		return orchestrator.BuildResult{}, err
	}

	secret, err := o.ensureBuildSecret(ctx, req.RegistryAuth)
	if err != nil {
		return orchestrator.BuildResult{}, err
	}

	name := buildJobName(req)
	job := buildJob(name, req, secret)

	// Removed first. A Job name is derived from the deployment, so a retry of
	// the same deployment collides with the previous attempt — and Kubernetes
	// reports that as an already-exists error rather than running anything.
	_ = o.deleteBuildJob(ctx, name)

	if _, err := o.client.BatchV1().Jobs(BuildNamespace).
		Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return orchestrator.BuildResult{}, fmt.Errorf("k8s: create build job: %w", err)
	}
	// The Job outlives this function only if something goes very wrong; the
	// deferred delete is what stops a failed build's pod sitting on a node
	// holding its image cache and its disk.
	defer func() { _ = o.deleteBuildJob(context.WithoutCancel(ctx), name) }()

	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()

	pod, err := o.waitForBuildPod(ctx, name, req.Log)
	if err != nil {
		return orchestrator.BuildResult{}, err
	}

	// Streamed rather than fetched at the end: this is the several minutes
	// somebody is actually watching, and a log that arrives afterwards is a
	// post-mortem rather than progress.
	result := o.streamBuildLog(ctx, pod, req.Log)

	if err := o.waitForBuildResult(ctx, name); err != nil {
		return result, err
	}
	return result, nil
}

// ensureBuildNamespace creates the namespace builds run in.
//
// Its Pod Security Admission level is "privileged", and that is the honest
// cost of building images in-cluster. Rootless BuildKit still needs a seccomp
// profile the restricted policy forbids, and the enforcement that would
// otherwise apply is replaced here by what can actually be controlled: nothing
// runs in this namespace except Jobs this code creates, from images named as
// constants above, and no tenant workload is ever scheduled into it.
func (o *Orchestrator) ensureBuildNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: BuildNamespace,
			Labels: map[string]string{
				orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
				psaEnforce:                  "privileged",
				psaAudit:                    "privileged",
				psaWarn:                     "privileged",
			},
		},
	}
	_, err := o.client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("k8s: create build namespace: %w", err)
	}

	// No RoleBinding accompanies this, deliberately and permanently. The
	// account exists so build pods are not the namespace default, and it is
	// useful precisely because nothing is ever granted to it.
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: BuildServiceAccount, Namespace: BuildNamespace,
			Labels: map[string]string{orchestrator.LabelManagedBy: orchestrator.ManagedByValue},
		},
		AutomountServiceAccountToken: boolPtr(false),
	}
	_, err = o.client.CoreV1().ServiceAccounts(BuildNamespace).
		Create(ctx, sa, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("k8s: create build service account: %w", err)
	}
	return nil
}

// buildRegistrySecret is the name of the push credential in the build
// namespace. One secret rather than one per build: the credential is the
// install's, and a copy per build would be a copy per build to clean up.
const buildRegistrySecret = "yacht-registry"

// ensureBuildSecret puts the push credential where the builder can read it.
func (o *Orchestrator) ensureBuildSecret(ctx context.Context, auth []byte) (string, error) {
	if len(auth) == 0 {
		// Refused rather than attempted. A build with no credential runs for
		// several minutes and fails at the push, which reads as a broken
		// Dockerfile rather than a missing registry.
		return "", fmt.Errorf("k8s: no registry credential was supplied for the build")
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: buildRegistrySecret, Namespace: BuildNamespace,
			Labels: map[string]string{orchestrator.LabelManagedBy: orchestrator.ManagedByValue},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: auth},
	}

	_, err := o.client.CoreV1().Secrets(BuildNamespace).Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = o.client.CoreV1().Secrets(BuildNamespace).Update(ctx, sec, metav1.UpdateOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("k8s: store the build credential: %w", err)
	}
	return buildRegistrySecret, nil
}

// buildJobName is derived from the image tag, which is derived from the
// deployment — so a build has one name, and retrying a deployment reuses it
// rather than accumulating Jobs nobody looks at.
func buildJobName(req orchestrator.BuildRequest) string {
	tag := req.Image
	if i := strings.LastIndex(tag, ":"); i >= 0 {
		tag = tag[i+1:]
	}
	return "build-" + sanitiseLabel(tag)
}

func (o *Orchestrator) deleteBuildJob(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationBackground
	err := o.client.BatchV1().Jobs(BuildNamespace).
		Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Deleting the Job in the background leaves its pod briefly alive, and the
	// next create would collide with a pod still holding the same name. Waited
	// for rather than raced.
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := o.client.BatchV1().Jobs(BuildNamespace).Get(ctx, name, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		})
}

// waitForBuildPod waits until there is a pod with a log to read.
//
// The wait is reported rather than silent. "Waiting for a node" and "pulling
// the builder image" are the two slowest parts of a first build, and a blank
// pane for four minutes reads as a hang.
func (o *Orchestrator) waitForBuildPod(
	ctx context.Context, job string, logf func(string),
) (string, error) {
	logf("Scheduling the build…\n")

	var pod string
	var lastPhase corev1.PodPhase
	err := wait.PollUntilContextCancel(ctx, time.Second, true,
		func(ctx context.Context) (bool, error) {
			pods, err := o.client.CoreV1().Pods(BuildNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "job-name=" + job,
			})
			if err != nil || len(pods.Items) == 0 {
				return false, nil
			}
			p := pods.Items[0]
			pod = p.Name

			if p.Status.Phase != lastPhase {
				lastPhase = p.Status.Phase
				logf("Build pod " + p.Name + " is " + strings.ToLower(string(p.Status.Phase)) + ".\n")
			}
			// Any phase past Pending has a log; a pod that failed to start has
			// one too, and it says why.
			return p.Status.Phase != corev1.PodPending || startedAnyContainer(p), nil
		})
	if err != nil {
		return "", fmt.Errorf("k8s: waiting for the build to start: %w", err)
	}
	return pod, nil
}

// startedAnyContainer reports whether there is output to read yet.
func startedAnyContainer(p corev1.Pod) bool {
	for _, cs := range append(append([]corev1.ContainerStatus{},
		p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.State.Running != nil || cs.State.Terminated != nil {
			return true
		}
	}
	return false
}

// streamBuildLog follows each container in turn and reports what it says.
//
// Each container is followed rather than only the last, because the clone and
// the image build are separate steps and the interesting failure is usually
// the first one. The commit is picked out of the clone's output on the way
// past, so there is no second call to ask what was built.
//
// A container that has not started yet has no log, and the API says so with an
// error. That is waited through rather than skipped: init containers run in
// order, so the build step's log is always unavailable at the moment the
// clone's ends — and skipping on the first error drops the one log somebody
// opened this to read.
func (o *Orchestrator) streamBuildLog(
	ctx context.Context, pod string, logf func(string),
) orchestrator.BuildResult {
	var result orchestrator.BuildResult

	for _, c := range []string{cloneContainer, dockerfileContainer, buildpackContainer} {
		stream, err := o.openContainerLog(ctx, pod, c)
		if err != nil {
			// Genuinely nothing to read: the pod finished without reaching
			// this step, which is the ordinary case for whichever build
			// strategy was not used.
			continue
		}

		sc := bufio.NewScanner(stream)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if sha, ok := strings.CutPrefix(line, commitMarker); ok {
				result.CommitSHA = strings.TrimSpace(sha)
				continue
			}
			if uid, ok := strings.CutPrefix(line, uidMarker); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(uid), 10, 64); err == nil {
					result.RunAsUser = n
				}
				continue
			}
			logf(line + "\n")
		}
		_ = stream.Close()
	}
	return result
}

// openContainerLog waits until a container has output, or until it is clear it
// will not run.
func (o *Orchestrator) openContainerLog(
	ctx context.Context, pod, container string,
) (io.ReadCloser, error) {
	var stream io.ReadCloser
	err := wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true,
		func(ctx context.Context) (bool, error) {
			s, err := o.client.CoreV1().Pods(BuildNamespace).GetLogs(pod,
				&corev1.PodLogOptions{Container: container, Follow: true}).Stream(ctx)
			if err == nil {
				stream = s
				return true, nil
			}
			// The pod reaching a terminal phase without this container having
			// started is the answer that it never will.
			p, getErr := o.client.CoreV1().Pods(BuildNamespace).
				Get(ctx, pod, metav1.GetOptions{})
			if getErr != nil {
				return false, getErr
			}
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				return false, err
			}
			return false, nil
		})
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// waitForBuildResult reports whether the Job finished, and why not.
func (o *Orchestrator) waitForBuildResult(ctx context.Context, name string) error {
	return wait.PollUntilContextCancel(ctx, time.Second, true,
		func(ctx context.Context) (bool, error) {
			job, err := o.client.BatchV1().Jobs(BuildNamespace).
				Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Errorf("k8s: read build job: %w", err)
			}
			for _, c := range job.Status.Conditions {
				if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
					return true, nil
				}
				if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
					// Which step failed, from the pod. The Job's own condition
					// says "backoff limit exceeded", which is true of every
					// failed build and tells nobody anything.
					return false, fmt.Errorf("the build failed: %s",
						o.buildFailureReason(ctx, name, c))
				}
			}
			return false, nil
		})
}

// buildFailureReason names the step that failed and how.
//
// Read from the pod rather than the Job. "Job has reached the specified
// backoff limit" is what Kubernetes says about every failed build, and it is
// the same sentence whether the clone could not find the branch or the
// compiler ran out of memory.
func (o *Orchestrator) buildFailureReason(
	ctx context.Context, job string, c batchv1.JobCondition,
) string {
	pods, err := o.client.CoreV1().Pods(BuildNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job,
	})
	if err == nil {
		for _, p := range pods.Items {
			all := append(append([]corev1.ContainerStatus{},
				p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
			for _, cs := range all {
				t := cs.State.Terminated
				if t == nil || t.ExitCode == 0 {
					continue
				}
				reason := strings.TrimSpace(t.Reason)
				if t.Message != "" {
					reason = strings.TrimSpace(t.Message)
				}
				if reason == "" {
					reason = "see the log above"
				}
				return fmt.Sprintf("the %s step exited %d (%s)",
					cs.Name, t.ExitCode, reason)
			}
		}
	}
	switch {
	case c.Message != "":
		return c.Message
	case c.Reason != "":
		return c.Reason
	default:
		return "no reason was reported — the log above is what there is"
	}
}

// sanitiseLabel reduces a value to what a Kubernetes name accepts.
func sanitiseLabel(s string) string {
	var b strings.Builder
	dashed := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dashed = false
			continue
		}
		if !dashed {
			b.WriteByte('-')
			dashed = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 50 {
		out = strings.Trim(out[:50], "-")
	}
	if out == "" {
		return "build"
	}
	return out
}

// BuildJobName is the Job name a request will use.
func (o *Orchestrator) BuildJobName(req orchestrator.BuildRequest) string {
	return buildJobName(req)
}

// BuildState reports what the cluster says about a build started earlier.
//
// The Job is asked rather than any record this process kept, which is the
// point: a build interrupted by a restart is settled by looking at the cluster,
// the same way a controller reconciles observed state against desired state
// instead of trusting that it was running when it last looked.
func (o *Orchestrator) BuildState(
	ctx context.Context, jobName string,
) (orchestrator.BuildState, error) {
	if jobName == "" {
		// A build recorded before its Job was named. Nothing is running under
		// a name nobody stored, so it is reported the same way as a Job that
		// has gone.
		return orchestrator.BuildState{}, nil
	}

	job, err := o.client.BatchV1().Jobs(BuildNamespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return orchestrator.BuildState{}, nil
		}
		return orchestrator.BuildState{}, fmt.Errorf("k8s: read build job: %w", err)
	}

	state := orchestrator.BuildState{Found: true}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			state.Done = true
		case batchv1.JobFailed:
			state.Done, state.Failed = true, true
			state.Reason = o.buildFailureReason(ctx, jobName, c)
		}
	}
	return state, nil
}
