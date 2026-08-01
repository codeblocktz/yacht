package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/notify"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Apps is the workload lifecycle this server drives.
//
// An interface rather than the concrete service so the dashboard can be tested
// without a database. It is defined here, at the consumer, which keeps the app
// package free of knowledge about who calls it.
type Apps interface {
	List(ctx context.Context, ownerID string) ([]app.App, error)
	Count(ctx context.Context, ownerID string) (int64, error)
	Get(ctx context.Context, ownerID, name string) (app.App, error)
	Create(ctx context.Context, ownerID string, in app.CreateInput) (app.App, error)
	Scale(ctx context.Context, ownerID, name string, replicas int32) (app.App, error)
	Redeploy(ctx context.Context, ownerID, name string) error
	Delete(ctx context.Context, ownerID, name string) error
	Deployments(ctx context.Context, ownerID string, appID uuid.UUID, limit int32) ([]app.Deployment, error)

	// Storage. Attaching holds the app to one replica and makes its deploys
	// recreate rather than roll; the service refuses what it cannot honour.
	AttachVolume(ctx context.Context, ownerID, appName string, in app.VolumeInput) (app.Volume, error)
	ResizeVolume(ctx context.Context, ownerID, appName, volumeName string, sizeBytes int64) error
	DeleteVolume(ctx context.Context, ownerID, appName, volumeName string, detach bool) error

	// Environment. A secret's value never comes back out — SetVariable is how
	// it is replaced, and there is no read path for it by design.
	SetVariable(ctx context.Context, ownerID, appName string, in app.VariableInput) error
	DeleteVariable(ctx context.Context, ownerID, appName, key string) error

	// SetHealth points the readiness probe at a path, and optionally lets the
	// same path restart the container.
	SetHealth(ctx context.Context, ownerID, name, healthPath string, liveness bool) error
	RecentActivity(ctx context.Context, ownerID string, limit int32) ([]app.Activity, error)
}

// Accounts is the sign-in surface's view of the account service.
//
// An interface rather than the concrete service, for the same reason Apps is
// one: the sign-in page has to be testable without a database, and this package
// has no business knowing how a person is stored.
type Accounts interface {
	// IssueMagicLink mints a sign-in link, reporting whether the address
	// belongs to anyone. Nothing is mailed when it does not, and nothing about
	// the difference reaches the visitor.
	IssueMagicLink(
		ctx context.Context, email string, ttl time.Duration,
	) (raw string, user account.User, existed bool, err error)

	// EnsureUser creates a person, or returns the one already there. Used only
	// to bootstrap the configured owner of a fresh install.
	EnsureUser(ctx context.Context, email, displayName string) (account.User, error)

	// ConsumeMagicLink spends a link and returns the person it proves. Unknown,
	// expired and already-spent links all come back as account.ErrTokenInvalid.
	ConsumeMagicLink(ctx context.Context, raw string) (account.User, error)

	// TeamsFor lists the teams a person may act as.
	TeamsFor(ctx context.Context, userID uuid.UUID) ([]account.Membership, error)

	// BootstrapOwner gives someone with no team one to act as, handing the
	// first of them the team the install was already running as.
	BootstrapOwner(ctx context.Context, teamID, teamName string, user account.User) error

	// CreateSession opens a session onto a team, returning the token for the
	// cookie. Membership is verified there, not trusted from here.
	CreateSession(
		ctx context.Context, userID uuid.UUID, teamID, userAgent, ip string, ttl time.Duration,
	) (string, error)

	// ResolveSession returns the session a token stands for. It is how sign-out
	// everywhere learns whose sessions to end: from the cookie, never from
	// anything the request says about itself.
	ResolveSession(ctx context.Context, raw string) (account.Session, error)

	// SwitchTeam points a session at another team. Membership is verified there,
	// against the session's own user rather than against anything the request
	// supplied, which is what makes a crafted team id switch into nothing.
	SwitchTeam(ctx context.Context, sessionID uuid.UUID, teamID string) error

	// RevokeSession ends one session — sign-out on this browser.
	RevokeSession(ctx context.Context, raw string) error

	// RevokeAllSessions ends every session a person holds, which is the only
	// answer to a cookie that has been copied off a machine.
	RevokeAllSessions(ctx context.Context, userID uuid.UUID) error

	// InvitationFor reads an invitation without spending it, so a visitor who
	// is not signed in can be sent a link to the address it names instead of
	// being let in on the strength of holding the token.
	InvitationFor(ctx context.Context, raw string) (account.Invitation, error)

	// AcceptInvitation spends one. It checks the address itself, so a
	// signed-in stranger holding the token is refused there rather than here.
	AcceptInvitation(ctx context.Context, raw string, userID uuid.UUID) (string, account.Role, error)

	// ListMembers and ListPendingInvitations are what the team page shows.
	// Both are scoped by team, and the team comes from the caller's session
	// rather than from the request.
	ListMembers(ctx context.Context, teamID string) ([]account.Member, error)
	ListPendingInvitations(ctx context.Context, teamID string) ([]account.Invitation, error)

	// Invite issues an invitation and returns the token to mail. The token is
	// returned rather than stored anywhere readable, so the message is the only
	// place it exists.
	Invite(
		ctx context.Context, actor uuid.UUID, teamID, email string,
		role account.Role, ttl time.Duration,
	) (string, error)

	// RevokeInvitation withdraws one. The mail cannot be recalled, so deleting
	// the row is the only way to take an invitation back.
	RevokeInvitation(ctx context.Context, actor uuid.UUID, teamID string, id uuid.UUID) error

	// SetRole and RemoveMember change who may do what. Both check the actor's
	// own authority and refuse to leave the team without an owner.
	SetRole(ctx context.Context, actor uuid.UUID, teamID string, target uuid.UUID, role account.Role) error
	RemoveMember(ctx context.Context, actor uuid.UUID, teamID string, target uuid.UUID) error
}

