// Package engine composes Yacht for a program to run — this repository's own
// cmd/yacht, or an application wrapping the engine with its own identity,
// chrome, notifications and orchestrator.
//
// The engine's packages are internal, which is what keeps them free to change.
// This package is the one public surface: the four seams, as aliases of the
// internal types so a wrapping application can name and implement them, and
// the composition that cmd/yacht used to hold, so a wrapper runs the same
// engine the same way with overrides rather than a copy of main.
//
// A wrapping application does:
//
//	cfg, _ := engine.LoadConfig()
//	engine.Run(ctx, cfg, engine.Overrides{
//		Slots:    myChrome{},          // brand, header tools, banner, extra nav
//		Identity: myOrganisations,     // who a request acts as
//		Extra:    engine.ExtraRoutes{Owner: mountBilling},
//	})
//
// Everything the engine does — apps, deploys, builds, domains, certificates,
// storage, teams — is unchanged underneath.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	"github.com/codeblocktz/yacht/internal/retire"
	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store"
	"github.com/codeblocktz/yacht/internal/web"
)

// The seams, by their engine names. Aliases rather than copies: a value a
// wrapper builds against these is the same type the engine consumes.
type (
	// Config is the engine's configuration, read from YACHT_* variables.
	Config = config.Config

	// Owner is the principal a request acts as. The engine treats ID as
	// opaque; a wrapper may make it an organisation's.
	Owner = identity.Owner
	// IdentityProvider resolves a request to an Owner — seam 2.
	IdentityProvider = identity.Provider

	// Slots is the chrome around every page, as data — seam 3.
	Slots = web.Slots
	// SlotProvider fills the chrome for one request.
	SlotProvider = web.SlotProvider
	// SlotProviderFunc is a SlotProvider from a function.
	SlotProviderFunc = web.SlotProviderFunc
	// DefaultSlots is the engine's own chrome, to start from and adjust.
	DefaultSlots = web.DefaultSlots
	NavGroup     = web.NavGroup
	NavItem      = web.NavItem
	Crumb        = web.Crumb
	// SidebarProjects is the sidebar's project tree, which DefaultSlots fills
	// from the team's projects; SidebarProject and SidebarApp are its rows.
	// A wrapper that sets Slots.SidebarTop replaces the engine's team
	// switcher with its own; every NavItem it adds is also a page in the
	// command palette, with nothing to register.
	SidebarProjects = web.SidebarProjects
	SidebarProject  = web.SidebarProject
	SidebarApp      = web.SidebarApp
	// ExtraRoutes are routes a wrapper mounts inside the engine's role gates.
	ExtraRoutes = web.ExtraRoutes
	// Surfaces is which optional pages a request is offered, including
	// whether its person is an operator — what a wrapper's chrome reads to
	// decide whether to offer its own install-wide pages — and ActingAs, the
	// team an operator is acting as for support. The engine's layout draws
	// the acting notice above any Banner a wrapper sets, so it cannot be
	// hidden; a wrapper reads ActingAs to decide what else to show beside it.
	Surfaces = web.Surfaces

	// ActingCheck decides, on every request, whether a person may act as a
	// team other than their own. See OperatorCheck.
	ActingCheck = account.ActingCheck

	// Mailer delivers the engine's messages — seam 4.
	Mailer  = notify.Mailer
	Message = notify.Message

	// Orchestrator runs workloads — seam 1.
	Orchestrator = orchestrator.Orchestrator
	// NodeInfo is one machine, from Orchestrator.Nodes: its pool, its site —
	// where it is, empty on an install in one place — and its room.
	NodeInfo = orchestrator.NodeInfo

	// Role is a team role, for a wrapper gating pages of its own.
	Role = account.Role

	// Accounts is the engine's teams, people, invitations and sessions. A
	// wrapper's organisations build on it rather than beside it.
	Accounts = account.Service
	// Apps is the engine's app service: everything about workloads.
	Apps = app.Service

	// Quota is how much a team may commit — apps, CPU, memory, storage —
	// which Apps.SetQuota replaces. Named here so a wrapper can set one from
	// something of its own, a plan say, without the engine knowing what that
	// is. Zero in a field is unlimited.
	Quota = app.Quota
	// TeamUsage is a team's committed use beside its quota, from
	// Apps.TeamUsage and Apps.TeamUsages.
	TeamUsage = app.TeamUsage

	// CapacitySnapshot is the whole install's room at one moment, from
	// Engine.Capacity: what the capacity policy sells of the machines, what
	// every team has committed, and what is still free to sell. CPU is in
	// millicores, memory and storage in bytes. It totals every team, so it is
	// for the install's operator and the wrapper's own arithmetic — a plan
	// catalogue saying how many more of each plan fit, say — and not for
	// showing to a customer beyond RoomShort.
	CapacitySnapshot = app.CapacitySnapshot
	// CapacityResource is one resource in a CapacitySnapshot.
	CapacityResource = app.CapacityResource
	// CapacityLevel is a snapshot's fullness in a word: ok, warn, full, or
	// unknown when the cluster could not be read.
	CapacityLevel = app.CapacityLevel
	// CapacityPolicy is how much of the install is sold, which the operator
	// edits on Admin → Capacity and Apps.SetCapacityPolicy replaces. Its
	// WakeReservePercent is the share of each sleeping app still counted as
	// committed, so that many of the sleepers can wake at once.
	CapacityPolicy = app.CapacityPolicy
	// SleepingCapacity is CapacitySnapshot.Sleeping: the apps asleep across
	// the install, their size awake, and the wake reserve held for them.
	SleepingCapacity = app.SleepingCapacity

	// SleepPolicy is whether apps sleep when idle and after how long: a
	// team's default, from Apps.TeamSleepDefault and Apps.SetTeamSleepDefault,
	// and what an app's own setting comes to beside it.
	SleepPolicy = app.SleepPolicy
	// SleepSetting is one app's own choice — its team's default, on, or off,
	// and an idle time of its own — which Apps.SetSleepSetting replaces.
	SleepSetting = app.SleepSetting
	SleepMode    = app.SleepMode
	// SleepStatus is everything about one app's sleeping, from
	// Apps.SleepStatus: its setting, its team's, what they come to, where it
	// stands, and its recent sleeps.
	SleepStatus = app.SleepStatus
	// Sleep is App.Sleep: an app's setting and state, read with the app.
	Sleep      = app.Sleep
	SleepState = app.SleepState
	// SleepInterval is one stretch an app spent asleep, from
	// Apps.SleepIntervals — what a meter subtracts; see AwakeWithin.
	SleepInterval = app.SleepInterval

	// Server is the engine's dashboard.
	Server = web.Server
	// Keeper seals secrets at rest.
	Keeper = secret.Keeper
)

