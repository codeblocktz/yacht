// Package orchestrator defines the contract between Yacht's control plane and
// whatever actually runs workloads.
//
// SEAM 1 of 4. This interface is the primary extension point of the engine.
// The engine ships a single-cluster Kubernetes implementation (subpackage k8s)
// and a no-op implementation for tests. A wrapping application can supply its
// own — for example one that selects a cluster from a registry and applies
// per-owner scheduling — without the engine knowing anything about it.
//
// Two rules keep this seam usable:
//
//  1. No Kubernetes types appear in this package. Everything crossing the
//     boundary is a plain Go type defined here. An implementation backed by
//     something other than Kubernetes stays possible, and callers never import
//     client-go.
//  2. Naming policy lives with the caller, not the implementation. The caller
//     decides namespaces and resource names; implementations only apply them.
//     A wrapping layer needs different naming, and this is what lets it.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// ErrNotFound is returned when a requested workload does not exist.
var ErrNotFound = errors.New("orchestrator: not found")

// OwnerID identifies who owns a resource.
//
// The engine runs with exactly one owner (see package identity). A wrapping
// application maps this to whatever its own principal is. The orchestrator
// never interprets the value: it labels resources with it and otherwise treats
// it as opaque.
type OwnerID string

// Ref uniquely identifies a workload.
type Ref struct {
	Owner     OwnerID
	Namespace string
	Name      string
}

func (r Ref) String() string { return fmt.Sprintf("%s/%s", r.Namespace, r.Name) }

// Validate checks that a Ref is well formed and safe to send to a cluster.
func (r Ref) Validate() error {
	if r.Owner == "" {
		return errors.New("ref: owner is required")
	}
	if err := ValidateDNSLabel("namespace", r.Namespace); err != nil {
		return err
	}
	return ValidateDNSLabel("name", r.Name)
}

// dnsLabel matches RFC 1123 DNS labels, which is what Kubernetes requires for
// most object names.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// hostRE matches a dotted DNS name. Deliberately strict: no scheme, no port,
// no path, no uppercase. Anything that arrives here in one of those shapes is
// a caller that has confused a URL with a hostname, and failing early is
// cheaper than an Ingress that silently never matches.
var hostRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

// ValidateDNSLabel reports whether s is a legal Kubernetes object name.
//
// Callers are expected to generate names rather than pass user input straight
// through, but this is the backstop: a name that reaches a cluster and collides
// with another owner's resource is the failure mode that matters most, so the
// check is here at the boundary rather than trusted to every caller.
func ValidateDNSLabel(field, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s: is required", field)
	case len(s) > 63:
		return fmt.Errorf("%s: must be at most 63 characters, got %d", field, len(s))
	case !dnsLabel.MatchString(s):
		return fmt.Errorf("%s: %q must be a lowercase RFC 1123 label", field, s)
	}
	return nil
}

// NamespaceSpec describes an isolation boundary for one owner's workloads.
type NamespaceSpec struct {
	Owner OwnerID
	Name  string

	// Limits become a default LimitRange in the namespace, so a workload that
	// specifies no resources of its own still cannot run unbounded. Zero value
	// means DefaultLimits is used; it is deliberately not possible to create a
	// namespace with no limits at all.
	Limits ResourceLimits
}

// Validate checks the spec.
func (s NamespaceSpec) Validate() error {
	if s.Owner == "" {
		return errors.New("namespace spec: owner is required")
	}
	return ValidateDNSLabel("namespace", s.Name)
}

// ResourceLimits expresses CPU and memory bounds using Kubernetes quantity
// strings ("500m", "512Mi").
type ResourceLimits struct {
	DefaultCPU    string // applied to containers that request nothing
	DefaultMemory string
	MaxCPU        string // ceiling any single container may request
	MaxMemory     string
}

// DefaultLimits is the fallback applied when a NamespaceSpec leaves Limits
// empty. Modest on purpose: a self-hoster running a single node should not have
// one runaway container evict everything else.
var DefaultLimits = ResourceLimits{
	DefaultCPU:    "100m",
	DefaultMemory: "128Mi",
	MaxCPU:        "2",
	MaxMemory:     "4Gi",
}

// OrEmpty returns l, substituting DefaultLimits for any unset field.
func (l ResourceLimits) OrDefaults() ResourceLimits {
	if l.DefaultCPU == "" {
		l.DefaultCPU = DefaultLimits.DefaultCPU
	}
	if l.DefaultMemory == "" {
		l.DefaultMemory = DefaultLimits.DefaultMemory
	}
	if l.MaxCPU == "" {
		l.MaxCPU = DefaultLimits.MaxCPU
	}
	if l.MaxMemory == "" {
		l.MaxMemory = DefaultLimits.MaxMemory
	}
	return l
}