// The engine's own implementation, asserted here so that a signature drifting
// apart from this interface fails the build in the package that defines it.
var _ Accounts = (*account.Service)(nil)

// Options configures the dashboard server.
//
// All three seams arrive here as dependencies. Nothing in this package
// constructs an orchestrator, resolves an identity, or decides what the chrome
// looks like — which is what makes the whole surface reusable by an
// application the engine has never heard of.
type Options struct {
	Orchestrator orchestrator.Orchestrator
	Identity     identity.Provider
	Apps         Apps

	// Slots is optional; DefaultSlots is used when nil.
	Slots SlotProvider

	// Authenticated reports whether a credential is required, purely so the
	// settings page can warn when it is not.
	Authenticated bool
	Version       string

	// AppDomain and WildcardTLS describe the platform's hostname policy. The
	// dashboard reads them to explain the install on the settings page; the
	// per-app scheme comes from app.App.TLS, which the app service populates.
	AppDomain   string
	WildcardTLS bool

	// Accounts, when set, puts the sign-in surface on the router. Left nil the
	// engine serves no sign-in page at all, which is right for an install
	// resolved by a shared token: a form that could never issue a session is a
	// worse answer than no form.
	Accounts Accounts

	// Mailer delivers sign-in links. Defaults to the logging transport, which
	// is the break-glass path when mail breaks after accounts are switched on.
	Mailer notify.Mailer

	// BaseURL is the public URL links are built from. Required with Accounts,
	// and deliberately not derived from the request: a link built from the Host
	// header is one an attacker can point at their own server and have Yacht
	// mail to a real person.
	BaseURL string

	// MagicLinkTTL is how long a sign-in link stays redeemable. Defaults to
	// defaultMagicLinkTTL.
	MagicLinkTTL time.Duration

	// SessionTTL is how long a signed-in browser stays signed in. Defaults to
	// defaultSessionTTL.
	SessionTTL time.Duration

	// MailTransport names the delivery route — "smtp", "resend" or "log" — so
	// the settings page can show whether links are actually being sent.
	MailTransport string

	// BootstrapEmail is the address allowed to create itself on first sign-in.
	//
	// A link is only ever issued to an address that already has an account,
	// which is what stops the sign-in form filling the user table. On a fresh
	// install that leaves nobody able to get in at all: no user, so no link, so
	// no user. This names the one address exempt from that, and only while it
	// has no account — once it does, it is an ordinary person like anyone else.
	BootstrapEmail string

	// BootstrapTeamID and BootstrapTeamName are the install's configured owner
	// — YACHT_OWNER_ID and YACHT_OWNER_NAME. The first person to sign in
	// inherits that team, so the apps an install already deployed under it stay
	// reachable once accounts are switched on. Required with Accounts.
	BootstrapTeamID   string
	BootstrapTeamName string

	Logger *slog.Logger
}