const (
	RoleOwner  = account.RoleOwner
	RoleAdmin  = account.RoleAdmin
	RoleMember = account.RoleMember

	// SessionCookie is the cookie the engine's sessions live in.
	SessionCookie = web.SessionCookie

	// DefaultBrandName is the engine's own name, where its chrome puts it —
	// what a wrapper replaces.
	DefaultBrandName = web.DefaultBrandName

	// AdminNavHeading is the sidebar group of the operator's install-wide
	// pages. A wrapper mounting its own with ExtraRoutes.Operator appends
	// their entries to this group.
	AdminNavHeading = web.AdminNavHeading

	// The levels a CapacitySnapshot can be at.
	CapacityOK      = app.CapacityOK
	CapacityWarn    = app.CapacityWarn
	CapacityFull    = app.CapacityFull
	CapacityUnknown = app.CapacityUnknown

	// An app's sleep setting: its team's default, or on or off whatever that is.
	SleepInherit = app.SleepInherit
	SleepOn      = app.SleepOn
	SleepOff     = app.SleepOff

	// Where an app stands. Waking is its pods starting, with requests held.
	SleepAwake  = app.SleepAwake
	SleepAsleep = app.SleepAsleep
	SleepWaking = app.SleepWaking

	// DefaultSleepAfter and MinSleepAfter bound the idle time: 30 minutes
	// where nobody says otherwise, and never under 5.
	DefaultSleepAfter = app.DefaultSleepAfter
	MinSleepAfter     = app.MinSleepAfter
)

