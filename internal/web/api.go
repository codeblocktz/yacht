package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/apitoken"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// APIPrefix is where the API is mounted. Versioned in the path, so a change
// that would break a CLI already on somebody's laptop is a /api/v2 beside
// this one rather than a surprise.
const APIPrefix = "/api/v1"

// Tokens is what the API and the token pages need of the token service.
type Tokens interface {
	Create(ctx context.Context, in apitoken.CreateInput) (string, apitoken.Token, error)
	List(ctx context.Context, ownerID string) ([]apitoken.Token, error)
	Revoke(ctx context.Context, ownerID string, actor uuid.UUID, id uuid.UUID) (apitoken.Token, error)
	Resolve(ctx context.Context, raw string) (apitoken.Principal, error)
	Touch(ctx context.Context, id uuid.UUID, ip string)

	StartLogin(ctx context.Context, clientName string) (device, user string, expires time.Time, err error)
	LoginByCode(ctx context.Context, code string) (apitoken.Login, error)
	ApproveLogin(ctx context.Context, code string, actor uuid.UUID, ownerID string, role account.Role) error
	DenyLogin(ctx context.Context, code string) error
	PollLogin(ctx context.Context, device string) (string, apitoken.Token, error)
}

var _ Tokens = (*apitoken.Service)(nil)

// apiPrincipal is who an API request acts as: a token, or — on an install
// with no accounts — the install's one principal, resolved by its identity
// provider, which is the owner.
//
// Its own type on its own key, so a request carries one only if the API's
// authentication put it there; nothing a dashboard route does can produce
// one, and roleOf reads it ahead of any session.
type apiPrincipal struct {
	token *apitoken.Principal
	role  account.Role
}

type apiPrincipalKey struct{}

func apiPrincipalFrom(ctx context.Context) (apiPrincipal, bool) {
	p, ok := ctx.Value(apiPrincipalKey{}).(apiPrincipal)
	return p, ok
}

// apiProject is the one project a request may reach, nil for every project of
// the team.
func apiProject(ctx context.Context) *uuid.UUID {
	if p, ok := apiPrincipalFrom(ctx); ok && p.token != nil {
		return p.token.ProjectID
	}
	return nil
}

// apiError is the API's one error shape. A CLI prints Message; Code is for a
// program deciding what to do.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: apiErrorBody{Code: code, Message: message}})
}