// defaultMagicLinkTTL and defaultSessionTTL match config's defaults, so a
// server built without them does not quietly outlive what the operator
// configured.
const (
	defaultMagicLinkTTL = 15 * time.Minute
	defaultSessionTTL   = 720 * time.Hour
)

// Server serves the dashboard.
type Server struct {
	orch      orchestrator.Orchestrator
	ident     identity.Provider
	apps      Apps
	slots     SlotProvider
	authn     bool
	ver       string
	appDomain string
	wildcard  bool
	log       *slog.Logger

	accounts       Accounts
	mailer         notify.Mailer
	baseURL        string
	magicTTL       time.Duration
	sessionTTL     time.Duration
	bootstrapID    string
	bootstrapName  string
	bootstrapEmail string

	// mailTransport names how links are delivered, purely so the settings page
	// can say. "log" means nothing is actually sent.
	mailTransport string
	signInLimit   *attemptLimiter
}

// New validates options and returns a server.
func New(opts Options) (*Server, error) {
	if opts.Orchestrator == nil {
		return nil, errors.New("web: an Orchestrator is required")
	}
	if opts.Identity == nil {
		return nil, errors.New("web: an identity Provider is required")
	}
	if opts.Slots == nil {
		opts.Slots = DefaultSlots{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.Accounts != nil {
		if opts.BaseURL == "" {
			return nil, errors.New(
				"web: a BaseURL is required with Accounts — sign-in links cannot be " +
					"built from the request without letting a Host header decide where " +
					"they point")
		}
		// Refused rather than defaulted: without it the first person to sign in
		// lands in a fresh empty team, and the apps the install already deployed
		// belong to an owner nobody can authenticate as. That failure is silent
		// and arrives on the day accounts are switched on.
		if opts.BootstrapTeamID == "" {
			return nil, errors.New(
				"web: a BootstrapTeamID is required with Accounts — without the " +
					"configured owner id the first person to sign in cannot inherit " +
					"the team the install has been running as")
		}
		if opts.Mailer == nil {
			opts.Mailer = notify.NewLog(opts.Logger)
		}
		if opts.MagicLinkTTL <= 0 {
			opts.MagicLinkTTL = defaultMagicLinkTTL
		}
		if opts.SessionTTL <= 0 {
			opts.SessionTTL = defaultSessionTTL
		}
	}
	return &Server{
		orch:      opts.Orchestrator,
		ident:     opts.Identity,
		apps:      opts.Apps,
		slots:     opts.Slots,
		authn:     opts.Authenticated,
		ver:       opts.Version,
		appDomain: opts.AppDomain,
		wildcard:  opts.WildcardTLS,
		log:       opts.Logger,

		accounts:       opts.Accounts,
		mailer:         opts.Mailer,
		baseURL:        strings.TrimRight(opts.BaseURL, "/"),
		magicTTL:       opts.MagicLinkTTL,
		sessionTTL:     opts.SessionTTL,
		bootstrapID:    opts.BootstrapTeamID,
		bootstrapName:  opts.BootstrapTeamName,
		bootstrapEmail: strings.TrimSpace(opts.BootstrapEmail),
		mailTransport:  cmp.Or(opts.MailTransport, "log"),
		signInLimit:    newAttemptLimiter(signInWindow),
	}, nil
}

// Handler returns the HTTP routes.
//
// Authenticated routes live in a group behind identity.Middleware rather than
// each checking for themselves. A per-handler check is one a handler can
// forget, and the forgotten one is the security bug.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Recoverer)

	// A trailing slash is the same page. /apps used to answer it because it was
	// a mounted subtree; the groups below are flat, so it is stated here instead
	// of leaving a typed URL to 404. Paths are normalised before routing, so no
	// pattern — and no gate on one — can be stepped around with a slash.
	r.Use(chimw.StripSlashes)

	// Health is deliberately outside the authenticated group: a load balancer
	// has no credentials, and a health check that needs them is not one.
	r.Get("/healthz", s.health)

	// Static assets are embedded and carry no owner data, so they sit outside
	// the authenticated group too — otherwise the stylesheet 401s on the login
	// page and every error screen renders unstyled.
	//
	// Registered for the two methods that read, rather than for every method: a
	// file server answering POST is a mutating verb on a route that has no role
	// gate and can never need one, which is a thing the next reader has to work
	// out is harmless.
	assets := assetHandler()
	r.Get("/assets/*", assets.ServeHTTP)
	r.Head("/assets/*", assets.ServeHTTP)

	// Sign-in is outside the authenticated group by necessity: someone with no
	// session is exactly who needs it. The POST is a mutating route with no role
	// gate for the same reason — there is no session to read a role from yet.
	if s.accounts != nil {
		r.Get("/sign-in", s.signInPage)
		r.Post("/sign-in", s.signInRequest)
		// The callback is a GET because it is a link in a mail message and
		// nothing else can be, which is the one exception to every mutating
		// route being a POST. What makes it tolerable is that the token is
		// single-use and short-lived: a mail client that prefetches links spends
		// one its own recipient asked for, and a crawler has nothing to guess.
		r.Get("/auth/{token}", s.signInCallback)

		// Outside the authenticated group because the point of an invitation is
		// that the person following it may not have an account yet. The handler
		// establishes who they are before spending anything — the token is not
		// treated as proof of identity.
		r.Get("/invitations/{token}", s.acceptInvitation)

		// Sign-out is POST only. A GET that ends a session is one a prefetching
		// browser, a crawler or an <img> tag on another site can fire, and being
		// signed out by a page you visited is a denial of service with no error
		// message. SameSite=Lax keeps the cookie off cross-site POSTs, which is
		// what makes the POST safe without a token.
		//
		// Both sit outside the authenticated group deliberately: a browser whose
		// session has already been revoked still has to be able to clear its
		// cookie, and a 401 would deny it exactly that.
		r.Post("/sign-out", s.signOut)
		r.Post("/sign-out-everywhere", s.signOutEverywhere)
	}

	r.Group(func(r chi.Router) {
		// With accounts on, an unresolved request is sent to the sign-in page
		// rather than answered 401. A bare 401 is a dead end in a browser:
		// nothing on it to click, and no way to discover where sign-in lives.
		//
		// Only with accounts on. A token-authenticated install has no page to
		// send anyone to, and redirecting an API caller to HTML would turn a
		// clear 401 into a confusing 200.
		if s.accounts != nil {
			r.Use(s.signInRedirect)
		}
		r.Use(identity.Middleware(s.ident))

		// The teams a person may act as, for whatever renders the chrome. Mounted
		// as middleware rather than fetched by each handler, because the switcher
		// belongs to the layout and not to any one page.
		if s.accounts != nil {
			r.Use(s.withTeams)
		}

		// The split between these groups is at the line where an action stops
		// being undoable by redeploying. Each gate is on the group rather than in
		// the handlers, so a route added to one of them later is protected
		// whether or not its author thought about roles at all.
		//
		// The paths are written out flat rather than nested under r.Route: a
		// mounted subtree can only belong to one group, and /apps has routes in
		// two.

		// Member: reading everything, and every change a redeploy can undo.
		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(account.RoleMember))

			r.Get("/", s.overview)

			r.Get("/apps", s.appList)
			r.Post("/apps", s.appCreate)
			r.Get("/apps/new", s.appNew)
			r.Get("/apps/{name}", s.appDetail)
			r.Get("/apps/{name}/variables", s.appDetail)
			r.Get("/apps/{name}/metrics", s.appDetail)
			r.Get("/apps/{name}/storage", s.appDetail)
			r.Get("/apps/{name}/settings", s.appDetail)
			r.Post("/apps/{name}/scale", s.appScale)
			r.Post("/apps/{name}/redeploy", s.appRedeploy)

			r.Get("/deployments", s.activity)

			// Readable by any member: knowing who else is in your team is not a
			// privilege. The page offers only the controls the viewer's role can
			// actually use, and every one of those is gated on its own route.
			if s.accounts != nil {
				r.Get("/team", s.teamPage)
			}

			r.Get("/cluster", s.clusterNodes)
			r.Get("/cluster/nodes", s.clusterNodes)
			r.Get("/cluster/pods", s.clusterPods)
			r.Get("/cluster/volumes", s.clusterVolumes)
			r.Get("/cluster/events", s.clusterEvents)
			r.Get("/settings", s.settings)

			// Switching team is gated at member because that is what holding any
			// session amounts to — the gate proves this browser is acting in a team
			// it belongs to. Which team it may move to is a separate question, and
			// one only the account service can answer, so it answers it.
			//
			// POST because it changes what every later request is scoped by. A GET
			// would let a prefetch or a crawler move somebody into another team.
			if s.accounts != nil {
				r.Post("/teams/switch", s.teamSwitch)
			}
		})

		// Admin: everything a redeploy cannot undo, and who is in the team.
		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(account.RoleAdmin))

			r.Post("/apps/{name}/delete", s.appDelete)

			// Storage sits with the admin actions rather than the member ones:
			// attaching changes how the app deploys, and deleting destroys what
			// it holds. Neither is undoable by redeploying, which is the line.
			// Variables sit with the admin actions: a value here is often the
			// credential the app runs as, and setting one is not undone by a
			// redeploy.
			r.Post("/apps/{name}/health", s.healthSet)
			r.Post("/apps/{name}/variables", s.variableSet)
			r.Post("/apps/{name}/variables/{key}/delete", s.variableDelete)

			r.Post("/apps/{name}/storage", s.storageAttach)
			r.Post("/apps/{name}/storage/{volume}/resize", s.storageResize)
			r.Post("/apps/{name}/storage/{volume}/delete", s.storageDelete)

			if s.accounts != nil {
				r.Post("/team/invite", s.teamInvite)
				r.Post("/team/invitations/{id}/revoke", s.teamRevokeInvitation)
				r.Post("/team/members/{id}/remove", s.teamRemoveMember)
			}
		})

		// Owner: everything that changes who can administer.
		//
		// Appointing an admin is ownership. An admin allowed to appoint another
		// admin could appoint an owner next, which is every permission the team
		// has.
		if s.accounts != nil {
			r.Group(func(r chi.Router) {
				r.Use(s.requireRole(account.RoleOwner))

				r.Post("/team/members/{id}/role", s.teamSetRole)

				// Deleting a team is deliberately absent rather than stubbed. It
				// cascades to every app row the team owns while leaving the
				// workloads running in the cluster, and nothing here reconciles
				// that. A route answering 501 is worse than no route: advertised,
				// gated, and useless.
			})
		}
	})

	return r
}