var (
	// ErrQuotaExceeded is what Apps refuses a change with when it would take
	// a team past its quota, for a wrapper that answers it with something of
	// its own rather than the engine's sentence.
	ErrQuotaExceeded = app.ErrQuotaExceeded

	// ErrCapacityFull is what Apps refuses a change with when the install
	// enforces its capacity policy and has no room left to sell for it. The
	// engine's sentence says how much more was needed and nothing about any
	// other team; a wrapper may answer it with its own, an upgrade prompt or
	// a waiting list, say.
	ErrCapacityFull = app.ErrCapacityFull

	// ErrSleepUnavailable is what putting an app to sleep is refused with on
	// an install with no waker (YACHT_WAKER_ADDR unset); ErrCannotSleep on an
	// app with no public hostname for a request to wake it through.
	ErrSleepUnavailable = app.ErrSleepUnavailable
	ErrCannotSleep      = app.ErrCannotSleep
	// ErrWakeNoRoom is a wake the install had no room for. The app stays
	// asleep, the refusal is recorded like any other capacity refusal, and a
	// request a minute later tries again.
	ErrWakeNoRoom = app.ErrWakeNoRoom

	// AwakeWithin is how long an app was awake in [from, to), given the
	// intervals Apps.SleepIntervals returned for that window.
	AwakeWithin = app.AwakeWithin

	// LoadConfig reads the engine's configuration from the environment.
	LoadConfig = config.Load
	// Layout draws a page inside the engine's chrome.
	Layout = web.Layout
	// SurfacesFromContext reads them, in a SlotProvider or a handler behind
	// the engine's identity middleware.
	SurfacesFromContext = web.SurfacesFromContext
	// OwnerFromContext reads the Owner identity middleware resolved.
	OwnerFromContext = identity.FromContext
	// MustOwnerFromContext is OwnerFromContext for a handler that is certainly
	// behind the middleware.
	MustOwnerFromContext = identity.MustFromContext
	// OperatorCheck is the ActingCheck the engine wires its own sessions
	// with: the person's address must be in YACHT_OPERATORS, and with none
	// named nobody may act. A wrapper building its identity on
	// Accounts.Provider passes it to WithActingCheck to keep impersonation;
	// without it, no session acts as anything.
	OperatorCheck = web.OperatorCheck
	// ActingBanner is the engine's acting notice, for a wrapper drawing its
	// own chrome that wants it somewhere of its choosing.
	ActingBanner = web.ActingBanner
	// NewSingleOwner and NewStaticToken are the engine's own providers, for a
	// wrapper that wants one of them in some deployments.
	NewSingleOwner = identity.NewSingleOwner
	NewStaticToken = identity.NewStaticToken
)

// Overrides are what a wrapping application supplies. Every field is optional;
// nil means the engine's own.
type Overrides struct {
	// Version is what the dashboard reports. cmd/yacht sets it at build time.
	Version string

	// Logger, or the engine's own text logger at the configured level.
	Logger *slog.Logger

	// Slots is the chrome: brand, navigation, header tools, banner.
	Slots SlotProvider

	// Identity resolves who a request acts as. Given the engine so it can
	// build on Accounts, which exist by the time it is called.
	Identity func(e *Engine) (IdentityProvider, error)

	// Mailer delivers sign-in links and invitations.
	Mailer Mailer

	// Orchestrator runs workloads; nil connects to the configured cluster.
	Orchestrator Orchestrator

	// Extra is routes mounted inside the engine's role gates.
	Extra ExtraRoutes

	// WakerBrand is the name the waking-up page says a sleeping app is
	// hosted on. The page is served on the app's own hostname to its
	// visitors, outside any chrome, so it cannot ask Slots. Empty is the
	// engine's own name.
	WakerBrand string

	// AfterMigrate runs once the engine's schema is current, for a wrapper's
	// own migrations. They share the database; a wrapper keeps its own goose
	// version table so the two histories never collide.
	AfterMigrate func(ctx context.Context, pool *pgxpool.Pool) error
}

// Engine is a composed, not yet running, Yacht.
type Engine struct {
	Config Config
	Log    *slog.Logger
	Pool   *pgxpool.Pool

	Orchestrator Orchestrator
	// Accounts is nil when accounts are off (no YACHT_BASE_URL).
	Accounts *Accounts
	Mailer   Mailer
	Identity IdentityProvider
	// Keeper is non-nil only with YACHT_SECRET_KEY set; Keeper.Configured
	// reports which, and is safe on nil.
	Keeper *Keeper
	Apps   *Apps
	Server *Server

	resolver domain.Resolver
}

// Run composes the engine and serves it until ctx ends.
func Run(ctx context.Context, cfg Config, ov Overrides) error {
	e, err := New(ctx, cfg, ov)
	if err != nil {
		return err
	}
	defer e.Close()
	return e.Serve(ctx)
}

