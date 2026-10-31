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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"path"
	"regexp"
	"strings"
	"time"
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

	// Project is the project its app is drawn on, which private networking
	// and isolation group namespaces by. Empty leaves the namespace as it
	// is: grouped at the team until a sync of the private network says.
	Project string

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
	if err := ValidateLabelValue("project", s.Project); err != nil {
		return err
	}
	return ValidateDNSLabel("namespace", s.Name)
}

// ResourceLimits expresses CPU and memory bounds using Kubernetes quantity
// strings ("500m", "512Mi").
type ResourceLimits struct {
	DefaultCPU    string // the limit of a container that sets none
	DefaultMemory string
	MaxCPU        string // ceiling any single container may request
	MaxMemory     string

	// DefaultCPURequest and DefaultMemoryRequest are what a container that
	// reserves nothing is given as its request: less than its limit, so an
	// app with no settings is a burstable one rather than one holding a
	// default's worth of the node whether it uses it or not. Empty is the
	// default limit, which is Kubernetes' own answer.
	DefaultCPURequest    string
	DefaultMemoryRequest string
}

// DefaultLimits is the fallback applied when a NamespaceSpec leaves Limits
// empty, and what an app that sets no limit of its own is given.
//
// The limit is half a core and 512 MiB: enough for the apps people actually
// deploy — Phoenix, Rails and Next.js all need more than 256 MiB to boot — so
// leaving the sliders alone gives a working app rather than one killed for
// memory on its first request. It was 0.1 vCPU and 128 MiB, which nothing
// larger than a static file server starts in.
//
// The request stays where the limit used to be. That is what the scheduler
// holds on the node, so raising the limit did not halve how many apps fit
// on a small one.
var DefaultLimits = ResourceLimits{
	DefaultCPU:           "500m",
	DefaultMemory:        "512Mi",
	MaxCPU:               "2",
	MaxMemory:            "4Gi",
	DefaultCPURequest:    "100m",
	DefaultMemoryRequest: "128Mi",
}