// detailTab derives which tab is selected from the trailing path segment,
// rather than a query parameter, so each tab is a real bookmarkable URL.
func detailTab(r *http.Request) string {
	switch {
	case strings.HasSuffix(r.URL.Path, "/variables"):
		return "variables"
	case strings.HasSuffix(r.URL.Path, "/metrics"):
		return "metrics"
	case strings.HasSuffix(r.URL.Path, "/storage"):
		return "storage"
	case strings.HasSuffix(r.URL.Path, "/settings"):
		return "settings"
	}
	return ""
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"status": "ok", "version": s.ver}
	code := http.StatusOK

	if err := s.orch.Ping(r.Context()); err != nil {
		body["status"] = "degraded"
		body["cluster"] = err.Error()
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	data := OverviewData{OwnerName: displayName(owner), ClusterOK: true}
	if err := s.orch.Ping(ctx); err != nil {
		data.ClusterOK = false
		data.ClusterError = err.Error()
	}
	if summary, err := s.orch.ClusterSummary(ctx); err == nil {
		data.Summary = summary
	}
	if s.apps != nil {
		if apps, err := s.apps.List(ctx, owner.ID); err == nil {
			data.Apps = apps
			data.AppCount = int64(len(apps))
		} else {
			s.log.Error("list apps", slog.String("error", err.Error()))
		}
	}

	s.render(w, r, Overview(data))
}