// New composes the engine: migrates and connects the database, connects the
// cluster, and builds every service and the dashboard. Nothing runs yet;
// Start or Serve does that.
func New(ctx context.Context, cfg Config, ov Overrides) (*Engine, error) {
	log := ov.Logger
	if log == nil {
		log = NewLogger(cfg.Debug)
	}
	version := ov.Version
	if version == "" {
		version = "dev"
	}
	log.Info("starting yacht",
		slog.String("version", version),
		slog.String("config", cfg.String()),
	)

	if err := store.Migrate(ctx, cfg.DatabaseURL, log); err != nil {
		return nil, err
	}
	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	e := &Engine{Config: cfg, Log: log, Pool: pool}
	if ov.AfterMigrate != nil {
		if err := ov.AfterMigrate(ctx, pool); err != nil {
			pool.Close()
			return nil, err
		}
	}

	if err := e.compose(ctx, ov, version); err != nil {
		pool.Close()
		return nil, err
	}
	return e, nil
}

func (e *Engine) compose(ctx context.Context, ov Overrides, version string) error {
	cfg, log := e.Config, e.Log

	e.Orchestrator = ov.Orchestrator
	if e.Orchestrator == nil {
		e.Orchestrator = newOrchestrator(ctx, cfg, log)
	}

	if cfg.AccountsEnabled() {
		e.Accounts = account.NewService(e.Pool, log)
		e.Mailer = ov.Mailer
		if e.Mailer == nil {
			m, err := newMailer(cfg, log)
			if err != nil {
				return err
			}
			e.Mailer = m
		}
	}

	if ov.Identity != nil {
		ident, err := ov.Identity(e)
		if err != nil {
			return err
		}
		e.Identity = ident
	} else {
		ident, err := newIdentity(cfg, e.Accounts, log)
		if err != nil {
			return err
		}
		e.Identity = ident
	}

	if cfg.SecretKey != "" {
		keeper, err := secret.NewKeeper(cfg.SecretKey, cfg.SecretKeyPrevious...)
		if err != nil {
			return err
		}
		e.Keeper = keeper
		log.Info("secret key loaded",
			slog.String("key_id", keeper.ActiveKeyID()),
			slog.Int("retired_keys_held", len(keeper.KeyIDs())-1))
	} else {
		log.Warn("no YACHT_SECRET_KEY set — environment variables can be stored, " +
			"but marking one secret will be refused rather than stored readable; " +
			"generate a key with `openssl rand -base64 32`")
	}

	// The registry holds a push credential, so building images needs a key
	// to seal it with; resolving a manifest needs none.
	var images app.Images
	var manifests app.Manifests
	var builder app.Builder
	registryStore := registry.New(e.Pool, e.Keeper, log)
	manifests = registryStore
	if e.Keeper.Configured() {
		images = registryStore
		if b, ok := e.Orchestrator.(orchestrator.Builder); ok {
			builder = b
		}
	}

	e.resolver = domain.AuthoritativeResolver{Fallback: domain.NetResolver{}}
	if cfg.DNSResolver != "" {
		e.resolver = domain.NewDirectResolver(cfg.DNSResolver)
		log.Info("custom domains are resolved by one configured server",
			slog.String("resolver", domain.ResolverName(e.resolver)))
	}

	waker, err := wakerEndpoint(cfg, log)
	if err != nil {
		return err
	}

	e.Apps = app.NewService(e.Pool, e.Orchestrator, log, app.Options{
		Builder:             builder,
		Images:              images,
		Manifests:           manifests,
		MaxConcurrentBuilds: cfg.MaxConcurrentBuilds,
		AppDomain:           cfg.AppDomain,
		WildcardTLS:         cfg.WildcardTLS,
		CertIssuer:          cfg.CertIssuer,
		Keeper:              e.Keeper,
		ReservedDomains:     cfg.ReservedDomains,
		Resolver:            e.resolver,
		Waker:               waker,
		WakeTimeout:         cfg.WakeTimeout,
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
	if cfg.CertIssuer != "" {
		log.Info("custom domains are issued certificates",
			slog.String("cluster_issuer", cfg.CertIssuer),
			slog.String("needs", "public port 80 reaching the cluster, not redirected to HTTPS"))
	}
	if cfg.AppDomain != "" {
		log.Info("per-app hostnames enabled",
			slog.String("app_domain", cfg.AppDomain),
			slog.String("dns", "point *."+cfg.AppDomain+" at this cluster"))
	}

	if err := e.Apps.EnsureOwner(ctx, cfg.OwnerID, cfg.OwnerName, ""); err != nil {
		return err
	}

	opts := web.Options{
		Orchestrator: e.Orchestrator,
		Identity:     e.Identity,
		Apps:         e.Apps,
		Slots:        ov.Slots,
		Extra:        ov.Extra,
		Operators:    cfg.Operators,
		// Accounts are a credential of their own, so the settings page must not
		// report the install as open to anyone merely because no shared token
		// is set.
		Authenticated: cfg.AccountsEnabled() || !cfg.Unauthenticated(),
		Version:       version,
		AppDomain:     cfg.AppDomain,
		WildcardTLS:   cfg.WildcardTLS,
		Logger:        log,
		Nets:          e.Apps,
		Hooks:         e.Apps,
		Logs:          e.Apps,
		Quotas:        e.Apps,
		Capacity:      e.Apps,
		Sleep:         e.Apps,
		WakerBrand:    ov.WakerBrand,
	}

	// The add-node surface only exists where a token could actually be sealed.
	// Without a key the page could store nothing and hand out nothing, so it is
	// left off the router entirely rather than shown and refused. Stacks and
	// the registry are off for the same reason: each holds a credential.
	if e.Keeper.Configured() {
		opts.Joiner = cluster.New(e.Pool, e.Keeper, log)
		opts.Stacks = e.Apps
		opts.Registries = registry.New(e.Pool, e.Keeper, log)
	} else {
		log.Info("add-node and the image registry are off — " +
			"set YACHT_SECRET_KEY to store a cluster join token or registry password")
	}

	if cfg.AccountsEnabled() {
		opts.Accounts = e.Accounts
		opts.Mailer = e.Mailer
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
	e.Server = srv
	return nil
}

// Capacity is the install's room now, for a wrapper deciding what it can
// still sell: how many more of a plan fit is Free divided by the plan's size,
// per resource, taking the smallest. The machines are read from the cluster at
// most every 30 seconds; what the teams have committed is read as it stands.
//
// Known false means the cluster could not be read — nothing is being refused
// for capacity then, and a wrapper should not advertise room it cannot see.
// RoomShort is the one field fit to turn into something a customer is shown.
func (e *Engine) Capacity(ctx context.Context) (CapacitySnapshot, error) {
	return e.Apps.Capacity(ctx)
}

// Handler is the dashboard, with the wrapper's extra routes mounted.
func (e *Engine) Handler() http.Handler { return e.Server.Handler() }

// Waker is the handler a sleeping app's hostnames are routed to: it wakes the
// app, holds or answers the request, and hands it on. Serve runs it on
// Config.WakerListenAddr() beside the dashboard; a wrapper serving the
// dashboard itself serves this there too, on a listener of its own — never on
// the dashboard's, since it answers any hostname the cluster sends it.
func (e *Engine) Waker() http.Handler { return e.Server.Waker() }

// Start runs the engine's background work until ctx ends: the operation
// worker that admits and executes deploys, the reconcilers, the domain
// checker, and request logging. Every loop is safe to run in several
// processes at once, so a wrapper may run replicas.
func (e *Engine) Start(ctx context.Context) {
	apps, log := e.Apps, e.Log

	// Settles builds whose process went away — a restart mid-build, or a
	// replica that stopped. Level-triggered against the Job rather than driven
	// by anything this process remembers, so it is correct after a restart and
	// correct when several replicas run it at once.
	go apps.RunReleaseBackfill(ctx)
	go apps.RunOperationAdmission(ctx)
	go apps.RunReconciler(ctx)
	go apps.RunAppReconciler(ctx)

	// Counts each app's requests and puts the idle ones to sleep, where the
	// install has a waker to wake them again.
	go apps.RunSleeper(ctx)

	// Proves claimed custom domains without anybody pressing anything. What a
	// name resolves to is a fact any replica can look up.
	go domain.NewChecker(e.Pool, e.resolver, apps, log).Run(ctx)

	// Request logging on by default: an app whose traffic is not being
	// recorded is an app nobody can debug. In a goroutine because it talks to
	// the cluster, and an unreachable API server must delay the dashboard
	// coming up, not stop it.
	go apps.EnsureHTTPLogs(ctx)

	// Advances node retirements: one app moved at a time, each after the last
	// has finished. Read from the cluster on every pass, so a retirement
	// resumes after a restart and replicas make the same choice.
	if r, ok := retire.For(e.Orchestrator, log); ok {
		go r.Run(ctx)
	}
}

// Serve starts the background work and serves the dashboard on the configured
// address until ctx ends, then shuts down within the configured timeout. With
// a waker configured it serves that too, on its own address; either listener
// failing stops both.
func (e *Engine) Serve(ctx context.Context) error {
	e.Start(ctx)
	wakerAddr := e.Config.WakerListenAddr()
	if wakerAddr == "" {
		return ServeHTTP(ctx, e.Config.Addr, e.Handler(), e.Config.ShutdownTimeout, e.Log)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() {
		errs <- ServeHTTP(ctx, e.Config.Addr, e.Handler(), e.Config.ShutdownTimeout, e.Log)
	}()
	go func() {
		errs <- ServeHTTP(ctx, wakerAddr, e.Waker(), e.Config.ShutdownTimeout,
			e.Log.With(slog.String("listener", "waker")))
	}()
	err := <-errs
	cancel()
	if second := <-errs; err == nil {
		err = second
	}
	return err
}

// Close releases the database pool. Run does this itself.
func (e *Engine) Close() { e.Pool.Close() }

// ServeHTTP serves handler on addr until ctx ends. Exposed for a wrapper that
// composes the engine's handler into a larger one and serves that instead.
func ServeHTTP(
	ctx context.Context, addr string, handler http.Handler, shutdown time.Duration, log *slog.Logger,
) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", addr))
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdown)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}

// NewLogger is the engine's own logger: text, to stdout, debug when asked.
func NewLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// newOrchestrator connects to a cluster, or falls back to an in-memory stub.
//
// Falling back rather than exiting is deliberate: a self-hoster should be able
// to start the dashboard, see a clear "cluster unreachable" state, and fix
// their kubeconfig from there — rather than face a process that refuses to
// boot and a log line they have to find.
func newOrchestrator(ctx context.Context, cfg Config, log *slog.Logger) Orchestrator {
	orch, err := k8s.New(ctx, k8s.Config{
		InCluster:  cfg.KubeInCluster,
		Kubeconfig: cfg.Kubeconfig,
	}, log)
	if err == nil {
		log.Info("connected to cluster")
		return orch
	}
	log.Warn("cluster unreachable — starting with an in-memory orchestrator; "+
		"deploys will not reach a cluster until this is fixed",
		slog.String("error", err.Error()),
	)
	return orchestrator.NewNoop()
}

// wakerEndpoint is where sleeping apps' hostnames are routed, nil when the
// install has no waker — and then no app sleeps, which is said once here.
func wakerEndpoint(cfg Config, log *slog.Logger) (*orchestrator.WakerEndpoint, error) {
	if cfg.WakerAddr == "" {
		log.Info("apps cannot sleep — set YACHT_WAKER_ADDR to an address the cluster " +
			"reaches the engine at to let idle apps scale to zero and wake on their next request")
		return nil, nil
	}
	ip, port, err := cfg.Waker()
	if err != nil {
		return nil, err
	}
	log.Info("apps can sleep when idle — a sleeping app's hostnames route to the waker",
		slog.String("waker", cfg.WakerAddr), slog.String("listen", cfg.WakerListenAddr()),
		slog.String("needs", "the ingress controller's pods reaching "+cfg.WakerAddr))
	return &orchestrator.WakerEndpoint{IP: ip, Port: port}, nil
}

func newIdentity(cfg Config, accounts *Accounts, log *slog.Logger) (IdentityProvider, error) {
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
		// Operators may act as a team for support only where they are named.
		// With none named every team's owner reads as an operator, and any of
		// them could act as every other team — so the check approves nobody.
		if len(cfg.Operators) == 0 {
			log.Info("acting as a team is off — name the install's operators in " +
				"YACHT_OPERATORS to let them act as a team for support")
		}
		return accounts.Provider(web.SessionCookie).
			WithActingCheck(web.OperatorCheck(cfg.Operators)), nil
	}

	owner := Owner{ID: cfg.OwnerID, DisplayName: cfg.OwnerName}
	if cfg.Unauthenticated() {
		log.Warn("no YACHT_AUTH_TOKEN set — the dashboard is unauthenticated. " +
			"Only run this way on a trusted network.")
		return identity.NewSingleOwner(owner), nil
	}
	log.Info("shared-token authentication — every caller acts as the single owner",
		slog.String("owner", cfg.OwnerID))
	return identity.NewStaticToken(owner, cfg.AuthToken)
}

func newMailer(cfg Config, log *slog.Logger) (Mailer, error) {
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
