package k8s

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// limitRangeName is the default LimitRange created in every namespace.
const limitRangeName = "yacht-defaults"

// EnsureNamespace creates or converges an owner's namespace.
//
// Two things always happen together here, and that coupling is intentional:
// the namespace gets Pod Security Admission labels, and it gets a LimitRange.
// A namespace without PSA labels has no enforced security floor; a namespace
// without a LimitRange lets a workload that specifies no resources run
// unbounded and evict its neighbours. Neither is offered as an option.
func (o *Orchestrator) EnsureNamespace(ctx context.Context, spec orchestrator.NamespaceSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	limits := spec.Limits.OrDefaults()

	labels := psaLabels()
	labels[orchestrator.LabelManagedBy] = orchestrator.ManagedByValue
	labels[orchestrator.LabelOwner] = string(spec.Owner)

	nsAC := corev1ac.Namespace(spec.Name).WithLabels(labels)

	if _, err := o.client.CoreV1().Namespaces().
		Apply(ctx, nsAC, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply namespace %q: %w", spec.Name, err)
	}

	if err := o.ensureLimitRange(ctx, spec.Name, limits); err != nil {
		return err
	}

	o.log.Info("namespace ready",
		slog.String("namespace", spec.Name),
		slog.String("owner", string(spec.Owner)),
	)
	return nil
}

func (o *Orchestrator) ensureLimitRange(
	ctx context.Context, namespace string, limits orchestrator.ResourceLimits,
) error {
	defaultLimit, err := resourceList(limits.DefaultCPU, limits.DefaultMemory)
	if err != nil {
		return fmt.Errorf("k8s: default limits: %w", err)
	}
	maxLimit, err := resourceList(limits.MaxCPU, limits.MaxMemory)
	if err != nil {
		return fmt.Errorf("k8s: max limits: %w", err)
	}

	lrAC := corev1ac.LimitRange(limitRangeName, namespace).
		WithLabels(map[string]string{
			orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		}).
		WithSpec(corev1ac.LimitRangeSpec().WithLimits(
			corev1ac.LimitRangeItem().
				WithType(corev1.LimitTypeContainer).
				WithDefault(defaultLimit).
				WithDefaultRequest(defaultLimit).
				WithMax(maxLimit),
		))

	if _, err := o.client.CoreV1().LimitRanges(namespace).
		Apply(ctx, lrAC, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply limitrange in %q: %w", namespace, err)
	}
	return nil
}

// DeleteNamespace removes a namespace and everything inside it.
func (o *Orchestrator) DeleteNamespace(ctx context.Context, name string) error {
	if err := orchestrator.ValidateDNSLabel("namespace", name); err != nil {
		return err
	}
	err := o.client.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete namespace %q: %w", name, err)
	}
	o.log.Info("namespace deleted", slog.String("namespace", name))
	return nil
}

// resourceList builds a Kubernetes ResourceList from quantity strings,
// skipping empty ones.
func resourceList(cpu, memory string) (corev1.ResourceList, error) {
	out := corev1.ResourceList{}
	if cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return nil, fmt.Errorf("parse cpu %q: %w", cpu, err)
		}
		out[corev1.ResourceCPU] = q
	}
	if memory != "" {
		q, err := resource.ParseQuantity(memory)
		if err != nil {
			return nil, fmt.Errorf("parse memory %q: %w", memory, err)
		}
		out[corev1.ResourceMemory] = q
	}
	return out, nil
}

// applyOpts returns the server-side apply options used everywhere.
//
// Force is on because the engine is the sole owner of the fields it sets; if
// something else has claimed one, the engine's desired state should win rather
// than the apply failing with a conflict a user cannot act on.
func applyOpts() metav1.ApplyOptions {
	return metav1.ApplyOptions{FieldManager: orchestrator.FieldManager, Force: true}
}
