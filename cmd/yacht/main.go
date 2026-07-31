// Command yacht runs the Yacht engine: a single-owner, self-hosted PaaS
// control plane for Kubernetes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/config"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/orchestrator/k8s"
	"github.com/codeblocktz/yacht/internal/store"
	"github.com/codeblocktz/yacht/internal/web"
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
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.Debug)
	log.Info("starting yacht",
		slog.String("version", version),
		slog.String("config", cfg.String()),
	)

	// Signals cancel the root context, which unwinds startup and serving
	// alike — so a Ctrl-C during a slow cluster connect exits promptly
	// instead of hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx, cfg.DatabaseURL, log); err != nil {
		return err
	}

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	orch, err := newOrchestrator(ctx, cfg, log)
	if err != nil {
		return err
	}

	ident, err := newIdentity(cfg, log)
	if err != nil {
		return err
	}

	apps := app.NewService(pool, orch, log, app.Options{
		AppDomain:   cfg.AppDomain,
		WildcardTLS: cfg.WildcardTLS,
	})

	// Yacht cannot check that the ingress controller actually has a default
	// certificate: there is no API for "what will you serve for an unknown
	// host". Without one, apps are served the wrong certificate rather than
	// failing, so the one thing available is to say so plainly at startup.
	if cfg.WildcardTLS {
		log.Info("wildcard TLS enabled — platform hostnames are served from the "+
			"ingress controller's default certificate; Yacht cannot verify one is "+
			"configured",
			slog.String("app_domain", cfg.AppDomain))
	}
	if cfg.AppDomain != "" {
		log.Info("per-app hostnames enabled",
			slog.String("app_domain", cfg.AppDomain),
			slog.String("dns", "point *."+cfg.AppDomain+" at this cluster"))
	}

	if err := apps.EnsureOwner(ctx, cfg.OwnerID, cfg.OwnerName, ""); err != nil {
		return err
	}

	srv, err := web.New(web.Options{
		Orchestrator: orch,
		Identity:     ident,
		Apps:         apps,
		// Slots is left nil: the engine uses its own single-owner chrome.
		// An application wrapping the engine passes its own SlotProvider
		// here, which is the whole of what it takes to add tenant chrome.
		Authenticated: !cfg.Unauthenticated(),
		Version:       version,
		AppDomain:     cfg.AppDomain,
		WildcardTLS:   cfg.WildcardTLS,
		Logger:        log,
	})
	if err != nil {
		return err
	}

	return serve(ctx, cfg, srv.Handler(), log)
}

// newOrchestrator connects to a cluster, or falls back to an in-memory stub.
//
// Falling back rather than exiting is deliberate: a self-hoster should be able
// to start the dashboard, see a clear "cluster unreachable" state, and fix
// their kubeconfig from there — rather than face a process that refuses to
// boot and a log line they have to find.
func newOrchestrator(
	ctx context.Context, cfg config.Config, log *slog.Logger,
) (orchestrator.Orchestrator, error) {
	orch, err := k8s.New(ctx, k8s.Config{
		InCluster:  cfg.KubeInCluster,
		Kubeconfig: cfg.Kubeconfig,
	}, log)
	if err == nil {
		log.Info("connected to cluster")
		return orch, nil
	}

	log.Warn("cluster unreachable — starting with an in-memory orchestrator; "+
		"deploys will not reach a cluster until this is fixed",
		slog.String("error", err.Error()),
	)
	return orchestrator.NewNoop(), nil
}

func newIdentity(cfg config.Config, log *slog.Logger) (identity.Provider, error) {
	owner := identity.Owner{ID: cfg.OwnerID, DisplayName: cfg.OwnerName}

	if cfg.Unauthenticated() {
		log.Warn("no YACHT_AUTH_TOKEN set — the dashboard is unauthenticated. " +
			"Only run this way on a trusted network.")
		return identity.NewSingleOwner(owner), nil
	}
	return identity.NewStaticToken(owner, cfg.AuthToken)
}

func serve(
	ctx context.Context, cfg config.Config, handler http.Handler, log *slog.Logger,
) error {
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
