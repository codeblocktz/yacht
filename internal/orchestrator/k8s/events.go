package k8s

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Events lists recent cluster events, newest first.
//
// Warnings are hoisted above Normal events regardless of age. A cluster emits
// a constant stream of routine Scheduled/Pulled/Created messages, and the one
// FailedScheduling that explains why nothing is running would otherwise be
// buried under them.
func (o *Orchestrator) Events(
	ctx context.Context, limit int,
) ([]orchestrator.EventInfo, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	list, err := o.client.CoreV1().Events("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: list events: %w", err)
	}

	out := make([]orchestrator.EventInfo, 0, len(list.Items))
	for _, e := range list.Items {
		out = append(out, orchestrator.EventInfo{
			Namespace: e.Namespace,
			Type:      e.Type,
			Reason:    e.Reason,
			Message:   e.Message,
			Object:    describeObject(e.InvolvedObject),
			Count:     e.Count,
			LastSeen:  eventTime(e),
		})
	}

	slices.SortFunc(out, func(a, b orchestrator.EventInfo) int {
		if a.Warning() != b.Warning() {
			if a.Warning() {
				return -1
			}
			return 1
		}
		return b.LastSeen.Compare(a.LastSeen)
	})

	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// eventTime picks the most recent timestamp an event carries.
//
// Kubernetes populates these inconsistently depending on which API wrote the
// event, and an event that sorts to 1970 because the obvious field was empty
// looks like a bug in the dashboard.
func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

func describeObject(ref corev1.ObjectReference) string {
	if ref.Kind == "" {
		return ref.Name
	}
	if ref.Name == "" {
		return ref.Kind
	}
	return ref.Kind + "/" + ref.Name
}

// Volumes lists persistent volume claims.
func (o *Orchestrator) Volumes(ctx context.Context) ([]orchestrator.VolumeInfo, error) {
	list, err := o.client.CoreV1().PersistentVolumeClaims("").
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: list volumes: %w", err)
	}

	out := make([]orchestrator.VolumeInfo, 0, len(list.Items))
	for _, pvc := range list.Items {
		info := orchestrator.VolumeInfo{
			Name:      pvc.Name,
			Namespace: pvc.Namespace,
			Phase:     string(pvc.Status.Phase),
			App:       pvc.Labels[orchestrator.LabelApp],
			Owner:     orchestrator.OwnerID(pvc.Labels[orchestrator.LabelOwner]),
			CreatedAt: pvc.CreationTimestamp.Time,
		}
		if pvc.Spec.StorageClassName != nil {
			info.StorageClass = *pvc.Spec.StorageClassName
		}
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			info.CapacityBytes = q.Value()
		}
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			info.RequestBytes = q.Value()
		}
		for _, m := range pvc.Spec.AccessModes {
			info.AccessModes = append(info.AccessModes, shortAccessMode(m))
		}
		out = append(out, info)
	}

	slices.SortFunc(out, func(a, b orchestrator.VolumeInfo) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// shortAccessMode renders the conventional kubectl abbreviations, which is
// what operators recognise.
func shortAccessMode(m corev1.PersistentVolumeAccessMode) string {
	switch m {
	case corev1.ReadWriteOnce:
		return "RWO"
	case corev1.ReadOnlyMany:
		return "ROX"
	case corev1.ReadWriteMany:
		return "RWX"
	case corev1.ReadWriteOncePod:
		return "RWOP"
	}
	return string(m)
}
