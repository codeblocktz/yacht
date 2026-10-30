package k8s

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// AppStatuses says of every workload what AppStatus says of it, from a list
// of each kind rather than a GET each: running, short of ready, failing to
// start, on a schedule, suspended, and not there at all.
func TestAppStatusesSaysWhatAppStatusSays(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	ref := func(owner, name string) orchestrator.Ref {
		// A namespace of each workload's own, as the engine gives them.
		return orchestrator.Ref{Owner: orchestrator.OwnerID(owner), Namespace: "yacht-" + name, Name: name}
	}
	deployment := func(r orchestrator.Ref, want, ready int32, release string) {
		t.Helper()
		if _, err := client.AppsV1().Deployments(r.Namespace).Create(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: r.Name, Namespace: r.Namespace,
				Labels:      orchestrator.ObjectLabels(r),
				Annotations: map[string]string{orchestrator.AnnotationReleaseID: release}},
			Spec:   appsv1.DeploymentSpec{Replicas: &want},
			Status: appsv1.DeploymentStatus{ReadyReplicas: ready, AvailableReplicas: ready, UpdatedReplicas: ready},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pod := func(r orchestrator.Ref, release string, cs corev1.ContainerStatus) {
		t.Helper()
		if _, err := client.CoreV1().Pods(r.Namespace).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: r.Name + "-abc", Namespace: r.Namespace,
				Labels:      orchestrator.ObjectLabels(r),
				Annotations: map[string]string{orchestrator.AnnotationReleaseID: release}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	cron := func(r orchestrator.Ref, suspend bool) {
		t.Helper()
		last := metav1.NewTime(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
		if _, err := client.BatchV1().CronJobs(r.Namespace).Create(ctx, &batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: r.Name, Namespace: r.Namespace, Labels: orchestrator.ObjectLabels(r)},
			Spec:       batchv1.CronJobSpec{Schedule: "*/5 * * * *", Suspend: &suspend},
			Status:     batchv1.CronJobStatus{LastScheduleTime: &last},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	running, degraded, broken, starting := ref("o1", "running"), ref("o1", "degraded"), ref("o1", "broken"), ref("o1", "starting")
	scheduled, suspended, missing := ref("o1", "scheduled"), ref("o1", "suspended"), ref("o1", "missing")
	other := ref("o2", "theirs")
	deployment(running, 2, 2, "r1")
	// Up, after one restart for its memory: the restarts are a deploy's to
	// watch, and read the same either way.
	pod(running, "r1", corev1.ContainerStatus{Name: "app", Ready: true, RestartCount: 1,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "OOMKilled"}}})
	deployment(degraded, 2, 1, "r1")
	deployment(broken, 1, 0, "r2")
	pod(broken, "r2", waiting("CreateContainerConfigError", 0, nil))
	deployment(starting, 1, 0, "r2")
	pod(starting, "r2", waiting("ContainerCreating", 0, nil))
	cron(scheduled, false)
	cron(suspended, true)
	deployment(other, 1, 1, "r9")
	refs := []orchestrator.Ref{running, degraded, broken, starting, scheduled, suspended, missing, other}

	var (
		mu    sync.Mutex
		calls []string
	)
	client.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, a.GetVerb()+" "+a.GetResource().Resource)
		return false, nil, nil
	})
	got, err := o.AppStatuses(ctx, refs)
	if err != nil {
		t.Fatalf("AppStatuses: %v", err)
	}
	// Per owner: Deployments, CronJobs, and pods for the workloads with a
	// release, which is every one with a Deployment. None of them a GET of
	// one workload.
	if len(calls) != 6 {
		t.Errorf("made %d calls for %d workloads of two owners, want 6: %v", len(calls), len(refs), calls)
	}
	for _, c := range calls {
		if c[:4] != "list" {
			t.Errorf("call %q, want only lists", c)
		}
	}

	for _, r := range refs {
		want, err := o.AppStatus(ctx, r)
		have, ok := got[r]
		if errors.Is(err, orchestrator.ErrNotFound) {
			if ok {
				t.Errorf("%s: AppStatuses has %+v, AppStatus says it is not there", r, have)
			}
			continue
		}
		if err != nil {
			t.Fatalf("AppStatus %s: %v", r, err)
		}
		if !ok {
			t.Errorf("%s: missing from AppStatuses, AppStatus says %+v", r, want)
			continue
		}
		if !reflect.DeepEqual(have, want) {
			t.Errorf("%s:\nAppStatuses %+v\nAppStatus   %+v", r, have, want)
		}
	}
	// And they are what they are, not merely the same as each other.
	for r, phase := range map[orchestrator.Ref]orchestrator.Phase{
		running: orchestrator.PhaseRunning, degraded: orchestrator.PhaseDegraded,
		broken: orchestrator.PhaseFailing, starting: orchestrator.PhasePending,
		scheduled: orchestrator.PhaseScheduled, suspended: orchestrator.PhaseStopped,
		other: orchestrator.PhaseRunning,
	} {
		if got[r].Phase != phase {
			t.Errorf("%s: phase %q, want %q", r, got[r].Phase, phase)
		}
	}
}

