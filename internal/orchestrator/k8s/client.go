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
	"sync"

	"k8s.io/client-go/dynamic"
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

	// Isolation is the network isolation enforced between teams. The zero
	// value is team isolation.
	Isolation orchestrator.Isolation
}

// Orchestrator applies workloads to a single Kubernetes cluster.
type Orchestrator struct {
	client kubernetes.Interface

	// dynamic reaches custom resources — the k3s Helm controller's
	// HelmChartConfig, so far. Nil in tests using the fake clientset, which is
	// why every use checks: a fake cluster has no CRDs to configure.
	dynamic dynamic.Interface

	// metrics is optional. metrics-server is not installed in every K3s
	// cluster, and utilisation percentages are a nice-to-have rather than a
	// prerequisite for managing workloads.
	metrics metricsv.Interface

	// restConfig is how the client reached the cluster, kept for the one call
	// a clientset cannot make: an exec upgrades its connection to a stream,
	// which needs the transport rather than a typed client. Nil with the fake
	// clientset, which has no apiserver to stream from.
	restConfig *rest.Config

	// execStream stands in for the apiserver's exec stream in tests; nil
	// streams for real. See streamFunc.
	execStream streamFunc

	// isolation is the network isolation every namespace is given; team
	// isolation unless WithIsolation or SetIsolation says otherwise. Read
	// through Isolation: SetIsolation changes it while the engine runs.
	isolationMu sync.RWMutex
	isolation   orchestrator.Isolation
	// isolating keeps EnsureIsolation apart from the single policies a deploy
	// writes: those may run together, but never during a pass over every
	// namespace, so a deploy that read the mode before a change cannot write
	// it after the pass that brought everything into line with the new one.
	isolating sync.RWMutex

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
	withRateLimits(restCfg)

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: build clientset: %w", err)
	}

	o := NewWithClient(client, log).WithIsolation(cfg.Isolation)
	o.restConfig = restCfg

	// Best effort, like the metrics client: a cluster this cannot build a
	// dynamic client for is still perfectly usable, it just cannot be asked to
	// reconfigure things installed by an operator.
	if dc, err := dynamic.NewForConfig(restCfg); err == nil {
		o.dynamic = dc
	} else {
		log.Warn("dynamic client unavailable; custom resources cannot be configured",
			slog.String("error", err.Error()))
	}

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
	return &Orchestrator{client: client, log: log, isolation: orchestrator.Isolation{}.OrDefault()}
}

// WithMetrics attaches a metrics client, enabling utilisation reporting.
func (o *Orchestrator) WithMetrics(m metricsv.Interface) *Orchestrator {
	o.metrics = m
	return o
}

// clientQPS and clientBurst bound how fast the dashboard may call the
// cluster: client-go's defaults are 5 a second, in bursts of 10.
//
// Those are a command-line tool's, and one client serves every page every
// signed-in person has open, each polling. At 5 a second a few pages listing
// their services queued behind each other, and a page that made a dozen calls
// waited seconds on the client's own limiter with the cluster idle ("Waited
// before sending request ... client-side throttling" in the log). The API
// server still has its own priority and fairness to protect itself; these
// only stop a runaway loop here from being a flood there. kubectl allows 50.
const (
	clientQPS   = 50
	clientBurst = 100
)

// withRateLimits raises a config's client-side limits to clientQPS and
// clientBurst, leaving alone any a kubeconfig or caller already set.
func withRateLimits(c *rest.Config) {
	if c.QPS == 0 {
		c.QPS = clientQPS
	}
	if c.Burst == 0 {
		c.Burst = clientBurst
	}
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
