package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildNamespaceHasExplicitContainerBounds(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	if err := o.ensureBuildNamespace(ctx); err != nil {
		t.Fatalf("ensureBuildNamespace: %v", err)
	}
	lr, err := client.CoreV1().LimitRanges(BuildNamespace).
		Get(ctx, limitRangeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get build LimitRange: %v", err)
	}
	if len(lr.Spec.Limits) != 1 || lr.Spec.Limits[0].Type != corev1.LimitTypeContainer {
		t.Fatalf("build limits = %#v", lr.Spec.Limits)
	}
	item := lr.Spec.Limits[0]
	if got := item.Default.Cpu().String(); got != "2" {
		t.Errorf("default build cpu limit = %q, want 2", got)
	}
	// A build reserves little CPU, so a busy node still has room for one.
	if got := item.DefaultRequest.Cpu().String(); got != "250m" {
		t.Errorf("default build cpu request = %q, want 250m", got)
	}
	if got := item.DefaultRequest.Memory().String(); got != "2Gi" {
		t.Errorf("default build memory request = %q, want 2Gi", got)
	}
	if got := item.Default.Memory().String(); got != "2Gi" {
		t.Errorf("default build memory = %q, want 2Gi", got)
	}
	if got := item.Max.Cpu().String(); got != "4" {
		t.Errorf("max build cpu = %q, want 4", got)
	}
	if got := item.Max.Memory().String(); got != "8Gi" {
		t.Errorf("max build memory = %q, want 8Gi", got)
	}
}

func TestCancelBuildDeletesTheJobIdempotently(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "build-cancel", Namespace: BuildNamespace,
	}}
	if _, err := client.BatchV1().Jobs(BuildNamespace).
		Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create build Job: %v", err)
	}
	for i := range 2 {
		if err := o.CancelBuild(ctx, job.Name); err != nil {
			t.Fatalf("CancelBuild call %d: %v", i+1, err)
		}
	}
	if _, err := client.BatchV1().Jobs(BuildNamespace).
		Get(ctx, job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Job after cancellation = %v, want not found", err)
	}
}

// A build the scheduler cannot place says why in its log, in words a
// customer reads, and goes on to run when there is room.
func TestAnUnschedulableBuildSaysItIsWaitingForRoom(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "build-x-abcde", Namespace: BuildNamespace, Labels: map[string]string{"job-name": "build-x"}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/1 nodes are available: 1 Insufficient cpu. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.",
		}}},
	}
	if _, err := client.CoreV1().Pods(BuildNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var log []string
	logf := func(s string) {
		log = append(log, s)
		// Room is made the moment the wait is said: the build then starts.
		if strings.HasPrefix(s, "Waiting for room") {
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
			if _, err := client.CoreV1().Pods(BuildNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
				t.Error(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := o.waitForBuildPod(ctx, "build-x", logf); err != nil {
		t.Fatalf("waitForBuildPod: %v (log %q)", err, log)
	}
	if got := strings.Join(log, ""); !strings.Contains(got, "Waiting for room on the cluster: insufficient CPU.\n") ||
		strings.Count(got, "Waiting for room") != 1 {
		t.Errorf("log = %q, want the wait said once", got)
	}
}

func TestSchedulingWaitSaysTheSchedulersReason(t *testing.T) {
	unschedulable := func(msg string) corev1.Pod {
		return corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: msg,
		}}}}
	}
	for msg, want := range map[string]string{
		"0/1 nodes are available: 1 Insufficient cpu, 1 Insufficient memory.": "insufficient CPU, insufficient memory",
		"0/1 nodes are available: 1 node(s) had untolerated taint.":           "0/1 nodes are available: 1 node(s) had untolerated taint",
	} {
		if got := schedulingWait(unschedulable(msg)); got != want {
			t.Errorf("schedulingWait(%q) = %q, want %q", msg, got, want)
		}
	}
	if got := schedulingWait(corev1.Pod{}); got != "" {
		t.Errorf("a pod not yet considered = %q, want nothing", got)
	}
}