// One owner's workload is not another's, even with the same name.
func TestAppStatusesKeepsOwnersApart(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	mine := orchestrator.Ref{Owner: "o1", Namespace: "yacht-a", Name: "web"}
	theirs := orchestrator.Ref{Owner: "o2", Namespace: "yacht-a", Name: "web"}
	one := int32(1)
	if _, err := client.AppsV1().Deployments(theirs.Namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: theirs.Name, Namespace: theirs.Namespace, Labels: orchestrator.ObjectLabels(theirs)},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := o.AppStatuses(ctx, []orchestrator.Ref{mine})
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := got[mine]; ok {
		t.Errorf("o1 was given o2's workload: %+v", st)
	}
}

// The client is not held to client-go's defaults of 5 calls a second in
// bursts of 10, a command-line tool's, which kept every page with a dozen
// cluster calls waiting on its own limiter (DEPLOY-13). A limit somebody set
// in their kubeconfig stands.
func TestTheClientIsNotHeldToClientGoDefaults(t *testing.T) {
	c := &rest.Config{}
	withRateLimits(c)
	if c.QPS != clientQPS || c.Burst != clientBurst {
		t.Errorf("QPS %v burst %d, want %v and %d", c.QPS, c.Burst, float32(clientQPS), clientBurst)
	}
	if clientQPS < 50 || clientBurst < 100 {
		t.Errorf("limits %d/%d are too low to load a page of a dozen calls at once", clientQPS, clientBurst)
	}

	set := &rest.Config{QPS: 20, Burst: 30}
	withRateLimits(set)
	if set.QPS != 20 || set.Burst != 30 {
		t.Errorf("QPS %v burst %d, want the 20 and 30 that were set", set.QPS, set.Burst)
	}
}

// The restarts a rollout is watched for are in the batched statuses too.
func TestAppStatusesCountRestarts(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	r := orchestrator.Ref{Owner: "o1", Namespace: "yacht-web", Name: "web"}
	one := int32(1)
	if _, err := client.AppsV1().Deployments(r.Namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: r.Name, Namespace: r.Namespace, Labels: orchestrator.ObjectLabels(r),
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r1"}},
		Spec:   appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods(r.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: r.Namespace, Labels: orchestrator.ObjectLabels(r),
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r1"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, RestartCount: 2,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := o.AppStatuses(ctx, []orchestrator.Ref{r})
	if err != nil {
		t.Fatal(err)
	}
	if st := got[r]; st.Pods != 1 || st.Restarts != 2 || st.LastExit != "code 137 (OOMKilled)" {
		t.Fatalf("pods %d, restarts %d, last exit %q; want 1, 2, code 137 (OOMKilled)", st.Pods, st.Restarts, st.LastExit)
	}
}