// AppSpec is the desired state of a long-running workload.
//
// Note what is absent: there is no field for privileged execution, host
// networking, host paths, or a service account token. Those are not omissions
// to be filled in later — the engine does not offer them, and the Kubernetes
// implementation hard-codes a restricted security context regardless of what a
// caller asks for.
type AppSpec struct {
	Ref

	Image    string
	Replicas int32

	// Port the container listens on. Zero means the workload takes no traffic.
	Port int32

	Env map[string]string

	// Resource requests and limits, as Kubernetes quantity strings. Empty
	// fields fall back to the namespace LimitRange.
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string

	// WritableRootFilesystem disables the read-only root filesystem.
	//
	// The default (false) is correct and should stay the default: it is one of
	// the cheapest real defences available. But some images genuinely cannot
	// run without writing outside /tmp, and the honest choice is an explicit,
	// visible escape hatch rather than silently weakening the default for
	// everyone. /tmp is always writable — see the k8s implementation.
	WritableRootFilesystem bool

	// Hosts are the hostnames routed to this workload. Empty means the
	// workload is reachable only inside the cluster.
	//
	// Plain strings rather than a richer type: the orchestrator's job is to
	// route a name, and which names are legitimate — platform-issued or
	// customer-claimed — is a decision that belongs above this seam.
	Hosts []string

	// HealthPath is an HTTP path on the workload's own port that reports
	// whether it is ready to serve. Empty means no probe.
	//
	// One path drives both probes, because an app that answers differently on
	// two paths is answering a question nobody asked it.
	HealthPath string

	// Liveness turns the same path into a restart condition as well.
	//
	// Off by default, and deliberately a separate switch. Readiness only
	// withholds traffic; liveness kills the container. A probe that is a little
	// too impatient turns a slow-starting app into a restart loop, which
	// presents as the app being broken rather than as the probe being wrong.
	Liveness bool

	// Secrets are environment values that must not appear in the pod template.
	//
	// They become a Kubernetes Secret the container reads with envFrom, rather
	// than literals in the Deployment — so they are absent from
	// `kubectl get deploy -o yaml`, which is the copy people read, paste into
	// issues, and check into repositories.
	Secrets map[string]string

	// Volumes are storage the workload keeps across redeploys.
	//
	// Attaching any forces the workload to recreate rather than roll on
	// deploy, and limits it to one replica — both because a ReadWriteOnce
	// claim mounts on one node at a time. See VolumeSpec.
	Volumes []VolumeSpec

	// TLS requests terminated TLS for Hosts.
	//
	// It carries no certificate reference. Platform hostnames are served from
	// the ingress controller's own default certificate, so the workload's
	// routing never names a Secret — an Ingress's TLS Secret must live in the
	// Ingress's own namespace, and every app has its own namespace.
	TLS bool
}

// VolumeSpec is one piece of storage attached to a workload.
//
// Size is bytes rather than a Kubernetes quantity string so that comparing a
// new size against the old is arithmetic. That comparison is the whole of the
// expansion rule: Kubernetes cannot shrink a claim, and neither can anything
// else — the filesystem on it may be full.
type VolumeSpec struct {
	Name      string
	MountPath string
	SizeBytes int64

	// Class is the StorageClass. Empty means the cluster default.
	Class string
}

// Validate checks the spec well enough to avoid sending nonsense to a cluster.
func (s AppSpec) Validate() error {
	if err := s.Ref.Validate(); err != nil {
		return err
	}
	if s.Image == "" {
		return errors.New("app spec: image is required")
	}
	if s.Replicas < 0 {
		return fmt.Errorf("app spec: replicas must be >= 0, got %d", s.Replicas)
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("app spec: port must be within 0-65535, got %d", s.Port)
	}
	if len(s.Hosts) > 0 && s.Port == 0 {
		return errors.New("app spec: hosts require a port to route to")
	}
	for _, h := range s.Hosts {
		if !hostRE.MatchString(h) {
			return fmt.Errorf("app spec: %q is not a valid hostname", h)
		}
	}
	if err := s.validateVolumes(); err != nil {
		return err
	}
	if err := s.validateHealth(); err != nil {
		return err
	}
	return nil
}

// Phase is a coarse lifecycle state, deliberately smaller than the set of
// conditions Kubernetes reports.
type Phase string

const (
	PhasePending  Phase = "pending"
	PhaseRunning  Phase = "running"
	PhaseDegraded Phase = "degraded"
	PhaseStopped  Phase = "stopped"
)

// AppStatus is the observed state of a workload.
type AppStatus struct {
	Phase     Phase
	Desired   int32
	Ready     int32
	Available int32
	Message   string
}