// OrDefaults returns l, substituting DefaultLimits for any unset field.
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
	if l.DefaultCPURequest == "" {
		l.DefaultCPURequest = DefaultLimits.DefaultCPURequest
	}
	if l.DefaultMemoryRequest == "" {
		l.DefaultMemoryRequest = DefaultLimits.DefaultMemoryRequest
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

	ReleaseID     string
	ConfigVersion int64
	Image         string
	Replicas      int32

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

	// Internal keeps the workload off the public internet: no hostname, and a
	// Service on the port it actually listens on rather than the fixed one.
	Internal bool

	// RunAsUser is the uid the container runs as. Zero leaves it to the image.
	//
	// Needed because the security posture requires a non-root user and most
	// stock images do not name one — the kubelet then refuses the pod with
	// "image will run as root" rather than picking something. A source that
	// knows its image knows the uid; an arbitrary image does not, and gets
	// nothing here.
	RunAsUser int64

	// FSGroup owns the mounted volumes.
	//
	// A freshly provisioned claim belongs to root, so a container running as
	// anyone else cannot write to it. This is what makes a volume usable by a
	// non-root process, and without it storage and the security posture are
	// mutually exclusive.
	FSGroup int64

	// ScratchPaths are writable empty directories inside the container.
	//
	// The root filesystem is read-only, which most software is fine with until
	// it wants one specific path — a socket directory, a lock file. Naming
	// those is what lets the read-only default stay the default.
	ScratchPaths []string

	// Args replace the image's command arguments. Empty runs the image's own.
	// $(NAME) is expanded from the workload's environment, secrets included,
	// which is how a password reaches a program that only takes it as a flag.
	Args []string

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

	// TLSHosts are the hostnames terminated TLS is requested for.
	//
	// A subset of Hosts, not all of them, and that is the whole point. The
	// certificate this relies on is the ingress controller's own default — a
	// wildcard for the platform domain — so listing a hostname it cannot match
	// does not produce a certificate, it produces a TLS handshake the browser
	// refuses. A custom domain is never under the platform domain, so it never
	// belongs here.
	//
	// It carries no certificate reference. An Ingress's TLS Secret must live in
	// the Ingress's own namespace, and every app has its own namespace, so one
	// pre-provisioned wildcard cannot be referenced from all of them.
	//
	// Empty leaves the TLS block off entirely.
	TLSHosts []string

	// IssuedHosts are hostnames that each get a certificate of their own,
	// issued by CertIssuer — the answer for a name the platform wildcard can
	// never cover, which is every custom domain.
	//
	// A subset of Hosts and disjoint from TLSHosts. One certificate per name
	// rather than one listing them all, because a certificate is issued whole
	// or not at all: a single domain whose DNS has drifted would otherwise hold
	// every other domain on the app hostage.
	//
	// Each lands in the Secret CertSecretName names, in the app's own
	// namespace, which is where an Ingress can reference it.
	IssuedHosts []string

	// CertIssuer is the cluster-wide issuer IssuedHosts are requested from.
	// Empty means the install has none, and IssuedHosts must be empty too.
	CertIssuer string

	// CNAMETarget, when set, becomes the ExternalDNS target annotation, so that
	// controller publishes a CNAME rather than the nodes' own addresses as A
	// records. Empty leaves the annotation off: an install running no
	// ExternalDNS should not carry configuration for one.
	CNAMETarget string

	// HTTPSOnly routes through the ingress controller's secure entrypoint only.
	HTTPSOnly bool

	// TCPProxyPort, when set, is a port every machine in the cluster answers
	// on and hands to the workload's own Port: a TCP proxy, for what does not
	// speak HTTP and so cannot go through the ingress controller. Zero is none,
	// and removes one that was there.
	//
	// Chosen above this seam and handed down rather than left to the cluster
	// to allocate, because it is what somebody's connection string says: it
	// has to survive the Service behind it being deleted and made again.
	TCPProxyPort int32

	// RegistryAuth is a Docker config.json for pulling this app's image, when
	// it comes from a private registry. Empty leaves the namespace without a
	// pull secret, which is right for a public image.
	RegistryAuth []byte

	// Waker, when set, routes Hosts to the engine's waker instead of to the
	// workload. Set while the workload sleeps, with Replicas zero, and while
	// it wakes, with Replicas back — the route returns only once pods are
	// ready to take it, so nobody is sent to a Service with nothing behind it.
	Waker *WakerEndpoint

	// Pool places the workload's pods on the nodes labelled PoolLabel=Pool,
	// and lets them onto those of the nodes that are tainted to keep the pool
	// for its own apps. Empty is anywhere the scheduler likes, which is every
	// install that has never named a pool.
	//
	// The pods only. A build runs wherever there is room: it is the install's
	// work on the team's behalf, and a pool sized for the team's apps is not
	// sized for a compiler.
	Pool string

	// Region keeps the workload's pods on the nodes labelled RegionLabel=
	// Region — a requirement, not a preference, since a region is where an
	// app's data and its customers' traffic are meant to stay. Combined with
	// Pool when both are set: a pod has to land on a node that is both in
	// the region and in the pool. Empty is no requirement, which is every
	// app on an install whose nodes carry no region.
	//
	// The pods only, like Pool. A build may run in any region: it produces an
	// image in a registry, and nothing of the app's stays where it ran.
	Region string

	// StartCommand, when set, is what the container runs instead of the
	// image's own ENTRYPOINT and CMD: a shell command line, run by /bin/sh -c
	// so it reads the way somebody would type it. Empty runs the image as it
	// was built, which is every app that has never set one.
	StartCommand string

	// Schedule, when set, runs the workload on a cron schedule instead of
	// keeping it running: each run starts the container, which exits when it
	// is done. A five-field cron expression, or one of the @hourly family, in
	// UTC — the grammar Kubernetes accepts, checked with the parser it uses.
	//
	// A scheduled workload takes no traffic: it has no Service and no
	// Ingress, since between runs there is nothing to route to. Replicas
	// zero pauses the schedule rather than meaning nothing.
	Schedule string

	// RestartPolicy is what happens when the container exits: one of the
	// Restart constants, empty being RestartOnFailure. MaxRestarts bounds
	// RestartOnFailure; zero is DefaultMaxRestarts. See RestartPolicy for
	// what each can honestly mean for a workload that always runs.
	RestartPolicy RestartPolicy
	MaxRestarts   int32

	// HealthTimeoutSeconds is how long a new version has to become ready
	// before its deploy counts as failed. Zero is the engine's own budget.
	HealthTimeoutSeconds int32
}

