package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// mounting is a pod spec that mounts the app's data claim.
func mounting(spec orchestrator.AppSpec) corev1.PodSpec {
	return corev1.PodSpec{Volumes: []corev1.Volume{{
		Name: "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: claimName(spec.Name, "data"),
		}},
	}}}
}

func mountingPod(spec orchestrator.AppSpec, name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spec.Namespace},
		Spec:       mounting(spec),
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// erase is EraseVolume and, once the claim is asked to go, WaitVolumeGone.
func erase(ctx context.Context, o *Orchestrator, ref orchestrator.Ref) (orchestrator.VolumeErasure, error) {
	e, err := o.EraseVolume(ctx, ref, "data")
	if err != nil || !e.Deleted {
		return e, err
	}
	return e, o.WaitVolumeGone(ctx, ref, "data")
}

// stoppedWithClaim is an app applied at one replica, so its claim exists,
// and then stopped, with its Deployment at zero.
func stoppedWithClaim(t *testing.T) (*Orchestrator, orchestrator.AppSpec, func() error) {
	t.Helper()
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := specWithVolume()
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	spec.Replicas = 0
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	claimGone := func() error {
		_, err := client.CoreV1().PersistentVolumeClaims(spec.Namespace).
			Get(ctx, claimName(spec.Name, "data"), metav1.GetOptions{})
		return err
	}
	return o, spec, claimGone
}

// Erasing deletes the claim; a stopped app is not given a new one, and its
// next start is, of the same size, none of the old one's. Erasing a claim
// already gone does nothing.
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

	// Running, its Deployment at one replica with no pod yet: in use.
	if _, err := erase(ctx, o, spec.Ref); !errors.Is(err, orchestrator.ErrVolumeInUse) ||
		!strings.Contains(err.Error(), "deployment") {
		t.Fatalf("erasing a running app's volume: %v, want in use by its deployment", err)
	}

	spec.Replicas = 0
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("a stopped app's claim: %v", err)
	}
	if e, err := erase(ctx, o, spec.Ref); err != nil || !e.Deleted || e.Claim != name {
		t.Fatalf("EraseVolume: %+v, %v", e, err)
	}
	if _, err := claims.Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("after erasing, the claim: %v", err)
	}

	// Still stopped: nothing made for it, and nothing to erase.
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a stopped app was given a new claim: %v", err)
	}
	if e, err := erase(ctx, o, spec.Ref); err != nil || e.Deleted {
		t.Fatalf("erasing a volume already erased: %+v, %v", e, err)
	}

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

