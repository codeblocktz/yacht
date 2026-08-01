package k8s

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func TestEventsHoistWarnings(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	now := time.Now()

	seed := []*corev1.Event{
		// Newest, but routine.
		{
			ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "ns-a"},
			Type:           "Normal",
			Reason:         "Pulled",
			Message:        "Successfully pulled image",
			LastTimestamp:  metav1.NewTime(now.Add(-1 * time.Minute)),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-abc"},
			Count:          1,
		},
		// Older, but the one that explains the outage.
		{
			ObjectMeta:     metav1.ObjectMeta{Name: "e2", Namespace: "ns-b"},
			Type:           "Warning",
			Reason:         "FailedScheduling",
			Message:        "0/3 nodes are available: insufficient cpu",
			LastTimestamp:  metav1.NewTime(now.Add(-30 * time.Minute)),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-xyz"},
			Count:          9,
		},
	}
	for _, e := range seed {
		if _, err := client.CoreV1().Events(e.Namespace).
			Create(ctx, e, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	got, err := o.Events(ctx, 50)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}

	// A cluster emits routine events constantly. If they sort purely by time,
	// the one FailedScheduling that matters is buried.
	if !got[0].Warning() {
		t.Errorf("first event = %+v, want the warning", got[0])
	}
	if got[0].Reason != "FailedScheduling" {
		t.Errorf("reason = %q, want FailedScheduling", got[0].Reason)
	}
	if got[0].Object != "Pod/api-xyz" {
		t.Errorf("object = %q, want Pod/api-xyz", got[0].Object)
	}
	if got[0].Count != 9 {
		t.Errorf("count = %d, want 9", got[0].Count)
	}
}

func TestEventsRespectLimit(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	for i := range 12 {
		e := &corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Name: fmt.Sprintf("e%d", i), Namespace: "ns"},
			Type:          "Normal",
			Reason:        "Pulled",
			LastTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(i) * time.Minute)),
		}
		if _, err := client.CoreV1().Events("ns").
			Create(ctx, e, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := o.Events(ctx, 5)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("got %d events, want the limit of 5", len(got))
	}
}

// Kubernetes populates event timestamps inconsistently depending on which API
// wrote them. An event that sorts to 1970 because the obvious field was empty
// looks like a bug in the dashboard.
func TestEventTimeFallsBackAcrossFields(t *testing.T) {
	now := time.Now().Truncate(time.Second)

	onlyEventTime := corev1.Event{EventTime: metav1.NewMicroTime(now)}
	if got := eventTime(onlyEventTime); !got.Equal(now) {
		t.Errorf("EventTime fallback = %v, want %v", got, now)
	}

	onlyFirst := corev1.Event{FirstTimestamp: metav1.NewTime(now)}
	if got := eventTime(onlyFirst); !got.Equal(now) {
		t.Errorf("FirstTimestamp fallback = %v, want %v", got, now)
	}

	onlyCreation := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now)},
	}
	if got := eventTime(onlyCreation); !got.Equal(now) {
		t.Errorf("CreationTimestamp fallback = %v, want %v", got, now)
	}
}

func TestVolumes(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	class := "local-path"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data-web-0", Namespace: "yacht-a1b2",
			Labels: map[string]string{
				orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
				orchestrator.LabelApp:       "web",
				orchestrator.LabelOwner:     "owner-local",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &class,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("8Gi"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("8Gi"),
			},
		},
	}
	if _, err := client.CoreV1().PersistentVolumeClaims("yacht-a1b2").
		Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pvc: %v", err)
	}

	got, err := o.Volumes(ctx, "owner-local")
	if err != nil {
		t.Fatalf("Volumes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d volumes, want 1", len(got))
	}

	v := got[0]
	if !v.Bound() {
		t.Errorf("phase = %q, want Bound", v.Phase)
	}
	if v.StorageClass != "local-path" {
		t.Errorf("class = %q, want local-path", v.StorageClass)
	}
	if v.CapacityBytes != 8<<30 {
		t.Errorf("capacity = %d, want %d", v.CapacityBytes, int64(8)<<30)
	}
	if v.App != "web" || v.Owner != "owner-local" {
		t.Errorf("labels not carried through: app=%q owner=%q", v.App, v.Owner)
	}
	if len(v.AccessModes) != 1 || v.AccessModes[0] != "RWO" {
		t.Errorf("access modes = %v, want [RWO]", v.AccessModes)
	}
}

func TestShortAccessMode(t *testing.T) {
	cases := map[corev1.PersistentVolumeAccessMode]string{
		corev1.ReadWriteOnce:    "RWO",
		corev1.ReadOnlyMany:     "ROX",
		corev1.ReadWriteMany:    "RWX",
		corev1.ReadWriteOncePod: "RWOP",
	}
	for in, want := range cases {
		if got := shortAccessMode(in); got != want {
			t.Errorf("shortAccessMode(%v) = %q, want %q", in, got, want)
		}
	}
}