func (s *Server) appList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	var apps []app.App
	if s.apps != nil {
		var err error
		if apps, err = s.apps.List(ctx, owner.ID); err != nil {
			s.log.Error("list apps", slog.String("error", err.Error()))
		}
	}
	s.render(w, r, AppList(apps))
}

func (s *Server) appNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, NewApp(NewAppData{
		Form: NewAppForm{Replicas: "1", Port: "8080"},
	}))
}

func (s *Server) appCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	if s.apps == nil {
		s.renderErr(w, r, NewAppForm{}, "no database is configured")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderErr(w, r, NewAppForm{}, "could not read the form")
		return
	}

	form := NewAppForm{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Image:    strings.TrimSpace(r.FormValue("image")),
		Port:     r.FormValue("port"),
		Replicas: r.FormValue("replicas"),
		Env:      r.FormValue("env"),
	}

	port, err := parseIntField(form.Port, 0)
	if err != nil {
		s.renderErr(w, r, form, "port must be a number")
		return
	}
	replicas, err := parseIntField(form.Replicas, 1)
	if err != nil {
		s.renderErr(w, r, form, "replicas must be a number")
		return
	}
	env, err := parseEnv(form.Env)
	if err != nil {
		s.renderErr(w, r, form, err.Error())
		return
	}

	_, err = s.apps.Create(ctx, owner.ID, app.CreateInput{
		Name:     form.Name,
		Image:    form.Image,
		Port:     int32(port),
		Replicas: int32(replicas),
		Env:      env,
	})
	if err != nil {
		s.log.Warn("create app failed",
			slog.String("name", form.Name), slog.String("error", err.Error()))
		s.renderErr(w, r, form, err.Error())
		return
	}

	http.Redirect(w, r, "/apps/"+form.Name, http.StatusSeeOther)
}

