package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// An install whose cluster was unreachable runs the in-memory orchestrator,
// which enforces no isolation: setting one says so rather than pretending.
func TestNetworkIsolationNeedsAnOrchestratorThatEnforcesIt(t *testing.T) {
	e := &Engine{Orchestrator: orchestrator.NewNoop()}
	if _, ok := e.NetworkIsolation(); ok {
		t.Error("the in-memory orchestrator reports an isolation")
	}
	if err := e.SetNetworkIsolation(context.Background(), Isolation{Mode: IsolationStrict}); !errors.Is(err, ErrNoNetworkIsolation) {
		t.Errorf("SetNetworkIsolation = %v, want ErrNoNetworkIsolation", err)
	}
}