// RestartPolicy is what happens to a container that exits.
//
// Railway's three choices, made to mean what they can on Kubernetes, which
// restarts every container a Deployment runs whatever it is asked: a pod
// template there accepts restartPolicy Always and nothing else. So for an app
// that always runs, the policy cannot stop a restart. What it decides is how
// many crashes a new version may have before its deploy fails — and with it,
// the reconciler returns the app to the version before, which is what does
// stop the crashing one — and when a running app counts as failing.
//
// A scheduled run is a Job, which can be told not to restart, and there the
// policy is the Job's own.
type RestartPolicy string

const (
	// RestartOnFailure gives a crashing container MaxRestarts restarts
	// before it is failing: a new version's deploy fails, and a running app
	// is reported failing while Kubernetes goes on retrying with its
	// back-off. A run on a schedule is retried MaxRestarts times.
	RestartOnFailure RestartPolicy = "on_failure"

	// RestartAlways never gives up: a crashing container is never what fails
	// a deploy, only a version that does not become ready in time does. A
	// run on a schedule is retried, as on failure, since a Job cannot be
	// restarted after it succeeds.
	RestartAlways RestartPolicy = "always"

	// RestartNever fails a new version's deploy on its first crash, and
	// reports a running app failing from its first. It does not stop
	// Kubernetes restarting a running app's container — that would mean
	// scaling the app to nothing, which is a decision about the app rather
	// than about one crash — but nothing calls it healthy while it does. A
	// run on a schedule is not retried.
	RestartNever RestartPolicy = "never"
)

// DefaultMaxRestarts is how many times a container may crash before
// RestartOnFailure counts it failing, when an app has not said: enough for one
// that waits on a database still starting, few enough not to wait out a
// deadline for one that never will.
const DefaultMaxRestarts = 3

// CrashLimit is how many restarts a container may have before it is failing.
// Zero is no limit.
func (s AppSpec) CrashLimit() int32 {
	switch s.RestartPolicy {
	case RestartAlways:
		return 0
	case RestartNever:
		return 1
	}
	if s.MaxRestarts > 0 {
		return s.MaxRestarts
	}
	return DefaultMaxRestarts
}

// ValidateRestartPolicy reports whether p is one of the Restart constants, or
// empty for the default.
func ValidateRestartPolicy(p RestartPolicy) error {
	switch p {
	case "", RestartOnFailure, RestartAlways, RestartNever:
		return nil
	}
	return fmt.Errorf("restart policy %q is not on_failure, always or never", p)
}

