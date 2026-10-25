package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func testJob() orchestrator.JobSpec {
	return orchestrator.JobSpec{
		Ref:  orchestrator.Ref{Owner: "owner-1", Namespace: "yacht-demo", Name: "db"},
		Name: "db-backup-1234",
		Steps: []orchestrator.JobStep{
			{Name: "dump", Image: "postgres@sha256:abc", Command: []string{"sh", "-c", "pg_dump"}},
			{Name: "upload", Image: "curlimages/curl:8.10.1", Command: []string{"sh", "-c", "curl"}},
		},
		RunAsUser:    70,
		Env:          map[string]string{"PGHOST": "db", "PGUSER": "yacht"},
		SecretEnv:    map[string]string{"UPLOAD_URL": "https://s3.example/b/k?X-Amz-Signature=x"},
		AppSecretEnv: map[string]string{"PGPASSWORD": "POSTGRES_PASSWORD"},
		ScratchPath:  "/backup",
		Deadline:     2 * time.Hour,
		Pool:         "pro",
	}
}

// A job runs under the same floor as the app: the restricted profile, a
// read-only root, no token, and nothing that would let the namespace's Pod
// Security level refuse it.
func TestAJobRunsUnderTheRestrictedProfile(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	if err := o.RunJob(ctx, testJob()); err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("yacht-demo").Get(ctx, "db-backup-1234", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if *job.Spec.BackoffLimit != 0 {
		t.Error("a job is retried; a restore must never run twice")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 7200 {
		t.Errorf("deadline = %v", job.Spec.ActiveDeadlineSeconds)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the job's pod is given a service account token")
	}
	psc := pod.SecurityContext
	if psc == nil || !*psc.RunAsNonRoot || *psc.FSGroup != 70 ||
		psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v", psc)
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "dump" ||
		len(pod.Containers) != 1 || pod.Containers[0].Name != "upload" {
		t.Fatalf("steps are not in order: init=%v main=%v", pod.InitContainers, pod.Containers)
	}
	for _, c := range append(pod.InitContainers, pod.Containers...) {
		sc := c.SecurityContext
		if sc == nil || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation ||
			*sc.RunAsUser != 70 || len(sc.Capabilities.Drop) != 1 {
			t.Errorf("%s security context = %+v", c.Name, sc)
		}
		if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
			t.Errorf("%s: a failure would not say why", c.Name)
		}
		var password, inline bool
		for _, e := range c.Env {
			if e.Name == "PGPASSWORD" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef.Name == "db-env" &&
				e.ValueFrom.SecretKeyRef.Key == "POSTGRES_PASSWORD" {
				password = true
			}
			if strings.Contains(e.Value, "X-Amz-Signature") {
				inline = true
			}
		}
		if !password {
			t.Errorf("%s does not read the password from the app's Secret: %+v", c.Name, c.Env)
		}
		if inline {
			t.Errorf("%s carries the signed URL in its definition", c.Name)
		}
		if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != "db-backup-1234-env" {
			t.Errorf("%s envFrom = %+v", c.Name, c.EnvFrom)
		}
	}
	if pod.NodeSelector[orchestrator.PoolLabel] != "pro" {
		t.Errorf("the job does not run in the team's pool: %v", pod.NodeSelector)
	}
	labels := job.Spec.Template.Labels
	if labels[orchestrator.LabelApp] != "" || labels[orchestrator.LabelManagedBy] != "" {
		t.Errorf("the job's pod would count as the app's own: %v", labels)
	}

	sec, err := client.CoreV1().Secrets("yacht-demo").Get(ctx, "db-backup-1234-env", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" {
		t.Errorf("the job's Secret is not owned by the Job: %+v", sec.OwnerReferences)
	}

	// Starting the same job again is a retry of the first, not a second job.
	if err := o.RunJob(ctx, testJob()); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteJob(ctx, "yacht-demo", "db-backup-1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("yacht-demo").Get(ctx, "db-backup-1234-env", metav1.GetOptions{}); err == nil {
		t.Error("the job's Secret outlived it")
	}
	if st, err := o.JobState(ctx, "yacht-demo", "db-backup-1234"); err != nil || st.Found {
		t.Errorf("state after delete = %+v, %v", st, err)
	}
}

func TestAJobWithAVolumeCreatesTheClaim(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testJob()
	spec.Steps = spec.Steps[:1]
	spec.SecretEnv = nil
	spec.Volume = &orchestrator.VolumeSpec{Name: "backups", MountPath: "/backups", SizeBytes: 2 << 30}
	if err := o.RunJob(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("yacht-demo").Get(ctx, "db-backups", metav1.GetOptions{}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	job, _ := client.BatchV1().Jobs("yacht-demo").Get(ctx, spec.Name, metav1.GetOptions{})
	if c := job.Spec.Template.Spec.Containers[0]; len(c.EnvFrom) != 0 {
		t.Errorf("envFrom a Secret that was never made: %+v", c.EnvFrom)
	}
}

func TestJobStateSaysWhichStepFailedAndWhy(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	if err := o.RunJob(ctx, testJob()); err != nil {
		t.Fatal(err)
	}
	job, _ := client.BatchV1().Jobs("yacht-demo").Get(ctx, "db-backup-1234", metav1.GetOptions{})
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
	}}
	if _, err := client.BatchV1().Jobs("yacht-demo").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := client.CoreV1().Pods("yacht-demo").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "db-backup-1234-x", Labels: map[string]string{"job-name": "db-backup-1234"}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
			Name: "dump",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: "pg_dump: error: connection to server failed\npg_dump: detail: password authentication failed",
			}},
		}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := o.JobState(ctx, "yacht-demo", "db-backup-1234")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done || !st.Failed || !strings.Contains(st.Reason, "the dump step exited 1") ||
		!strings.Contains(st.Reason, "password authentication failed") {
		t.Errorf("state = %+v", st)
	}
}

// Redis takes its password only as a flag. The argument names the variable
// and the kubelet expands it from the Secret, so the pod template carries the
// reference and never the value — and an app started with its image's own
// command keeps the fingerprint it always had.
func TestArgumentsReachTheContainerAndTheFingerprint(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()
	plain := specHash(spec)
	spec.Args = []string{"redis-server", "--requirepass", "$(REDIS_PASSWORD)"}
	spec.Secrets = map[string]string{"REDIS_PASSWORD": "hunter2"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	dep, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if strings.Join(c.Args, " ") != "redis-server --requirepass $(REDIS_PASSWORD)" || len(c.Command) != 0 {
		t.Errorf("command=%q args=%q; the image's entrypoint must stay", c.Command, c.Args)
	}
	if specHash(spec) == plain {
		t.Error("changing the arguments does not change the fingerprint")
	}
	spec.Args = nil
	if specHash(spec) != plain {
		t.Error("an app with no arguments changed fingerprint")
	}
}
