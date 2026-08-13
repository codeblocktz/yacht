// Command yacht runs the Yacht engine: a self-hosted PaaS control plane for
// Kubernetes.
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

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/cluster"
	"github.com/codeblocktz/yacht/internal/config"
	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/notify"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/orchestrator/k8s"
	"github.com/codeblocktz/yacht/internal/registry"
	"github.com/codeblocktz/yacht/internal/secret"
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

	// Non-nil only when sign-in is switched on. Held here rather than inside
	// newIdentity because the dashboard needs the same service the session
	// provider resolves against — one that signed a cookie the other did not
	// know about would be a sign-in that never takes.
	var accounts *account.Service
	var mailer notify.Mailer
	if cfg.AccountsEnabled() {
		accounts = account.NewService(pool, log)

		// Built before serving starts so that a relay which could never work
		// stops startup, rather than surfacing at the first sign-in attempt
		// where it reads to an operator as sign-in being broken.
		if mailer, err = newMailer(cfg, log); err != nil {
			return err
		}
	}

	ident, err := newIdentity(cfg, accounts, log)
	if err != nil {
		return err
	}

	// Built before serving so a malformed key stops startup rather than
	// surfacing the first time somebody saves a secret — by which point they
	// read it as the feature being broken.
	var keeper *secret.Keeper
	if cfg.SecretKey != "" {
		if keeper, err = secret.NewKeeper(cfg.SecretKey, cfg.SecretKeyPrevious...); err != nil {
			return err
		}
		// The ID is a fingerprint of the key, not the key: it is already stored
		// beside every value sealed with it. Logging it is what lets an operator
		// mid-rotation see which key this process will seal with, rather than
		// inferring it from which values still fail to open.
		//
		// The count comes from the keeper rather than from the configuration,
		// because the keeper is what actually holds them: a key listed twice, or
		// listed as retired while it is also the active one, is held once. An
		// operator reading this line is deciding whether the old key is still
		// loaded, and the configured length would answer yes when it is not.
		log.Info("secret key loaded",
			slog.String("key_id", keeper.ActiveKeyID()),
			slog.Int("retired_keys_held", len(keeper.KeyIDs())-1))
	} else {
		log.Warn("no YACHT_SECRET_KEY set — environment variables can be stored, " +
			"but marking one secret will be refused rather than stored readable; " +
			"generate a key with `openssl rand -base64 32`")
	}

	// Where built images go, and what runs the build. Both optional: without a
	// key there is no registry, and an orchestrator that cannot run a Job is a
	// working orchestrator — the source is listed as unavailable rather than
	// offered and failed.
	var images app.Images
	var manifests app.Manifests
	var builder app.Builder
	registryStore := registry.New(pool, keeper, log)
	manifests = registryStore
	if keeper.Configured() {
		images = registryStore
		if b, ok := orch.(orchestrator.Builder); ok {
			builder = b
		}
	}

	// Which resolver answers questions about custom domains.
	//
	// The domain's own nameservers, by default. Every recursive resolver caches
	// negative answers, and the check made moments before somebody creates a
	// record is exactly what puts one there — so a correct record reads as
	// missing for the zone's negative TTL. Naming a public resolver instead
	// would not remove a cache from the path, only swap one for another.
	//
	// The system resolver stays as the fallback, for an install whose egress
	// does not allow arbitrary port 53.
	var resolver domain.Resolver = domain.AuthoritativeResolver{Fallback: domain.NetResolver{}}
	if cfg.DNSResolver != "" {
		resolver = domain.NewDirectResolver(cfg.DNSResolver)
		log.Info("custom domains are resolved by one configured server",
			slog.String("resolver", domain.ResolverName(resolver)))
	}

	apps := app.NewService(pool, orch, log, app.Options{
		Builder:             builder,
		Images:              images,
		Manifests:           manifests,
		MaxConcurrentBuilds: cfg.MaxConcurrentBuilds,
		AppDomain:           cfg.AppDomain,
		WildcardTLS:         cfg.WildcardTLS,
		Keeper:              keeper,
		ReservedDomains:     cfg.ReservedDomains,
		// Verifying a custom domain is a DNS lookup, and an install that cannot
		// make one says so rather than failing in a way that looks like the
		// domain being wrong.
		Resolver: resolver,
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

	opts := web.Options{
		Orchestrator: orch,
		Identity:     ident,
		Apps:         apps,
		// Slots is left nil: the engine uses its own single-owner chrome.
		// An application wrapping the engine passes its own SlotProvider
		// here, which is the whole of what it takes to add tenant chrome.
		// Accounts are a credential of their own, so the settings page must not
		// report the install as open to anyone merely because no shared token
		// is set.
		Authenticated: cfg.AccountsEnabled() || !cfg.Unauthenticated(),
		Version:       version,
		AppDomain:     cfg.AppDomain,
		WildcardTLS:   cfg.WildcardTLS,
		Logger:        log,
		Nets:          apps,
		Logs:          apps,
	}

	// The add-node surface only exists where a token could actually be sealed.
	// Without a key the page could store nothing and hand out nothing, so it is
	// left off the router entirely rather than shown and refused.
	if keeper.Configured() {
		opts.Joiner = cluster.New(pool, keeper, log)
		// Every stack mints credentials, so the same key that gates joining
		// gates this. Offered without one it would fail after creating the
		// first app.
		opts.Stacks = apps
		// And the registry, for the third time the same reason: it holds a
		// push credential, and a credential this install cannot seal is one it
		// declines to hold.
		opts.Registries = registry.New(pool, keeper, log)
	} else {
		log.Info("add-node and the image registry are off — " +
			"set YACHT_SECRET_KEY to store a cluster join token or registry password")
	}

	if cfg.AccountsEnabled() {
		opts.Accounts = accounts
		opts.Mailer = mailer
		opts.BaseURL = cfg.BaseURL
		opts.MagicLinkTTL = cfg.MagicLinkTTL
		opts.SessionTTL = cfg.SessionTTL
		// The team the install has been running as. The first person to sign in
		// inherits it, so the apps already deployed under YACHT_OWNER_ID stay
		// reachable instead of belonging to an owner nobody can authenticate as.
		opts.BootstrapTeamID = cfg.OwnerID
		opts.BootstrapTeamName = cfg.OwnerName
		opts.MailTransport = cfg.MailTransport()
		opts.BootstrapEmail = cfg.OwnerEmail
	}

	srv, err := web.New(opts)
	if err != nil {
		return err
	}

	// Settles builds whose process went away — a restart mid-build, or a
	// replica that stopped. Level-triggered against the Job rather than driven
	// by anything this process remembers, so it is correct after a restart and
	// correct when several replicas run it at once.
	go apps.RunReleaseBackfill(ctx)
	go apps.RunOperationAdmission(ctx)
	go apps.RunReconciler(ctx)
	go apps.RunAppReconciler(ctx)

	// Proves claimed custom domains without anybody pressing anything. Same
	// shape as the reconciler above and safe for the same reasons: what a name
	// resolves to is a fact any replica can look up, so nothing here depends on
	// this process having been the one that took the claim.
	go domain.NewChecker(pool, resolver, apps, log).Run(ctx)

	// Request logging on by default. An app whose traffic is not being recorded
	// is an app nobody can debug, so this is not left as something to find and
	// switch on — and because there is one ingress controller and one access
	// log, it was never a per-app decision to present in the first place.
	//
	// In a goroutine like the two above because it talks to the cluster: an
	// unreachable API server must delay the dashboard coming up, not stop it.
	go apps.EnsureHTTPLogs(ctx)

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

// newIdentity picks which of the three providers resolves an owner.
//
// Which one is active decides who can reach the dashboard and what they see, so
// each branch says so in the log: an operator must be able to tell from a
// startup line whether the install signs people in, guards a shared token, or
// trusts everyone who can reach the port.
func newIdentity(
	cfg config.Config, accounts *account.Service, log *slog.Logger,
) (identity.Provider, error) {
	if cfg.AccountsEnabled() {
		log.Info("accounts enabled — requests are resolved from a session cookie "+
			"to the team it is acting as",
			slog.String("base_url", cfg.BaseURL),
			slog.String("mail_transport", cfg.MailTransport()),
			slog.Duration("session_ttl", cfg.SessionTTL),
		)
		// With no mail transport configured, sign-in links go to the log. That
		// is the documented break-glass path rather than an accident, but an
		// operator who does not know it will wait for mail that is never sent.
		if cfg.MailTransport() == "log" {
			log.Warn("no mail transport configured — sign-in links will be written " +
				"to this log instead of being sent; set YACHT_SMTP_ADDR or " +
				"YACHT_RESEND_API_KEY to deliver them")
		}
		return accounts.Provider(web.SessionCookie), nil
	}

	owner := identity.Owner{ID: cfg.OwnerID, DisplayName: cfg.OwnerName}

	if cfg.Unauthenticated() {
		log.Warn("no YACHT_AUTH_TOKEN set — the dashboard is unauthenticated. " +
			"Only run this way on a trusted network.")
		return identity.NewSingleOwner(owner), nil
	}
	log.Info("shared-token authentication — every caller acts as the single owner",
		slog.String("owner", cfg.OwnerID))
	return identity.NewStaticToken(owner, cfg.AuthToken)
}

// newMailer builds the transport sign-in links are delivered through.
//
// The logging transport is the fallback rather than an error: with no relay
// configured the link goes to the log, which is the only way back in when mail
// breaks after accounts are switched on.
func newMailer(cfg config.Config, log *slog.Logger) (notify.Mailer, error) {
	switch {
	case cfg.SMTPAddr != "":
		return notify.NewSMTP(notify.SMTPConfig{
			Addr:     cfg.SMTPAddr,
			Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword,
			From:     cfg.SMTPFrom,
		})
	case cfg.ResendAPIKey != "":
		return notify.NewResend(cfg.ResendAPIKey, cfg.ResendFrom)
	default:
		log.Warn("no mail transport configured — sign-in links will be written to " +
			"this log, where anyone who can read it can use them")
		return notify.NewLog(log), nil
	}
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