// MaxHealthTimeoutSeconds is the longest a new version may be given to
// become ready: half an hour, past which a deploy nobody can tell from a hung
// one is worse than one that fails and says why.
const MaxHealthTimeoutSeconds = 1800

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
	if err := ValidatePool(s.Pool); err != nil {
		return fmt.Errorf("app spec: %w", err)
	}
	if err := ValidateRegion(s.Region); err != nil {
		return fmt.Errorf("app spec: %w", err)
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
	if s.TCPProxyPort < 0 || s.TCPProxyPort > 65535 {
		return fmt.Errorf("app spec: tcp proxy port must be within 0-65535, got %d", s.TCPProxyPort)
	}
	if s.TCPProxyPort > 0 && s.Port == 0 {
		return errors.New("app spec: a tcp proxy requires a port to send connections to")
	}
	for _, h := range s.Hosts {
		if !hostRE.MatchString(h) {
			return fmt.Errorf("app spec: %q is not a valid hostname", h)
		}
	}
	if err := s.validateIssued(); err != nil {
		return err
	}
	if err := s.validateVolumes(); err != nil {
		return err
	}
	if err := s.validateHealth(); err != nil {
		return err
	}
	if err := ValidateSchedule(s.Schedule); err != nil {
		return fmt.Errorf("app spec: %w", err)
	}
	if err := ValidateRestartPolicy(s.RestartPolicy); err != nil {
		return fmt.Errorf("app spec: %w", err)
	}
	if s.MaxRestarts < 0 {
		return fmt.Errorf("app spec: max restarts must be >= 0, got %d", s.MaxRestarts)
	}
	if s.HealthTimeoutSeconds < 0 || s.HealthTimeoutSeconds > MaxHealthTimeoutSeconds {
		return fmt.Errorf("app spec: health timeout must be within 0-%d seconds, got %d",
			MaxHealthTimeoutSeconds, s.HealthTimeoutSeconds)
	}
	return s.validateWaker()
}

// validateIssued keeps IssuedHosts to names the Ingress actually routes and the
// wildcard does not already serve. A certificate for a name that is not routed
// is a request the issuer will fail, repeatedly, against a rate limit.
func (s AppSpec) validateIssued() error {
	if len(s.IssuedHosts) == 0 {
		return nil
	}
	if s.CertIssuer == "" {
		return errors.New("app spec: issued hosts need a certificate issuer")
	}
	routed := make(map[string]bool, len(s.Hosts))
	for _, h := range s.Hosts {
		routed[h] = true
	}
	for _, h := range s.TLSHosts {
		routed[h] = false
	}
	for _, h := range s.IssuedHosts {
		if !routed[h] {
			return fmt.Errorf("app spec: %q is issued a certificate but is not a routed host outside the wildcard", h)
		}
	}
	return nil
}

// CertSecretName is the Secret an issued certificate for host is kept in.
//
// The hostname itself where it fits, so `kubectl get certificates` reads as a
// list of domains. A Secret name may be 253 characters but the Certificate
// that shares it ends up in label values, which may be 63; past that the name
// is shortened and a hash of the whole host keeps it unique.
func CertSecretName(host string) string {
	const prefix, max = "tls-", 63
	name := prefix + host
	if len(name) <= max {
		return name
	}
	sum := sha256.Sum256([]byte(host))
	suffix := "-" + hex.EncodeToString(sum[:])[:10]
	head := strings.TrimRight(name[:max-len(suffix)], ".-")
	return head + suffix
}

// Certificate is what is known about the certificate issued for one hostname.
type Certificate struct {
	// Issued is true once a certificate for the host is in place. False
	// covers both "still being issued" and "failing to issue": the Secret is
	// only written on success, and it cannot tell those apart.
	Issued bool

	// NotAfter is when the issued certificate expires. The issuer renews well
	// before then; a date that has passed means renewal has been failing.
	NotAfter time.Time

	// Trusted is true when the certificate chains to an authority browsers
	// trust. Let's Encrypt's staging environment — the installer's default —
	// issues certificates that do not, and a visitor gets the same warning as
	// for no certificate at all.
	Trusted bool
}

// StatusLister reports many workloads' observed state in one pass.
//
// Optional, and asserted for like CertificateReader. A page that lists a
// project's services wants every one's state, and asking for them one at a
// time is a request to the cluster per service: fifteen services was fifteen
// GETs queued behind the client's rate limit, seconds of a page load with
// the cluster idle. An implementation reads each kind once per owner
// instead, as live as AppStatus: nothing is cached between calls.
//
// The result holds a status for every workload that exists; one that does not
// is left out, as AppStatus would have returned ErrNotFound for it.
type StatusLister interface {
	AppStatuses(ctx context.Context, refs []Ref) (map[Ref]AppStatus, error)
}

