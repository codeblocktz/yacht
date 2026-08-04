package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// helmChartConfig is how k3s lets a chart it installed be reconfigured.
//
// Applying one makes the k3s Helm controller redeploy Traefik with the values
// merged in. It is the mechanism k3s documents for exactly this, rather than
// editing a Deployment the controller would revert on the next reconcile.
var helmChartConfig = schema.GroupVersionResource{
	Group:    "helm.cattle.io",
	Version:  "v1",
	Resource: "helmchartconfigs",
}

// EnableHTTPLogs switches the ingress controller's access log on.
//
// Called once at startup, not from a page. It restarts a controller every
// workload in the cluster routes through, so it is announced in the log rather
// than offered as a button — a cluster-wide restart is not something to ask one
// app's owner to accept on everybody else's behalf, and an app whose requests
// go unrecorded until somebody finds that button is an app nobody can debug.
//
// Only where k3s installed Traefik. Elsewhere the controller was installed by
// something Yacht knows nothing about — a Helm release, an operator, a
// hand-written manifest — and writing a HelmChartConfig would create an object
// nothing reads, which looks like it worked and changes nothing.
func (o *Orchestrator) EnableHTTPLogs(ctx context.Context) error {
	if o.dynamic == nil {
		return fmt.Errorf("%w: no dynamic client", orchestrator.ErrNotSupported)
	}

	// The CRD is the test for "k3s installed this". Its absence is not a
	// failure to report as one — it is the answer that this cluster's ingress
	// controller is somebody else's to configure.
	if _, err := o.client.Discovery().ServerResourcesForGroupVersion("helm.cattle.io/v1"); err != nil {
		return fmt.Errorf("%w: this cluster's ingress controller was not installed by k3s, "+
			"so its access log is configured wherever it was installed from",
			orchestrator.ErrNotSupported)
	}

	cfg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChartConfig",
		"metadata": map[string]any{
			"name":      "traefik",
			"namespace": "kube-system",
			"labels": map[string]any{
				orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
			},
		},
		"spec": map[string]any{
			// Headers are dropped. An access log that keeps them records
			// Authorization and Cookie for every request through the cluster,
			// into a log this platform then shows people — which would turn a
			// request log into a credential store.
			"valuesContent": "logs:\n" +
				"  access:\n" +
				"    enabled: true\n" +
				"    format: json\n" +
				"    fields:\n" +
				"      headers:\n" +
				"        defaultMode: drop\n",
		},
	}}

	data, err := cfg.MarshalJSON()
	if err != nil {
		return fmt.Errorf("k8s: encode helm chart config: %w", err)
	}

	// Server-side apply, so this owns only the fields it sets and re-running
	// it is not a conflict. Anything else somebody configured on the same
	// object stays theirs.
	_, err = o.dynamic.Resource(helmChartConfig).Namespace("kube-system").
		Patch(ctx, "traefik", types.ApplyPatchType, data, metav1.PatchOptions{
			FieldManager: orchestrator.ManagedByValue,
			Force:        boolPtr(true),
		})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return fmt.Errorf("%w: this install is not allowed to configure the "+
				"ingress controller", orchestrator.ErrNotSupported)
		}
		return fmt.Errorf("k8s: enable the access log: %w", err)
	}

	o.log.Info("ingress access logging enabled — traefik will restart")
	return nil
}

// HTTPLogsEnabled reports whether the access log is already on.
//
// Read from the controller's own arguments rather than from the config that
// was applied, because those are two different questions: the config says what
// was asked for, and the arguments say what is running. Between them sits a
// restart that may not have happened yet.
func (o *Orchestrator) HTTPLogsEnabled(ctx context.Context) bool {
	pods, ns, err := o.ingressPods(ctx)
	if err != nil || len(pods) == 0 {
		return false
	}
	pod, err := o.client.CoreV1().Pods(ns).Get(ctx, pods[0], metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, c := range pod.Spec.Containers {
		for _, arg := range c.Args {
			if arg == "--accesslog=true" || arg == "--accesslog" {
				return true
			}
		}
	}
	return false
}

// CanConfigureIngress reports whether this cluster's ingress controller is one
// Yacht can reconfigure.
//
// The k3s Helm controller's CRD is the whole test: it is there when k3s
// installed Traefik and absent when something else did, and writing a
// HelmChartConfig in the second case creates an object nothing reads — which
// looks like success and changes nothing.
func (o *Orchestrator) CanConfigureIngress(ctx context.Context) bool {
	if o.dynamic == nil {
		return false
	}
	_, err := o.client.Discovery().ServerResourcesForGroupVersion("helm.cattle.io/v1")
	return err == nil
}