// Orchestrator applies desired state to a runtime and reports back what it
// observes.
//
// Implementations must be safe to call repeatedly with the same arguments:
// every method is expected to converge rather than fail on "already exists".
type Orchestrator interface {
	// EnsureNamespace creates or updates an owner's namespace, including the
	// security posture and default limits that go with it.
	EnsureNamespace(ctx context.Context, spec NamespaceSpec) error

	// DeleteNamespace removes a namespace and everything in it. It returns nil
	// if the namespace is already gone.
	DeleteNamespace(ctx context.Context, name string) error

	// ApplyApp converges a workload to the given spec.
	ApplyApp(ctx context.Context, spec AppSpec) error

	// DeleteApp removes a workload. It returns nil if already gone.
	DeleteApp(ctx context.Context, ref Ref) error

	// AppStatus reports observed state. It returns ErrNotFound if the workload
	// does not exist.
	AppStatus(ctx context.Context, ref Ref) (AppStatus, error)

	// Ping verifies the runtime is reachable. Used by health checks and at
	// startup, so a misconfigured cluster fails loudly instead of at first
	// deploy.
	Ping(ctx context.Context) error

	ClusterInspector
}

// ClusterInspector reports on the runtime itself rather than on workloads.
//
// Read-only by design. Everything that mutates a cluster goes through the
// workload methods above, so an implementation can serve inspection from a
// cache, a read replica, or a restricted credential without that choice
// leaking into the deploy path.
type ClusterInspector interface {
	// ClusterSummary returns headline capacity and utilisation.
	ClusterSummary(ctx context.Context) (ClusterSummary, error)

	// Nodes lists the machines backing the runtime.
	Nodes(ctx context.Context) ([]NodeInfo, error)

	// Pods lists running pods.
	Pods(ctx context.Context, opts PodListOptions) ([]PodInfo, error)

	// Events lists recent cluster events, newest first, capped at limit.
	Events(ctx context.Context, limit int) ([]EventInfo, error)

	// Volumes lists an owner's persistent volume claims.
	//
	// Scoped rather than cluster-wide: a claim names the workload it belongs to,
	// and one team reading another's is a disclosure. A claim the engine did not
	// create carries no owner and belongs to whoever runs the cluster, so it is
	// shown to nobody here.
	Volumes(ctx context.Context, owner OwnerID) ([]VolumeInfo, error)
}

// validateVolumes checks the storage attached to a workload.
func (s AppSpec) validateVolumes() error {
	if len(s.Volumes) == 0 {
		return nil
	}

	// A ReadWriteOnce claim mounts on one node at a time, so a second pod has
	// nowhere to schedule that can also reach the volume. Refused here rather
	// than left to the cluster, where it appears as a pod that stays Pending
	// with the reason somewhere nobody is looking.
	if s.Replicas > 1 {
		return fmt.Errorf(
			"app spec: a workload with storage runs one replica, not %d — "+
				"its volume can only be mounted by one pod at a time", s.Replicas)
	}

	seen := make(map[string]bool, len(s.Volumes))
	for _, v := range s.Volumes {
		switch {
		case !dnsLabel.MatchString(v.Name):
			return fmt.Errorf("app spec: %q is not a valid volume name", v.Name)
		case v.SizeBytes <= 0:
			return fmt.Errorf("app spec: volume %q needs a size", v.Name)
		case !path.IsAbs(v.MountPath):
			return fmt.Errorf("app spec: volume %q mount path must be absolute", v.Name)
		case v.MountPath == "/":
			return fmt.Errorf("app spec: volume %q cannot be mounted at /", v.Name)
		// path.Clean removes a trailing slash and resolves "..", so a path that
		// is not already clean is one where what was asked for and what would
		// be mounted differ.
		case path.Clean(v.MountPath) != v.MountPath:
			return fmt.Errorf("app spec: volume %q mount path %q is not a clean path",
				v.Name, v.MountPath)
		case seen[v.MountPath]:
			return fmt.Errorf("app spec: two volumes are mounted at %q", v.MountPath)
		}
		seen[v.MountPath] = true
	}
	return nil
}

// validateHealth checks the probe settings.
func (s AppSpec) validateHealth() error {
	if s.Liveness && s.HealthPath == "" {
		return errors.New(
			"app spec: liveness needs a health path — restarting a container on a " +
				"condition nobody specified would restart a working one")
	}
	if s.HealthPath == "" {
		return nil
	}
	if s.Port == 0 {
		return errors.New("app spec: a health path needs a port to probe")
	}
	// Kubernetes takes a path, not a URL: anything with a scheme, a query or a
	// space is a caller who has confused the two, and the pod would be rejected
	// with a message about the field rather than about what they typed.
	switch {
	case !path.IsAbs(s.HealthPath):
		return fmt.Errorf("app spec: health path %q must be absolute", s.HealthPath)
	case strings.ContainsAny(s.HealthPath, " ?#"):
		return fmt.Errorf("app spec: health path %q must be a path, not a URL", s.HealthPath)
	case path.Clean(s.HealthPath) != s.HealthPath:
		return fmt.Errorf("app spec: health path %q is not a clean path", s.HealthPath)
	}
	return nil
}