// CertificateReader reports on certificates issued for IssuedHosts.
//
// Optional, and asserted for like NodeManager. An implementation that cannot
// read them leaves the dashboard saying issuance was requested rather than
// what became of it.
type CertificateReader interface {
	Certificate(ctx context.Context, ref Ref, host string) (Certificate, error)
}

// DefaultPreDeployTimeout is how long a pre-deploy command may run when the
// request names no limit of its own.
//
// Long enough for a migration that rewrites a large table, short enough that
// a command waiting on something that will never answer — a database it
// cannot reach, a prompt nobody will see — fails the deploy in minutes rather
// than holding it for the whole of the deploy's budget.
const DefaultPreDeployTimeout = 10 * time.Minute

// PreDeployRequest is one run of a release's pre-deploy command.
type PreDeployRequest struct {
	// App is the release about to be applied, exactly as it will be applied.
	// The command runs in its image, with its environment, its secrets, its
	// user and its limits, in its namespace and its region — the same place
	// the new version is about to start, so a migration that works here works
	// for the app.
	App AppSpec

	// Command is a shell command line, run by /bin/sh -c.
	Command string

	// Timeout caps the run. Zero is DefaultPreDeployTimeout.
	Timeout time.Duration
}

// PreDeployResult is how a pre-deploy command ended.
type PreDeployResult struct {
	// Succeeded is true when the command exited 0 within its time.
	Succeeded bool

	// ExitCode is what the command exited with, when it exited at all. A
	// command stopped at its deadline, or that never started, has none and
	// says why in Reason.
	ExitCode int32

	// Reason is why a command that did not succeed did not, in a phrase:
	// "timed out after 10m0s", or what the runtime said about a container
	// that could not start. Empty when the exit code says it all.
	Reason string

	// Output is the end of what the command printed, oldest first. The end
	// because that is where a failure says what it was, and a bounded amount
	// because a migration can print a line per row.
	Output []string
}

// PreDeployer runs a release's pre-deploy command before the release is
// applied.
//
// Optional, and asserted for like CertificateReader. Most of what an
// orchestrator does is converge a workload; running a command to completion
// and reporting how it ended is a different capability, and an implementation
// without it is still a working orchestrator. What it is not is one that can
// deploy an app with a pre-deploy command: the engine refuses such a deploy
// rather than skipping the command, because the new version would start
// against a database nobody migrated.
//
// The error is for a run that could not be carried out — the cluster refused
// the Job, or stopped answering. A command that ran and failed is a result,
// not an error, so a caller can tell "the migration failed" from "the
// migration could not be asked for", which is the difference between a
// deploy that has failed and one worth trying again.
type PreDeployer interface {
	PreDeploy(ctx context.Context, req PreDeployRequest) (PreDeployResult, error)
}

// Phase is a coarse lifecycle state, deliberately smaller than the set of
// conditions Kubernetes reports.
type Phase string

const (
	PhasePending  Phase = "pending"
	PhaseRunning  Phase = "running"
	PhaseDegraded Phase = "degraded"
	PhaseStopped  Phase = "stopped"
	// PhaseFailing is an app with nothing ready whose container cannot
	// start, or whose rollout Kubernetes has given up on: not on its way
	// up, the way pending is.
	PhaseFailing Phase = "failing"
	// PhaseScheduled is an app that runs on a schedule rather than all the
	// time, with its schedule in force. Between runs nothing is running, and
	// that is the healthy state for it.
	PhaseScheduled Phase = "scheduled"
)

// ReasonProgressDeadlineExceeded is AppStatus.Reason for a rollout the
// cluster gave up on because its new version never became ready in time:
// a readiness failure, which a deploy reports in its own words (what port
// was knocked on, what the app printed) rather than the cluster's.
const ReasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"

