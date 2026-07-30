// Package k8s implements orchestrator.Orchestrator against a Kubernetes
// cluster (K3s in the common case).
//
// Everything here is single-cluster on purpose. Multi-cluster placement is a
// different implementation of the same interface and belongs to whatever wraps
// the engine, not to the engine.
package k8s

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Config selects how to reach the cluster.
type Config struct {
	// InCluster uses the service account mounted into the pod. Takes
	// precedence over Kubeconfig.
	InCluster bool

	// Kubeconfig is a path to a kubeconfig file. Empty falls back to the
	// standard client-go loading rules (KUBECONFIG, then ~/.kube/config).
	Kubeconfig string
}

// Orchestrator applies workloads to a single Kubernetes cluster.
type Orchestrator struct {
	client kubernetes.Interface

	// metrics is optional. metrics-server is not installed in every K3s
	// cluster, and utilisation percentages are a nice-to-have rather than a
	// prerequisite for managing workloads.
	metrics metricsv.Interface

	log *slog.Logger
}

// Compile-time check that this satisfies the seam.
var _ orchestrator.Orchestrator = (*Orchestrator)(nil)

// New connects to a cluster and verifies it is reachable.
//
// Connecting eagerly is deliberate: a misconfigured cluster should fail at
// startup with a clear message, not at a customer's first deploy.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Orchestrator, error) {
	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, err
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: build clientset: %w", err)
	}

	o := NewWithClient(client, log)

	// Best effort: a cluster without metrics-server is still perfectly
	// usable, so failing to build this client must not fail startup.
	if mc, err := metricsv.NewForConfig(restCfg); err == nil {
		o.metrics = mc
	} else {
		log.Warn("metrics client unavailable; utilisation will not be shown",
			slog.String("error", err.Error()))
	}

	if err := o.Ping(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// NewWithClient wraps an existing clientset. This is the constructor tests use
// with client-go's fake clientset.
func NewWithClient(client kubernetes.Interface, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{client: client, log: log}
}

// WithMetrics attaches a metrics client, enabling utilisation reporting.
func (o *Orchestrator) WithMetrics(m metricsv.Interface) *Orchestrator {
	o.metrics = m
	return o
}

func restConfig(cfg Config) (*rest.Config, error) {
	if cfg.InCluster {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("k8s: in-cluster config: %w", err)
		}
		return c, nil
	}

	if cfg.Kubeconfig != "" {
		c, err := clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("k8s: load kubeconfig %q: %w", cfg.Kubeconfig, err)
		}
		return c, nil
	}

	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s: load default kubeconfig: %w", err)
	}
	return c, nil
}

// Ping verifies the cluster answers.
func (o *Orchestrator) Ping(_ context.Context) error {
	v, err := o.client.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("k8s: unreachable: %w", err)
	}
	o.log.Debug("cluster reachable", slog.String("version", v.String()))
	return nil
}