// Anything that mounts the claim, or may start something that does, keeps
// it: a pod pending, running, terminating or retried by a Job; a CronJob not
// suspended; a Job not finished. A finished pod or Job, or a suspended
// CronJob, does not.
func TestEraseVolumeIsRefusedWhileAnythingMayMountIt(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	terminating := mountingPod(specWithVolume(), "web-old", corev1.PodRunning)
	terminating.DeletionTimestamp, terminating.Finalizers = &now, []string{"test/hold"}
	retry := mountingPod(specWithVolume(), "migrate-x7k2p", corev1.PodPending)
	retry.Labels = map[string]string{"job-name": "migrate"}

	suspended, running := true, false
	for name, c := range map[string]struct {
		objects []runtime.Object
		in      string
	}{
		"pending pod":     {[]runtime.Object{mountingPod(specWithVolume(), "web-1", corev1.PodPending)}, "pod web-1"},
		"running pod":     {[]runtime.Object{mountingPod(specWithVolume(), "web-1", corev1.PodRunning)}, "pod web-1"},
		"terminating pod": {[]runtime.Object{terminating}, "pod web-old"},
		"job retry pod":   {[]runtime.Object{retry}, "pod migrate-x7k2p"},
		"cronjob": {[]runtime.Object{&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "yacht-demo"},
			Spec: batchv1.CronJobSpec{Suspend: &running, JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: mounting(specWithVolume())}}}},
		}}, "cronjob web"},
		"job": {[]runtime.Object{&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "yacht-demo"},
			Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: mounting(specWithVolume())}},
		}}, "job migrate"},
		"finished": {[]runtime.Object{
			mountingPod(specWithVolume(), "web-done", corev1.PodSucceeded),
			mountingPod(specWithVolume(), "web-crashed", corev1.PodFailed),
			&batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "yacht-demo"},
				Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: mounting(specWithVolume())}},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
			},
			&batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "yacht-demo"},
				Spec: batchv1.CronJobSpec{Suspend: &suspended, JobTemplate: batchv1.JobTemplateSpec{
					Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: mounting(specWithVolume())}}}},
			},
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			o, spec, claim := stoppedWithClaim(t)
			client := o.client
			for _, obj := range c.objects {
				var err error
				switch v := obj.(type) {
				case *corev1.Pod:
					_, err = client.CoreV1().Pods(spec.Namespace).Create(ctx, v, metav1.CreateOptions{})
				case *batchv1.Job:
					_, err = client.BatchV1().Jobs(spec.Namespace).Create(ctx, v, metav1.CreateOptions{})
				case *batchv1.CronJob:
					_, err = client.BatchV1().CronJobs(spec.Namespace).Create(ctx, v, metav1.CreateOptions{})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := erase(ctx, o, spec.Ref)
			if c.in == "" {
				if err != nil || !apierrors.IsNotFound(claim()) {
					t.Fatalf("with only finished users: %v, claim %v", err, claim())
				}
				return
			}
			if !errors.Is(err, orchestrator.ErrVolumeInUse) || !strings.Contains(err.Error(), c.in) {
				t.Fatalf("erasing: %v, want in use by %s", err, c.in)
			}
			if claim() != nil {
				t.Fatalf("a claim in use was deleted: %v", claim())
			}
		})
	}
}

// A pod that starts between the look and the delete is found after: the
// claim is Terminating, and the error says so.
func TestEraseVolumeSaysWhenAPodStartedInBetween(t *testing.T) {
	ctx := context.Background()
	o, spec, _ := stoppedWithClaim(t)
	client := o.client.(interface {
		PrependReactor(verb, resource string, fn k8stesting.ReactionFunc)
		Tracker() k8stesting.ObjectTracker
	})
	client.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		pod := mountingPod(spec, "web-late", corev1.PodPending)
		return false, nil, client.Tracker().Create(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, pod, spec.Namespace)
	})
	e, err := o.EraseVolume(ctx, spec.Ref, "data")
	if !errors.Is(err, orchestrator.ErrVolumeInUse) || !strings.Contains(err.Error(), "web-late") ||
		!strings.Contains(err.Error(), "Terminating") || !e.Deleted {
		t.Fatalf("a pod started in between: %+v, %v", e, err)
	}
}

// The delete names the claim that was read, by its uid: one made since under
// the same name is not deleted, and the conflict is said.
func TestEraseVolumeDeletesOnlyTheClaimItRead(t *testing.T) {
	ctx := context.Background()
	o, spec, _ := stoppedWithClaim(t)
	claims := o.client.CoreV1().PersistentVolumeClaims(spec.Namespace)
	c, _ := claims.Get(ctx, claimName(spec.Name, "data"), metav1.GetOptions{})
	c.UID = types.UID("claim-uid-1")
	if _, err := claims.Update(ctx, c, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	var sawUID string
	o.client.(interface {
		PrependReactor(verb, resource string, fn k8stesting.ReactionFunc)
	}).PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if p := a.(k8stesting.DeleteAction).GetDeleteOptions().Preconditions; p != nil && p.UID != nil {
			sawUID = string(*p.UID)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "persistentvolumeclaims"},
			c.Name, errors.New("uid precondition failed"))
	})
	_, err := o.EraseVolume(ctx, spec.Ref, "data")
	if sawUID != "claim-uid-1" || err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("delete preconditioned on %q: %v", sawUID, err)
	}
}

