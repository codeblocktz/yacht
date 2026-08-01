package app

import (
	"context"
	"fmt"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// LogRequest asks for one app's container output.
type LogRequest struct {
	// Pod is optional. Empty reads the app's first pod, which is what somebody
	// looking at a single-replica service wants and is the common case.
	Pod string

	// Previous reads the container that died rather than the one running.
	Previous bool

	Tail int64
}

// Logs is an app's container output, and which pods it could have come from.
type Logs struct {
	Pod      string
	Pods     []string
	Lines    []orchestrator.LogLine
	Previous bool

	// Note explains an empty result that is not a failure — a pod that has
	// never restarted has no previous container, and saying so beats an empty
	// pane that looks like a bug.
	Note string
}

// Logs reads an app's container output.
//
// The app is resolved by owner first and the namespace comes from that row, so
// the only pods reachable are the ones belonging to an app the caller owns. A
// pod name is enough to read any tenant's output, so it is never trusted from
// the request: the name is checked against the pods this app actually has.
func (s *Service) Logs(ctx context.Context, ownerID, appName string, req LogRequest) (Logs, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return Logs{}, err
	}

	pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{
		Namespace: a.Namespace, ManagedOnly: true, Owner: orchestrator.OwnerID(ownerID),
	})
	if err != nil {
		return Logs{}, fmt.Errorf("app: list pods for logs: %w", err)
	}

	out := Logs{Previous: req.Previous}
	for _, p := range pods {
		out.Pods = append(out.Pods, p.Name)
	}
	if len(out.Pods) == 0 {
		out.Note = "This app has no running pods, so there is no output to read."
		return out, nil
	}

	// A pod named by the request is honoured only if it is one of this app's.
	// Otherwise the name is a way to read somebody else's container.
	out.Pod = out.Pods[0]
	if req.Pod != "" {
		var ok bool
		for _, name := range out.Pods {
			if name == req.Pod {
				out.Pod, ok = name, true
				break
			}
		}
		if !ok {
			return Logs{}, ErrNotFound
		}
	}

	lines, err := s.orch.Logs(ctx, orchestrator.LogOptions{
		Namespace: a.Namespace, Pod: out.Pod, Tail: req.Tail, Previous: req.Previous,
	})
	if err != nil {
		return Logs{}, fmt.Errorf("app: read logs: %w", err)
	}
	out.Lines = lines

	if len(lines) == 0 {
		if req.Previous {
			out.Note = "This container has not restarted, so there is no earlier run to show."
		} else {
			out.Note = "The container has not written anything yet."
		}
	}
	return out, nil
}