func (s *Server) renderErr(w http.ResponseWriter, r *http.Request, form NewAppForm, msg string) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, NewApp(NewAppData{Error: msg, Form: form}))
}

func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	if s.apps == nil {
		http.NotFound(w, r)
		return
	}

	a, err := s.apps.Get(ctx, owner.ID, name)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("get app", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := AppDetailData{App: a, Tab: detailTab(r)}

	if pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{
		Namespace: a.Namespace, ManagedOnly: true,
	}); err == nil {
		data.Pods = pods
	}

	// The rail lists every app so you can move between them without going
	// back to the index. Status is already attached by List.
	if siblings, err := s.apps.List(ctx, owner.ID); err == nil {
		data.Siblings = siblings
	}

	if deps, err := s.apps.Deployments(ctx, owner.ID, a.ID, 20); err == nil {
		data.Deployments = deps
	} else {
		s.log.Error("list deployments", slog.String("error", err.Error()))
	}

	s.renderWithCrumb(w, r, AppDetail(data), a.Name)
}

func (s *Server) appScale(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")

	replicas, err := parseIntField(r.FormValue("replicas"), 1)
	if err != nil || s.apps == nil {
		http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
		return
	}
	if _, err := s.apps.Scale(r.Context(), owner.ID, name, int32(replicas)); err != nil {
		s.log.Error("scale", slog.String("app", name), slog.String("error", err.Error()))
	}
	http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
}

func (s *Server) appRedeploy(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")

	if s.apps != nil {
		if err := s.apps.Redeploy(r.Context(), owner.ID, name); err != nil {
			s.log.Error("redeploy", slog.String("app", name), slog.String("error", err.Error()))
		}
	}
	http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
}

func (s *Server) appDelete(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")

	if s.apps != nil {
		if err := s.apps.Delete(r.Context(), owner.ID, name); err != nil {
			s.log.Error("delete", slog.String("app", name), slog.String("error", err.Error()))
		}
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	var data ActivityData
	if s.apps != nil {
		items, err := s.apps.RecentActivity(ctx, owner.ID, 50)
		if err != nil {
			s.log.Error("recent activity", slog.String("error", err.Error()))
		}
		data.Items = items
	}
	s.render(w, r, Activity(data))
}

func (s *Server) clusterVolumes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := VolumesData{OK: true}

	// The owner comes from the resolved identity, never from the request. A
	// cluster page that listed every namespace was correct while the engine had
	// one owner and is a disclosure now that it has teams.
	owner := identity.MustFromContext(ctx)

	volumes, err := s.orch.Volumes(ctx, orchestrator.OwnerID(owner.ID))
	if err != nil {
		data.OK = false
		data.Error = err.Error()
	}
	data.Volumes = volumes

	s.render(w, r, Volumes(data))
}

func (s *Server) clusterEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := EventsData{OK: true}

	events, err := s.orch.Events(ctx, 100)
	if err != nil {
		data.OK = false
		data.Error = err.Error()
	}
	data.Events = events

	s.render(w, r, Events(data))
}

