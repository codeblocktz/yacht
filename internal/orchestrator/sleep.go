package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
)

// Sleeping workloads.
//
// A workload that has had no requests for a while is scaled to zero and its
// hostnames are routed to a waker — an HTTP handler the engine serves — which
// scales it back up when the next request arrives. The orchestrator's part is
// the two halves of that: routing hosts somewhere other than the workload,
// and counting the requests that decide when a workload is idle.

// WakerEndpoint is where a sleeping workload's requests are sent: the
// engine's waker, at an address the ingress controller can reach.
//
// An IP rather than a hostname, because it becomes an endpoint of a Service
// with no selector, and an endpoint is an address. Where the engine runs as a
// process on a cluster node, that is the node's address; where it runs in the
// cluster, the cluster IP of a Service in front of it.
type WakerEndpoint struct {
	IP   string
	Port int32
}

// Validate checks the endpoint is an address and a port.
func (w WakerEndpoint) Validate() error {
	addr, err := netip.ParseAddr(w.IP)
	if err != nil {
		return fmt.Errorf("waker: %q is not an IP address", w.IP)
	}
	if addr.IsUnspecified() || addr.IsLoopback() {
		// Loopback is this machine to the engine and the ingress controller's
		// own pod to the ingress controller, which are not the same place.
		return fmt.Errorf("waker: %s cannot be reached from the cluster", w.IP)
	}
	if w.Port < 1 || w.Port > 65535 {
		return fmt.Errorf("waker: port must be within 1-65535, got %d", w.Port)
	}
	return nil
}

// validateWaker keeps a waker route to workloads it can serve: one with
// hostnames, since a request reaches the waker by name.
func (s AppSpec) validateWaker() error {
	if s.Waker == nil {
		return nil
	}
	if len(s.Hosts) == 0 {
		return errors.New("app spec: a workload routed to the waker needs a hostname")
	}
	return s.Waker.Validate()
}

// ReasonUnschedulable is PodInfo.Reason for a pod no node has room for. A
// wake that meets it has run out of machine, which is a different answer from
// an app that is slow to start.
const ReasonUnschedulable = "Unschedulable"

// RequestCounter reports how many requests the ingress controller has routed
// to each workload.
//
// Optional, like HTTPLogger. The counts are cumulative and only compared with
// each other: a count that changed, in either direction, is a workload
// somebody is using — a controller that restarted has reset its counters, and
// that is not evidence that nobody came.
type RequestCounter interface {
	// RequestCounts returns a count for every ref it can see; a ref with no
	// requests since the controller started is zero. ErrNotSupported means
	// this cluster's controller does not publish them, and nothing about any
	// workload's traffic is known.
	RequestCounts(ctx context.Context, refs []Ref) (map[Ref]int64, error)
}

// ServiceAddresser reports the address a workload's own Service answers on,
// for the waker to hand a held request to once the workload is up.
//
// Reachable only from somewhere inside the cluster's network — a node, or a
// pod. An engine running anywhere else gets an address it cannot dial, and
// the waker answers the held request with a retry instead.
type ServiceAddresser interface {
	ServiceAddress(ctx context.Context, ref Ref) (string, error)
}