// apiAuthenticate resolves the bearer token on an API request.
//
// With accounts on, only an API token is accepted: a session cookie is never
// read here, which is what keeps the API outside CSRF's reach — a page on
// another site can make a browser send a cookie, and cannot make it send an
// Authorization header. With accounts off, the install's own identity
// provider decides, which for a shared-token install is YACHT_AUTH_TOKEN as a
// bearer token, and that principal is the owner, as on the dashboard.
func (s *Server) apiAuthenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		// Failures are counted per address before anything is looked up, so
		// guessing tokens is slow whatever it is guessing.
		if !s.apiFailures.allowed(ip) {
			writeAPIError(w, http.StatusTooManyRequests, "rate_limited",
				"too many failed attempts from this address — wait a minute")
			return
		}
		ctx := r.Context()
		var owner identity.Owner
		var p apiPrincipal
		if s.accounts != nil {
			raw, ok := bearer(r)
			if !ok || s.tokens == nil {
				s.apiFailures.fail(ip)
				writeAPIError(w, http.StatusUnauthorized, "unauthorized",
					"send an API token as Authorization: Bearer <token>")
				return
			}
			tok, err := s.tokens.Resolve(ctx, raw)
			if err != nil {
				if !errors.Is(err, apitoken.ErrInvalid) {
					s.log.Error("resolve api token", slog.String("error", err.Error()))
				}
				s.apiFailures.fail(ip)
				writeAPIError(w, http.StatusUnauthorized, "unauthorized",
					"that token is not valid — it may have been revoked")
				return
			}
			owner = identity.Owner{ID: tok.OwnerID, DisplayName: tok.TeamName, Email: tok.TeamEmail}
			p = apiPrincipal{token: &tok, role: tok.Role}
			// Recorded off the request: a slow write must not slow the call,
			// and the throttle in the statement makes most of these no-ops.
			go s.tokens.Touch(context.WithoutCancel(ctx), tok.TokenID, ip)
		} else {
			o, err := s.ident.Resolve(ctx, r)
			if err != nil || !o.Valid() {
				s.apiFailures.fail(ip)
				writeAPIError(w, http.StatusUnauthorized, "unauthorized",
					"send the install's token as Authorization: Bearer <token>")
				return
			}
			owner, p = o, apiPrincipal{role: account.RoleOwner}
		}

		key := "owner:" + owner.ID
		if p.token != nil {
			key = "token:" + p.token.TokenID.String()
		}
		if !s.apiRate.allow(key) {
			w.Header().Set("Retry-After", "1")
			writeAPIError(w, http.StatusTooManyRequests, "rate_limited",
				"too many requests with this token — slow down")
			return
		}
		ctx = identity.NewContext(ctx, owner)
		ctx = context.WithValue(ctx, apiPrincipalKey{}, p)
		// A deploy a token makes is recorded as the token's, by name, so the
		// deployments list can say a CI job did it rather than a person.
		if p.token != nil {
			ctx = app.WithActor(ctx, app.Actor{Kind: "token", ID: p.token.TokenName})
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearer reads "Authorization: Bearer <token>".
func bearer(r *http.Request) (string, bool) {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(value) == "" {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// apiRequire is requireRole for the API: the same roleOf, the same minimum
// as the dashboard route doing the same thing, and an answer in JSON.
func (s *Server) apiRequire(min account.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, ok := s.roleOf(r)
			if !ok {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized", "not signed in")
				return
			}
			if !role.AtLeast(min) {
				writeAPIError(w, http.StatusForbidden, "forbidden",
					"this needs a token with the "+string(min)+" role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// mountAPI puts the API on the router. Outside the dashboard's authenticated
// group: no sign-in redirect, no session, no chrome — a token or nothing.
func (s *Server) mountAPI(r chi.Router) {
	if s.accounts != nil && s.tokens == nil {
		// Accounts with no token service has nothing that could authenticate
		// a call. Left off rather than mounted and refusing everything.
		return
	}
	r.Route(APIPrefix, func(r chi.Router) {
		// A build fetching the source it was started with. No token and no
		// team: the credential is the one minted for that build, checked
		// against the upload it names. See apiBuildSource.
		r.Get("/build-sources/{id}", s.apiBuildSource)

		// Signing a CLI in. Unauthenticated by its nature — this is how a
		// CLI with no token gets one — and spending nothing until a person
		// signed in to the dashboard approves it.
		if s.tokens != nil {
			r.Post("/cli/login", s.apiCLILoginStart)
			r.Post("/cli/login/poll", s.apiCLILoginPoll)
		}

		r.Group(func(r chi.Router) {
			r.Use(s.apiAuthenticate)
			r.Use(s.auditTrail)

			// Member: reading, and every change a redeploy undoes — the
			// dashboard's line, drawn in the same place.
			r.Group(func(r chi.Router) {
				r.Use(s.apiRequire(account.RoleMember))
				r.Get("/me", s.apiMe)
				r.Get("/projects", s.apiProjects)
				r.Get("/projects/{slug}/apps", s.apiProjectApps)
				r.Get("/apps", s.apiApps)
				r.Get("/apps/{name}", s.apiAppGet)
				r.Get("/apps/{name}/deployments", s.apiDeployments)
				r.Get("/apps/{name}/deployments/{id}", s.apiDeployment)
				r.Get("/apps/{name}/deployments/{id}/build-log", s.apiBuildLog)
				r.Get("/apps/{name}/variables", s.apiVariables)
				if s.logs != nil {
					r.Get("/apps/{name}/logs", s.apiLogs)
				}
				r.Post("/apps/{name}/redeploy", s.apiRedeploy)
				r.Post("/apps/{name}/deploy-staged", s.apiDeployStaged)
				r.Post("/apps/{name}/rollback", s.apiRollback)
				r.Post("/apps/{name}/up", s.apiUp)
			})

			// Admin: a variable is often the credential the app runs as, and
			// setting one is not undone by a redeploy.
			r.Group(func(r chi.Router) {
				r.Use(s.apiRequire(account.RoleAdmin))
				r.Put("/apps/{name}/variables/{key}", s.apiVariableSet)
				r.Delete("/apps/{name}/variables/{key}", s.apiVariableDelete)
			})
		})

		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			writeAPIError(w, http.StatusNotFound, "not_found", "no such endpoint")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "that method is not allowed here")
		})
	})
}

// apiRateLimiter is a token bucket per token: a burst for a CLI doing several
// things at once, and a steady rate a CI job will not reach by accident.
//
// In memory and per process, like the sign-in limiter: a brake on a runaway
// script, not a quota. Two replicas each allow the rate.
type apiRateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens a second
	burst   float64
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newAPIRateLimiter(perMinute, burst int) *apiRateLimiter {
	return &apiRateLimiter{
		rate: float64(perMinute) / 60, burst: float64(burst),
		buckets: map[string]*bucket{}, swept: time.Now(),
	}
}

func (l *apiRateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > 10*time.Minute || len(l.buckets) > maxTrackedKeys {
		// A bucket that has been full for a while says nothing a new one
		// would not.
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// apiFailureLimiter counts failed authentications per address over a
// minute, so guessing tokens costs a minute per handful of guesses.
type apiFailureLimiter struct {
	l     *attemptLimiter
	limit int
}

func (f *apiFailureLimiter) allowed(ip string) bool {
	f.l.mu.Lock()
	defer f.l.mu.Unlock()
	return len(recent(f.l.hits["fail:"+ip], time.Now().Add(-f.l.window))) < f.limit
}

func (f *apiFailureLimiter) fail(ip string) { f.l.allow("fail:"+ip, 1<<30) }

// The API's limits: a CLI's burst of calls at the start of `up`, a CI job's
// steady polling, and a handful of wrong tokens a minute from one address.
const (
	apiRatePerMinute     = 240
	apiRateBurst         = 60
	apiFailuresPerMinute = 20
)

// APIOwner is the team an API request's bearer token acts as, resolved the
// way the API resolves it, without serving the request: for a wrapper's own
// guard in front of the engine — one that stops a suspended customer's
// deploys, say — which otherwise sees an API call as somebody not signed in.
// False for a request that would be refused as unauthenticated.
func (s *Server) APIOwner(r *http.Request) (identity.Owner, bool) {
	if s.accounts == nil {
		o, err := s.ident.Resolve(r.Context(), r)
		return o, err == nil && o.Valid()
	}
	raw, ok := bearer(r)
	if !ok || s.tokens == nil {
		return identity.Owner{}, false
	}
	p, err := s.tokens.Resolve(r.Context(), raw)
	if err != nil {
		return identity.Owner{}, false
	}
	return identity.Owner{ID: p.OwnerID, DisplayName: p.TeamName, Email: p.TeamEmail}, true
}