func (s *Server) clusterNodes(w http.ResponseWriter, r *http.Request) {
	s.renderCluster(w, r, "nodes")
}

func (s *Server) clusterPods(w http.ResponseWriter, r *http.Request) {
	s.renderCluster(w, r, "pods")
}

func (s *Server) renderCluster(w http.ResponseWriter, r *http.Request, tab string) {
	ctx := r.Context()
	data := ClusterData{Tab: tab, OK: true}

	if err := s.orch.Ping(ctx); err != nil {
		data.OK = false
		data.Error = err.Error()
	}
	if summary, err := s.orch.ClusterSummary(ctx); err == nil {
		data.Summary = summary
	} else {
		data.OK = false
		data.Error = err.Error()
	}

	switch tab {
	case "pods":
		if pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{
			Owner: orchestrator.OwnerID(identity.MustFromContext(ctx).ID),
		}); err == nil {
			data.Pods = pods
		}
	default:
		if nodes, err := s.orch.Nodes(ctx); err == nil {
			data.Nodes = nodes
		}
	}

	s.render(w, r, Cluster(data))
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	s.render(w, r, Settings(SettingsData{
		OwnerID:       owner.ID,
		OwnerName:     displayName(owner),
		Authenticated: s.authn,
		Version:       s.ver,
		ClusterOK:     s.orch.Ping(r.Context()) == nil,
		AppDomain:     s.appDomain,
		WildcardTLS:   s.wildcard,
		AccountsOn:    s.accounts != nil,
		MailTransport: s.mailTransport,
		SessionTTL:    s.sessionTTL.String(),
	}))
}

// renderWithCrumb renders a page and appends a final breadcrumb segment.
//
// The SlotProvider cannot know an app's name — only the handler that loaded it
// can — so the trailing crumb is appended here rather than pushing resource
// lookups into the chrome.
func (s *Server) renderWithCrumb(
	w http.ResponseWriter, r *http.Request, page templ.Component, crumb string,
) {
	slots := s.slots.Slots(r.Context(), r)
	if crumb != "" {
		slots.Breadcrumb = append(slots.Breadcrumb, Crumb{Label: crumb})
	}
	s.renderWithSlots(w, r, slots, page)
}

// renderSignedOut renders a page for a visitor who has no session.
//
// The provider's brand survives, so an application wrapping the engine still
// presents its own product on the page where people arrive. Its navigation does
// not: every link in it needs a session, and offering a signed-out visitor a
// column of links that will bounce them straight back here is worse than
// offering none.
func (s *Server) renderSignedOut(
	w http.ResponseWriter, r *http.Request, status int, title string, page templ.Component,
) {
	slots := s.slots.Slots(r.Context(), r)
	slots.Title = title
	slots.Nav = nil
	slots.Breadcrumb = nil
	slots.HeaderTools = nil
	slots.SidebarTop = nil
	slots.SidebarFooter = nil
	slots.Banner = nil

	// Content-Type before the status line: set afterwards it is dropped, and a
	// rejected address would come back as a page the browser renders as text.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := Layout(slots, page).Render(r.Context(), w); err != nil {
		s.log.Error("render failed",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
	}
}

// render wraps a page in the layout, filling the chrome from the injected
// SlotProvider.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page templ.Component) {
	s.renderWithSlots(w, r, s.slots.Slots(r.Context(), r), page)
}

func (s *Server) renderWithSlots(
	w http.ResponseWriter, r *http.Request, slots Slots, page templ.Component,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := Layout(slots, page).Render(r.Context(), w); err != nil {
		// The response is already partly written by this point, so there is
		// nothing useful to send the client. Log and move on.
		s.log.Error("render failed",
			slog.String("path", r.URL.Path),
			slog.String("error", err.Error()),
		)
	}
}

func displayName(o identity.Owner) string {
	if o.DisplayName != "" {
		return o.DisplayName
	}
	return o.ID
}

func parseIntField(s string, fallback int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback, nil
	}
	return strconv.Atoi(s)
}

// parseEnv reads KEY=value lines.
//
// Blank lines and # comments are skipped so a user can paste a .env file
// without editing it first.
func parseEnv(raw string) (map[string]string, error) {
	out := map[string]string{}
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, errors.New("environment must be one KEY=value per line")
		}
		out[key] = strings.TrimSpace(value)
	}
	return out, nil
}
