package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

var _ orchestrator.NodeManager = (*Orchestrator)(nil)

// Cordon stops or resumes scheduling onto a node.
//
// A merge patch rather than a read-modify-write: two operators cordoning at
// once should not have one silently undo the other's unrelated edit, and the
// only field being asserted is this one.
func (o *Orchestrator) Cordon(ctx context.Context, node string, unschedulable bool) error {
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, unschedulable)

	_, err := o.client.CoreV1().Nodes().
		Patch(ctx, node, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: no node %q: %w", node, orchestrator.ErrNotFound)
		}
		return fmt.Errorf("k8s: cordon %s: %w", node, err)
	}
	return nil
}

// Drain asks the pods on a node to leave.
//
// Eviction rather than deletion, so a pod covered by a disruption budget is
// refused rather than taken down anyway — the budget exists precisely to stop
// an operator emptying a machine from taking a service down with it. A refusal
// is reported to the caller, not swallowed: a drain that quietly skipped what
// it could not move would look complete while the node was still serving.
func (o *Orchestrator) Drain(ctx context.Context, node string) (int, error) {
	pods, err := o.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return 0, fmt.Errorf("k8s: list pods on %s: %w", node, err)
	}

	requested := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if skipOnDrain(pod) {
			continue
		}

		err := o.client.PolicyV1().Evictions(pod.Namespace).Evict(ctx, &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		})
		switch {
		case err == nil:
			requested++
		case apierrors.IsNotFound(err):
			// Gone between the list and the eviction, which is the outcome
			// asked for.
		case apierrors.IsTooManyRequests(err):
			// A disruption budget refused. Named, because the operator has to
			// decide whether to wait or to change the budget, and neither is
			// something this should decide for them.
			return requested, fmt.Errorf(
				"k8s: %s/%s cannot be evicted without breaking its disruption budget",
				pod.Namespace, pod.Name)
		default:
			return requested, fmt.Errorf("k8s: evict %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return requested, nil
}

// skipOnDrain reports whether a pod should be left where it is.
//
// Two kinds stay. A DaemonSet pod is meant to run on every node and would be
// recreated on this one immediately, so evicting it is a loop rather than a
// drain. A mirror pod is a static pod the kubelet owns from a file on disk;
// the API object is a reflection, and deleting it does not stop the container.
func skipOnDrain(pod *corev1.Pod) bool {
	if _, mirrored := pod.Annotations[corev1.MirrorPodAnnotationKey]; mirrored {
		return true
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	// Already finished. Evicting a completed pod achieves nothing and counts
	// toward a total that is meant to mean "still to move".
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// DeleteNode removes the node object from the cluster.
func (o *Orchestrator) DeleteNode(ctx context.Context, node string) error {
	err := o.client.CoreV1().Nodes().Delete(ctx, node, metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("k8s: no node %q: %w", node, orchestrator.ErrNotFound)
		}
		return fmt.Errorf("k8s: delete node %s: %w", node, err)
	}
	return nil
}
