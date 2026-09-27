// Command yacht runs the Yacht engine: a self-hosted PaaS control plane for
// Kubernetes.
//
// The composition lives in package engine so an application wrapping the
// engine runs the same one with overrides. This is the engine with none.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/codeblocktz/yacht/engine"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/yacht
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "yacht: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := engine.LoadConfig()
	if err != nil {
		return err
	}

	// Signals cancel the root context, which unwinds startup and serving
	// alike — so a Ctrl-C during a slow cluster connect exits promptly
	// instead of hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return engine.Run(ctx, cfg, engine.Overrides{Version: version})
}
