package k8s

import (
	"context"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// An app on a schedule is a CronJob running the release, in UTC, one run at a
// time — with no Deployment, Service or Ingress — and converged the moment the
// CronJob carries the release. Taking the schedule away brings the Deployment
// back and the CronJob goes.
func TestAScheduleRunsTheAppAsACronJob(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	spec.Schedule = "*/15 * * * *"
	spec.StartCommand = "bin/report"
	spec.RestartPolicy = orchestrator.RestartNever
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp on a schedule: %v", err)
	}

	cj, err := client.BatchV1().CronJobs(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cron job: %v", err)
	}
	if cj.Spec.Schedule != "*/15 * * * *" || cj.Spec.TimeZone == nil || *cj.Spec.TimeZone != "Etc/UTC" {
		t.Errorf("schedule = %q in %v, want the app's in UTC", cj.Spec.Schedule, cj.Spec.TimeZone)
	}
	if cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("concurrency = %q, want a run still going to skip the next", cj.Spec.ConcurrencyPolicy)
	}
	if cj.Spec.Suspend == nil || *cj.Spec.Suspend {
		t.Error("a scheduled app with replicas must not be suspended")
	}
	job := cj.Spec.JobTemplate.Spec
	if job.BackoffLimit == nil || *job.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want no retries for the never policy", job.BackoffLimit)
	}
	pod := job.Template.Spec
	c := pod.Containers[0]
	if want := []string{"/bin/sh", "-c", "bin/report"}; !slices.Equal(c.Command, want) {
		t.Errorf("command = %v, want the start command", c.Command)
	}
	if c.Image != spec.Image || c.ReadinessProbe != nil {
		t.Errorf("container = %s with probe %v, want the release's image and no probe", c.Image, c.ReadinessProbe)
	}
	if psc := pod.SecurityContext; psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("a run's pod must run as non-root, as the app's do")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod restartPolicy = %q, want each retry a new pod", pod.RestartPolicy)
	}

	for what, err := range map[string]error{
		"deployment": get(client.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})),
		"service":    get(client.CoreV1().Services(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})),
		"ingress":    get(client.NetworkingV1().Ingresses(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})),
	} {
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s of a scheduled app: %v, want it gone", what, err)
		}
	}

	status, err := o.AppStatus(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("AppStatus: %v", err)
	}
	if status.Phase != orchestrator.PhaseScheduled || status.Schedule != spec.Schedule ||
		status.ReleaseID != spec.ReleaseID || status.ConfigVersion != spec.ConfigVersion {
		t.Errorf("status = %+v, want scheduled and converged", status)
	}

	spec.Schedule = ""
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp off the schedule: %v", err)
	}
	if _, err := client.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("deployment after the schedule went: %v", err)
	}
	if _, err := client.BatchV1().CronJobs(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("cron job after the schedule went: %v, want it removed", err)
	}
}

func get[T any](_ T, err error) error { return err }

func TestAScheduleKubernetesWouldRefuseIsRefused(t *testing.T) {
	spec := testSpec()
	for _, bad := range []string{"every day", "61 * * * *", "TZ=UTC 0 3 * * *"} {
		spec.Schedule = bad
		if err := spec.Validate(); err == nil {
			t.Errorf("schedule %q passed validation", bad)
		}
	}
	for _, good := range []string{"0 3 * * *", "*/5 * * * 1-5", "@hourly"} {
		spec.Schedule = good
		if err := spec.Validate(); err != nil {
			t.Errorf("schedule %q: %v", good, err)
		}
	}
}

// The restart policy is how many crashes a release may have before it is
// failing: none with never, the retries with on-failure, and no limit with
// always — and it is recorded on the Deployment, where AppStatus reads it.
func TestTheRestartPolicyDecidesWhenACrashFails(t *testing.T) {
	crashed := func(restarts int32) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			RestartCount: restarts,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff",
			}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error",
			}},
		}
	}
	exited := corev1.ContainerStatus{State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"},
	}}
	for _, c := range []struct {
		name   string
		policy orchestrator.RestartPolicy
		max    int32
		status corev1.ContainerStatus
		want   bool
	}{
		{"never fails as the container exits", orchestrator.RestartNever, 0, exited, true},
		{"never fails on the first crash", orchestrator.RestartNever, 0, crashed(0), true},
		{"on failure waits out its retries", orchestrator.RestartOnFailure, 5, crashed(4), false},
		{"on failure fails after them", orchestrator.RestartOnFailure, 5, crashed(5), true},
		{"on failure defaults to three", "", 0, crashed(3), true},
		{"always never gives up on a crash", orchestrator.RestartAlways, 0, crashed(50), false},
	} {
		spec := orchestrator.AppSpec{RestartPolicy: c.policy, MaxRestarts: c.max}
		if _, _, got := containerCannotStart(c.status, spec.CrashLimit()); got != c.want {
			t.Errorf("%s: failing = %v, want %v", c.name, got, c.want)
		}
	}

	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	spec.RestartPolicy, spec.MaxRestarts = orchestrator.RestartOnFailure, 7
	spec.HealthTimeoutSeconds = 90
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	dep, err := client.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := crashLimit(dep.Annotations); got != 7 {
		t.Errorf("recorded crash limit = %d, want 7", got)
	}
	// The healthcheck timeout is Kubernetes' progress deadline too, so the
	// cluster gives up when the deploy does and says why.
	if dep.Spec.ProgressDeadlineSeconds == nil || *dep.Spec.ProgressDeadlineSeconds != 90 {
		t.Errorf("progress deadline = %v, want the healthcheck timeout", dep.Spec.ProgressDeadlineSeconds)
	}
	if _, ok := dep.Spec.Template.Annotations[annotationCrashLimit]; ok {
		t.Error("the policy is in the pod template, so changing it would restart the app")
	}
}

// A Dockerfile path reaches the build as data, and a named Dockerfile that is
// missing fails the build rather than falling back to buildpacks.
func TestADockerfilePathReachesTheBuild(t *testing.T) {
	step := dockerfileStep(orchestrator.BuildRequest{Image: "registry/x:1", Dockerfile: "docker/web.Dockerfile"}, false)
	var found bool
	for _, e := range step.Env {
		if e.Name == "DOCKERFILE" && e.Value == "docker/web.Dockerfile" {
			found = true
		}
	}
	if !found {
		t.Errorf("env = %v, want DOCKERFILE set", step.Env)
	}
	script := step.Args[0]
	for _, want := range []string{`--opt filename="$(basename "$dockerfile")"`, "exit 1"} {
		if !strings.Contains(script, want) {
			t.Errorf("the build script does not have %q", want)
		}
	}
}
