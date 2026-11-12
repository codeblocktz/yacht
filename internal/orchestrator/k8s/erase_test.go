package k8s

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// mountingPod is a pod in the app's namespace that mounts its data claim.
func mountingPod(spec orchestrator.AppSpec, name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spec.Namespace},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: claimName(spec.Name, "data"),
			}},
		}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// Erasing deletes the claim — refused while a pod still mounts it — and a
// stopped app is not given a new one; its next start is, empty. Erasing a
// claim already gone does nothing.
func TestEraseVolumeDeletesTheClaimAndTheNextStartMakesANewOne(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := specWithVolume()
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	claims := client.CoreV1().PersistentVolumeClaims(spec.Namespace)
	name := claimName(spec.Name, "data")
	old, err := claims.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	old.Annotations = map[string]string{"test/old": "data"}
	if _, err := claims.Update(ctx, old, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	pods := client.CoreV1().Pods(spec.Namespace)
	if _, err := pods.Create(ctx, mountingPod(spec, "web-1", corev1.PodRunning), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pods.Create(ctx, mountingPod(spec, "web-0", corev1.PodSucceeded), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := o.EraseVolume(ctx, spec.Ref, "data"); !errors.Is(err, orchestrator.ErrVolumeInUse) {
		t.Fatalf("erasing a mounted volume: %v, want ErrVolumeInUse", err)
	}
	if _, err := claims.Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("a mounted volume's claim: %v", err)
	}

	// Stopped, its pod gone: the claim is kept by a stopped app until erased.
	if err := pods.Delete(ctx, "web-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	spec.Replicas = 0
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("a stopped app's claim: %v", err)
	}
	if err := o.EraseVolume(ctx, spec.Ref, "data"); err != nil {
		t.Fatalf("EraseVolume: %v", err)
	}
	if _, err := claims.Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("after erasing, the claim: %v", err)
	}

	// Still stopped: nothing made for it.
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a stopped app was given a new claim: %v", err)
	}
	if err := o.EraseVolume(ctx, spec.Ref, "data"); err != nil {
		t.Fatalf("erasing a volume already erased: %v", err)
	}

	// Started: a new claim, the same size, none of the old one's.
	spec.Replicas = 1
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	fresh, err := claims.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("after starting, the claim: %v", err)
	}
	if fresh.Annotations["test/old"] != "" {
		t.Error("the started app was given the old claim back")
	}
	if got := fresh.Spec.Resources.Requests[corev1.ResourceStorage]; got.Value() != spec.Volumes[0].SizeBytes {
		t.Errorf("the new claim asks for %s, want %d bytes", got.String(), spec.Volumes[0].SizeBytes)
	}
}

// Storage kept when its claim is deleted would be erased in name only:
// refused, the claim left as it is. Storage deleted with it is erased.
func TestEraseVolumeRefusesStorageThatOutlivesItsClaim(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := specWithVolume()
	spec.Replicas = 0
	name := claimName(spec.Name, "data")
	claims := client.CoreV1().PersistentVolumeClaims(spec.Namespace)
	for _, policy := range []corev1.PersistentVolumeReclaimPolicy{
		corev1.PersistentVolumeReclaimRetain, corev1.PersistentVolumeReclaimDelete,
	} {
		pvName := "pv-" + string(policy)
		if _, err := client.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pvName},
			Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: policy},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := claims.Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spec.Namespace},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		err := o.EraseVolume(ctx, spec.Ref, "data")
		_, getErr := claims.Get(ctx, name, metav1.GetOptions{})
		switch policy {
		case corev1.PersistentVolumeReclaimRetain:
			if !errors.Is(err, orchestrator.ErrVolumeRetained) || getErr != nil {
				t.Fatalf("a retained volume: %v, claim %v", err, getErr)
			}
			if err := claims.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		default:
			if err != nil || !apierrors.IsNotFound(getErr) {
				t.Fatalf("a volume deleted with its claim: %v, claim %v", err, getErr)
			}
		}
	}
}
