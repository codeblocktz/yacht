package k8s

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Scaling is a release, but the pods it adds are the ones already running.
// Stamping the template with the scale's release made a new ReplicaSet, and
// every running pod was replaced to add one (CONFIG-9). A release that
// changes nothing at all is a Redeploy, though, and that still restarts.
func TestScalingLeavesThePodTemplateAlone(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	template := func() map[string]string {
		t.Helper()
		dep, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return dep.Spec.Template.Annotations
	}

	first := testSpec()
	if err := o.ApplyApp(ctx, first); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	before := template()

	scaled := testSpec()
	scaled.ReleaseID, scaled.ConfigVersion, scaled.Replicas = "22222222-2222-2222-2222-222222222222", 8, 3
	if err := o.ApplyApp(ctx, scaled); err != nil {
		t.Fatalf("ApplyApp scaled: %v", err)
	}
	if after := template(); !reflect.DeepEqual(after, before) {
		t.Errorf("a scale changed the pod template from %v to %v, which replaces every pod", before, after)
	}
	status, err := o.AppStatus(ctx, scaled.Ref)
	if err != nil || status.ReleaseID != scaled.ReleaseID || status.Desired != 3 {
		t.Errorf("after the scale, status = %+v, %v: want release %s at 3", status, err, scaled.ReleaseID)
	}

	redeploy := scaled
	redeploy.ReleaseID, redeploy.ConfigVersion = "33333333-3333-3333-3333-333333333333", 9
	if err := o.ApplyApp(ctx, redeploy); err != nil {
		t.Fatalf("ApplyApp redeploy: %v", err)
	}
	if got := template()[orchestrator.AnnotationReleaseID]; got != redeploy.ReleaseID {
		t.Errorf("a redeploy left the pods stamped %s, want %s: it would restart nothing", got, redeploy.ReleaseID)
	}

	changed := redeploy
	changed.ReleaseID, changed.Replicas = "44444444-4444-4444-4444-444444444444", 1
	changed.Image = "ghcr.io/example/web:v2"
	if err := o.ApplyApp(ctx, changed); err != nil {
		t.Fatalf("ApplyApp changed: %v", err)
	}
	if got := template()[orchestrator.AnnotationReleaseID]; got != changed.ReleaseID {
		t.Errorf("a new image at a new count left the pods stamped %s, want %s", got, changed.ReleaseID)
	}
}

// After a scale the Deployment is at the scale's release and its pods at the
// one that made them, and those pods are the ones whose crashes count.
func TestAScaledWorkloadsPodsAreItsTemplates(t *testing.T) {
	dep := &appsv1.Deployment{}
	status := orchestrator.AppStatus{ReleaseID: "r2"}
	if got := podRelease(dep, status); got != "r2" {
		t.Errorf("an unstamped template's pods = %q, want the Deployment's r2", got)
	}
	dep.Spec.Template.Annotations = map[string]string{orchestrator.AnnotationReleaseID: "r1"}
	if got := podRelease(dep, status); got != "r1" {
		t.Errorf("a scaled workload's pods = %q, want the template's r1", got)
	}
}