// A claim already being deleted is not deleted again, and is waited for —
// for no longer than eraseWait, after which the call says so. A start meets
// it as a volume still being deleted, not one to mount.
func TestATerminatingClaimIsWaitedForAndNotMounted(t *testing.T) {
	ctx := context.Background()
	o, spec, _ := stoppedWithClaim(t)
	claims := o.client.CoreV1().PersistentVolumeClaims(spec.Namespace)
	c, _ := claims.Get(ctx, claimName(spec.Name, "data"), metav1.GetOptions{})
	now := metav1.Now()
	c.DeletionTimestamp, c.Finalizers = &now, []string{"kubernetes.io/pvc-protection"}
	if _, err := claims.Update(ctx, c, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	saved := eraseWait
	eraseWait = 50 * time.Millisecond
	t.Cleanup(func() { eraseWait = saved })

	e, err := o.EraseVolume(ctx, spec.Ref, "data")
	if err != nil || !e.Deleted {
		t.Fatalf("erasing a claim being deleted: %+v, %v", e, err)
	}
	if err := o.WaitVolumeGone(ctx, spec.Ref, "data"); !errors.Is(err, orchestrator.ErrVolumeDeleting) {
		t.Fatalf("waiting past eraseWait: %v", err)
	}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("a stopped app with a claim being deleted: %v", err)
	}
	spec.Replicas = 1
	if err := o.ApplyApp(ctx, spec); !errors.Is(err, orchestrator.ErrVolumeDeleting) {
		t.Fatalf("starting on a claim being deleted: %v, want ErrVolumeDeleting", err)
	}
}

// Storage kept when its claim is deleted would be erased in name only:
// refused, the claim left as it is. Storage deleted with it is erased, and
// so is a claim bound to nothing, or to a volume that is not there.
func TestEraseVolumeRefusesStorageThatOutlivesItsClaim(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		policy    corev1.PersistentVolumeReclaimPolicy
		volume    string
		makeVol   bool
		wantError error
	}{
		"retain":     {corev1.PersistentVolumeReclaimRetain, "pv-retain", true, orchestrator.ErrVolumeRetained},
		"delete":     {corev1.PersistentVolumeReclaimDelete, "pv-delete", true, nil},
		"unbound":    {"", "", false, nil},
		"pv missing": {"", "pv-gone", false, nil},
	} {
		t.Run(name, func(t *testing.T) {
			o, client := testOrchestrator(t)
			spec := specWithVolume()
			if c.makeVol {
				if _, err := client.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: c.volume},
					Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: c.policy},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			claims := client.CoreV1().PersistentVolumeClaims(spec.Namespace)
			if _, err := claims.Create(ctx, &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: claimName(spec.Name, "data"), Namespace: spec.Namespace},
				Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: c.volume},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			e, err := erase(ctx, o, spec.Ref)
			_, getErr := claims.Get(ctx, claimName(spec.Name, "data"), metav1.GetOptions{})
			if c.wantError != nil {
				if !errors.Is(err, c.wantError) || getErr != nil {
					t.Fatalf("%v, claim %v", err, getErr)
				}
				return
			}
			if err != nil || !apierrors.IsNotFound(getErr) || e.PersistentVolume != c.volume {
				t.Fatalf("%+v, %v, claim %v", e, err, getErr)
			}
		})
	}
}

// An asleep app runs no pods, and keeps the claim it has, grown when asked.
func TestAnAsleepAppKeepsAndGrowsItsClaim(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := specWithVolume()
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	spec.Replicas = 0
	spec.Volumes[0].SizeBytes = 4 << 30
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatal(err)
	}
	c, err := client.CoreV1().PersistentVolumeClaims(spec.Namespace).
		Get(ctx, claimName(spec.Name, "data"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("an asleep app's claim: %v", err)
	}
	if got := c.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(*resource.NewQuantity(4<<30, resource.BinarySI)) != 0 {
		t.Errorf("an asleep app's claim asks for %s, want 4Gi", got.String())
	}
}
