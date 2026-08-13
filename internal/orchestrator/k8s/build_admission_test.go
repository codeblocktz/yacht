package k8s

import (
	"context"
	"testing"

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
	if got := item.Default.Cpu().String(); got != "1" {
		t.Errorf("default build cpu = %q, want 1", got)
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