// AppStatus is the observed state of a workload.
type AppStatus struct {
	Phase              Phase
	ReleaseID          string
	ConfigVersion      int64
	Generation         int64
	ObservedGeneration int64
	Desired            int32
	// Pods is every pod of the workload, an old revision's still serving
	// included; Updated is the new revision's. Ready and Available count
	// both, so a rollout is only through once Pods is down to Updated.
	Pods               int32
	Updated            int32
	Ready              int32
	Available          int32
	AvailableCondition bool
	Terminal           bool
	Reason             string
	Message            string

	// Restarts is how many times the containers of the release being rolled
	// out have restarted, across its pods, and LastExit how the most recent
	// of those restarts ended: "code 137 (OOMKilled)". A deploy is not
	// healthy while the count is still climbing, however ready the pods look
	// between crashes; see the rollout's settle window.
	Restarts int32
	LastExit string

	// Schedule is the cron expression a scheduled workload is running on,
	// as the cluster has it; empty for one that always runs. LastRun is when
	// it last started a run, zero for never.
	Schedule string
	LastRun  time.Time
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

// NodeManager takes a machine out of service.
//
// Optional, and asserted for rather than required. Everything else here can be
// served by a restricted credential — inspection from a cache or a read
// replica, workloads through a namespaced role — and evicting pods across every
// namespace and deleting a node object is the one thing that cannot. An
// implementation that cannot do it simply does not satisfy this, and the
// surface offering it stays off rather than failing when somebody presses it.
//
// Deliberately not part of ClusterInspector, which is read-only by design.
type NodeManager interface {
	// Cordon stops or resumes scheduling onto a node. It does not move
	// anything already running there.
	Cordon(ctx context.Context, node string, unschedulable bool) error

	// Drain evicts the pods a node is running and reports how many it asked
	// to leave.
	//
	// Eviction is a request, not a deletion: it respects disruption budgets,
	// so a pod whose budget would be violated stays and is reported rather
	// than being taken down anyway. It returns once the evictions are
	// requested, not once they have finished — the caller watches the node
	// empty rather than holding a request open for however long that takes.
	Drain(ctx context.Context, node string) (requested int, err error)

	// DeleteNode removes the node object. The machine itself is not touched:
	// it stops being part of the cluster, and shutting it down is a separate
	// act somebody performs where the machine is.
	DeleteNode(ctx context.Context, node string) error
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

	// Logs reads a container's output.
	//
	// On the read-only seam because it is a read. The scoping that matters is
	// not here: the namespace and pod arrive already resolved from an app the
	// caller looked up by owner, because a pod name on its own is enough to
	// read another tenant's logs.
	Logs(ctx context.Context, opts LogOptions) ([]LogLine, error)

	// LogStream follows a container's output until ctx is done.
	//
	// Separate from Logs rather than a flag on LogOptions: the batch call
	// returns a complete slice, and a flag that makes it never return would
	// leave that signature honest for one value of one field and false for the
	// other.
	//
	// The iterator holds a connection open for as long as it is read, so a
	// caller must either drain it or cancel ctx. Scoping is the caller's job
	// here exactly as it is for Logs — the namespace and pod arrive already
	// resolved from an app looked up by owner.
	LogStream(ctx context.Context, opts LogOptions) (iter.Seq2[LogLine, error], error)

	// Volumes lists an owner's persistent volume claims.
	//
	// Scoped rather than cluster-wide: a claim names the workload it belongs to,
	// and one team reading another's is a disclosure. A claim the engine did not
	// create carries no owner and belongs to whoever runs the cluster, so it is
	// shown to no team.
	//
	// An empty owner is every claim in the cluster, the engine's or not, as an
	// empty PodListOptions.Owner is every pod: the operator's view, and wrong
	// for anything a member can reach.
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

// ErrNotStarted means a container exists but has not run yet.
//
// Its own error because it is not a failure to read: there is genuinely no log
// , and the reason is on the pod rather than in the log. Reported as a failure
// it reads as "the log is broken" for a container whose problem is that it
// never started — which is the more useful thing to say and is already on
// screen a few lines above.
var ErrNotStarted = errors.New("orchestrator: the container has not started")

// PullSecretName is the Secret an app's namespace holds its pull credential
// in. Named here rather than in the Kubernetes layer because the app service
// decides when to supply one.
const PullSecretName = "yacht-registry"

// BuildRequest is one build of a repository into an image.
type BuildRequest struct {
	// Owner scopes the build's own resources, so a Job can be found and
	// cleaned up by the team it belongs to.
	Owner OwnerID

	// App is the app being built, for naming and labelling only.
	App string

	// Image is where the result must be pushed, fully qualified. Decided by
	// the caller rather than here: the path encodes which team owns the image,
	// and a builder choosing it could put one team's build in another's path.
	Image string

	// RepoURL, Ref and Subdir are what to build.
	RepoURL string
	Ref     string
	Subdir  string

	// Dockerfile is the Dockerfile to build, relative to Subdir. Empty builds
	// the one named Dockerfile there, and falls back to buildpacks when there
	// is none; a path that is set and missing fails the build instead, since
	// somebody named a file and a buildpack guess is not what they asked for.
	Dockerfile string

	// Insecure means the registry speaks plain HTTP, so the push has to be
	// told not to expect TLS. Both build strategies need it and neither
	// discovers it, so it travels with the request.
	Insecure bool

	// RegistryAuth is a Docker config.json authorising the push. Passed per
	// build rather than held by the orchestrator, so the credential lives in
	// the one package allowed to unseal it and crosses this seam only when a
	// build actually needs it.
	RegistryAuth []byte

	// RepoToken is a credential that can read RepoURL, for a private
	// repository: a GitHub installation token scoped to the one repository.
	// Empty clones anonymously. Like RegistryAuth it crosses this seam only
	// for the build that needs it, and it must reach the clone without
	// appearing in the Job's spec, its arguments, the checkout, or the log.
	RepoToken string

	// SourceURL, when set, is fetched instead of RepoURL being cloned: a
	// gzipped tar of somebody's working directory, uploaded by the CLI and
	// served by the engine. SourceToken is the bearer credential the fetch
	// presents, short-lived and good for this one archive; like RepoToken it
	// must reach the fetch without appearing in the Job's spec or the log.
	SourceURL   string
	SourceToken string

	// Variables are the app's variables as its deploy will set them,
	// references resolved and the platform's own included. A Dockerfile gets
	// those it declares with ARG as build arguments, and a buildpacks build
	// gets them all as its platform environment, as Railway does. Like
	// RepoToken they must reach the build without appearing in the Job's
	// spec, its arguments or the log.
	Variables map[string]string

	// Redact are values the build log and its error must not show: the
	// secrets among Variables. A Dockerfile that echoes one would otherwise
	// put it on the build's page.
	Redact []string

	// Log receives output as it arrives. A build that only reported at the end
	// would be silent for the several minutes it is most worth watching.
	Log func(chunk string)
}

// BuildResult is what a build produced.
type BuildResult struct {
	// CommitSHA is what was actually built. A branch moves; this is the answer
	// to "what is running" a week later.
	CommitSHA string

	// RunAsUser is the numeric uid the image runs as, resolved during the
	// build. Zero when it could not be worked out, which leaves the decision
	// where it was before.
	RunAsUser int64

	// BuildArgs are the names the Dockerfile declares with ARG, in any stage,
	// so a deploy can tell whether a changed variable needs a new build.
	// BuildArgsAll means a buildpacks build, which is given every variable.
	// Nil when the build did not say.
	BuildArgs []string
}

// BuildArgsAll is the BuildArgs of a build given every variable.
const BuildArgsAll = "*"

// Builder turns a repository into an image.
//
// Optional, like NodeManager, and for the same reason: an orchestrator that
// cannot run a build is a working orchestrator, and folding this into the
// required interface would make every implementation carry it.
type Builder interface {
	Build(ctx context.Context, req BuildRequest) (BuildResult, error)

	// BuildJobName is the name the Job for a request will have, known before
	// the build starts so it can be recorded. Deriving it here rather than
	// passing one in keeps the naming in the layer that has to make it a legal
	// Kubernetes object name.
	BuildJobName(req BuildRequest) string

	// BuildState reports what the cluster says about a build that was started
	// earlier, by name.
	//
	// This is what makes an interrupted build recoverable: the goroutine that
	// started it does not survive a restart and never existed on the other
	// replicas, but the Job is there for all of them to read.
	BuildState(ctx context.Context, jobName string) (BuildState, error)

	// CancelBuild removes a Job by the stable name BuildJobName returned. The
	// operation is idempotent: a Job already gone is a cancelled build too.
	CancelBuild(ctx context.Context, jobName string) error
}

// BuildState is what the cluster knows about a build.
type BuildState struct {
	// Found is false when there is no such Job. That means it finished and was
	// cleaned up, or it never started — either way nothing is running, which
	// is the answer the reconciler needs.
	Found bool

	// Done and Failed describe a Job that is still there.
	Done   bool
	Failed bool
	Reason string
}

// Why an app's HTTP log is empty.
const (
	// HTTPLogNotEnabled means the ingress controller is running and is not
	// writing an access log. A setting, not a fault, and a different thing to
	// tell somebody than "no requests yet".
	HTTPLogNotEnabled = "not-enabled"

	// HTTPLogNoController means no ingress controller was found at all.
	HTTPLogNoController = "no-controller"
)

// HTTPLogOptions asks for the requests made to some hostnames.
type HTTPLogOptions struct {
	// Hosts is the tenancy boundary. One controller logs every tenant's
	// traffic, so the caller resolves which hosts belong to the app from an
	// owner-scoped query and this layer never decides it.
	Hosts []string
}

// HTTPLogs is what the ingress controller recorded.
type HTTPLogs struct {
	Lines []HTTPLogLine

	// Reason explains an empty result that is not simply "no traffic".
	Reason string

	// From is when the log read begins: every request since is in it, and
	// none from before. The controller's log is rotated away from under the
	// reader, so this is the only honest account of how far back Lines go.
	// Zero when nothing was read.
	From time.Time
}

// HTTPLogLine is one request as the ingress controller saw it.
type HTTPLogLine struct {
	At       time.Time
	Host     string
	Method   string
	Path     string
	Protocol string
	Status   int
	Bytes    int64
	Client   string
	Duration time.Duration
}

// HTTPLogger is an orchestrator that can report requests.
//
// Optional, like Builder and NodeManager: an orchestrator with no ingress
// controller in front of it is still a working orchestrator.
// ErrNotSupported means this cluster cannot be asked to do something.
//
// A configuration answer rather than a failure: an ingress controller somebody
// else installed is not broken, it is simply not Yacht's to reconfigure.
var ErrNotSupported = errors.New("orchestrator: not supported on this cluster")

type HTTPLogger interface {
	HTTPLogs(ctx context.Context, opts HTTPLogOptions) (HTTPLogs, error)

	// HTTPLogHint is the configuration that switches the access log on, for
	// the page to show when Yacht cannot apply it.
	HTTPLogHint() string

	// EnableHTTPLogs applies it, where this cluster's controller is one Yacht
	// can reconfigure. Called once at startup rather than from a page: it
	// restarts a controller every workload routes through, which is a
	// cluster-wide cost and so never one app's decision to make.
	EnableHTTPLogs(ctx context.Context) error

	// HTTPLogsEnabled reports whether the running controller is writing one.
	HTTPLogsEnabled(ctx context.Context) bool
}

// PublicServicePort is where a public app's Service listens, whatever port
// the app itself does.
const PublicServicePort int32 = 80

// ServicePort is the port an app's Service listens on inside the cluster:
// its own for an internal app, and a fixed one for a public app, so the
// ingress in front of it does not change when the app's port does.
func ServicePort(internal bool, port int32) int32 {
	if internal {
		return port
	}
	return PublicServicePort
}
