package k8s

import (
	"context"
	"slices"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// preDeploySpec is an app with everything a pre-deploy run has to carry over:
// secrets, limits, a pool, a user, and a volume it must not mount.
func preDeploySpec() orchestrator.AppSpec {
	spec := testSpec()
	spec.Replicas = 1
	spec.Secrets = map[string]string{"DATABASE_URL": "postgres://db/app"}
	spec.CPULimit, spec.MemoryLimit = "500m", "512Mi"
	spec.Pool = "pro"
	spec.RunAsUser = 1000
	spec.StartCommand = "bin/web start"
	spec.Volumes = []orchestrator.VolumeSpec{{Name: "data", MountPath: "/data", SizeBytes: 1 << 30}}
	return spec
}

// The Job runs the release the way the app is about to run: its image, its
// environment, its secrets, its limits and placement, under the same security
// context — and the command, through a shell.
func TestThePreDeployJobRunsTheReleaseAsTheAppDoes(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := preDeploySpec()
	req := orchestrator.PreDeployRequest{App: spec, Command: "bin/migrate"}
	name := preDeployJobName(spec)

	desired, err := preDeployJob(name, req, name+"-env", 10*time.Minute)
	if err != nil {
		t.Fatalf("preDeployJob: %v", err)
	}
	if _, err := o.client.BatchV1().Jobs(spec.Namespace).Apply(ctx, desired, applyOpts()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	job, err := client.BatchV1().Jobs(spec.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0: a failed migration is not retried", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("activeDeadlineSeconds = %v, want the timeout", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Error("a finished run must be cleaned up by Kubernetes too")
	}
	// Not the app's selector labels: the app's Service would send it traffic.
	if _, ok := job.Spec.Template.Labels[orchestrator.LabelApp]; ok {
		t.Error("the run's pod carries the app's selector label")
	}

	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.RestartPolicy)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be explicitly false")
	}
	if psc := pod.SecurityContext; psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot ||
		psc.RunAsUser == nil || *psc.RunAsUser != 1000 {
		t.Errorf("pod securityContext = %+v, want the app's non-root user", psc)
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != orchestrator.PullSecretName {
		t.Errorf("imagePullSecrets = %v, want the app's", pod.ImagePullSecrets)
	}
	if pod.NodeSelector[orchestrator.PoolLabel] != "pro" {
		t.Errorf("nodeSelector = %v, want the app's pool", pod.NodeSelector)
	}
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("volume %q mounts a claim the running version holds", v.Name)
		}
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Image != spec.Image {
		t.Errorf("image = %q, want the release's %q", c.Image, spec.Image)
	}
	if want := []string{"/bin/sh", "-c", "bin/migrate"}; !slices.Equal(c.Command, want) {
		t.Errorf("command = %v, want %v", c.Command, want)
	}
	if len(c.Args) != 0 {
		t.Errorf("args = %v, want none", c.Args)
	}
	var env []string
	for _, e := range c.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	if want := []string{"APP_ENV=prod", "LOG_LEVEL=info", "PORT=8080"}; !slices.Equal(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil ||
		c.EnvFrom[0].SecretRef.Name != name+"-env" {
		t.Errorf("envFrom = %+v, want the run's own secret", c.EnvFrom)
	}
	sc := c.SecurityContext
	switch {
	case sc == nil:
		t.Fatal("container securityContext must be set")
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Error("allowPrivilegeEscalation must be false")
	case sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem:
		t.Error("readOnlyRootFilesystem must be true, as it is for the app")
	case sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}):
		t.Errorf("capabilities = %+v, want ALL dropped", sc.Capabilities)
	case sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
		t.Error("seccompProfile must be RuntimeDefault")
	}
	if got := c.Resources.Limits.Memory().String(); got != "512Mi" {
		t.Errorf("memory limit = %s, want the app's", got)
	}
}

// The start command replaces the image's command; none leaves the image's.
func TestTheStartCommandLandsInTheContainerCommand(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	spec.StartCommand = "bin/web start"
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	container := func() corev1.Container {
		t.Helper()
		dep, err := client.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get deployment: %v", err)
		}
		return dep.Spec.Template.Spec.Containers[0]
	}
	if want := []string{"/bin/sh", "-c", "bin/web start"}; !slices.Equal(container().Command, want) {
		t.Errorf("command = %v, want %v", container().Command, want)
	}

	spec.StartCommand = ""
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	if got := container().Command; len(got) != 0 {
		t.Errorf("command = %v after clearing it, want the image's own", got)
	}
}

// A run that fails reports its exit code and the end of its log, and leaves
// neither its Job nor the copy of the app's secrets behind.
func TestPreDeployReportsHowTheCommandEnded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o, client := testOrchestrator(t)
	spec := preDeploySpec()
	name := preDeployJobName(spec)

	go finishPreDeploy(ctx, t, client, spec.Namespace, name, 1)

	result, err := o.PreDeploy(ctx, orchestrator.PreDeployRequest{App: spec, Command: "bin/migrate"})
	if err != nil {
		t.Fatalf("PreDeploy: %v", err)
	}
	if result.Succeeded || result.ExitCode != 1 {
		t.Errorf("result = %+v, want a failure with exit code 1", result)
	}
	// The fake clientset answers every log request with this.
	if !slices.Equal(result.Output, []string{"fake logs"}) {
		t.Errorf("output = %q, want the pod's log", result.Output)
	}
	if _, err := client.BatchV1().Jobs(spec.Namespace).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job after the run: %v, want it removed", err)
	}
	if _, err := client.CoreV1().Secrets(spec.Namespace).Get(ctx, name+"-env", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("secret after the run: %v, want it removed", err)
	}
}

// finishPreDeploy plays the Job controller: once the Job exists, it gives it a
// pod that exited with code and marks the Job finished accordingly.
func finishPreDeploy(
	ctx context.Context, t *testing.T, client *fake.Clientset, namespace, name string, code int32,
) {
	for {
		job, err := client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			// The secret is written before the Job, so the pod can read it.
			if _, err := client.CoreV1().Secrets(namespace).Get(ctx, name+"-env", metav1.GetOptions{}); err != nil {
				t.Errorf("the job exists before its secret: %v", err)
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: name + "-abcde", Namespace: namespace,
					Labels: map[string]string{"job-name": name},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					ContainerStatuses: []corev1.ContainerStatus{{
						Name: preDeployContainer,
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
							ExitCode: code, Reason: "Error",
						}},
					}},
				},
			}
			if _, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
				t.Errorf("create pod: %v", err)
				return
			}
			job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
			})
			if _, err := client.BatchV1().Jobs(namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
				t.Errorf("finish job: %v", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
