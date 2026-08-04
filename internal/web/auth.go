package web

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/notify"
)

// SessionCookie is the name of the cookie carrying a session token.
//
// It matches account.DefaultCookieName because the session provider resolves
// requests by reading a cookie of that name: a mismatch would write a cookie
// nothing reads.
const SessionCookie = "yacht_session"

// requestIsTLS reports whether the request reached us over TLS.
//
// X-Forwarded-Proto is honoured because the common deployment terminates TLS
// at a reverse proxy and forwards plain HTTP. Reading only r.TLS would drop
// the Secure flag on exactly the installs that have a certificate.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setSessionCookie writes the session cookie.
//
// Secure is conditional on purpose. Setting it unconditionally makes the
// browser drop the cookie on a plain-HTTP install, so sign-in appears to
// succeed and then does nothing, with nothing on screen explaining why.
//
// SameSite=Lax is the CSRF defence, which is why no form in this codebase
// carries a token: a cross-site POST does not get the cookie attached.
func setSessionCookie(w http.ResponseWriter, r *http.Request, raw string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsTLS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

// clearSessionCookie expires the session cookie.
//
// The attributes repeat those of setSessionCookie because a browser only
// replaces a cookie whose name, path and domain match: clearing with a
// different Path would leave the live cookie in place and sign-out would do
// nothing.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: requestIsTLS(r), MaxAge: -1,
	})
}

// signInFloor is the minimum time a sign-in POST takes.
//
// Issuing a link for a registered address does an extra INSERT that an
// unregistered address does not, which is measurable — a 3.5x difference was
// demonstrated against this code. Padding to a floor removes the signal
// without pretending the two paths do equal work.
//
// The floor is above the slow path, so the pad is always a wait rather than a
// race the fast path can lose.
const signInFloor = 250 * time.Millisecond

// Attempts allowed in a window, per address and per client.
//
// The floor already makes a single attempt cost 250ms; these are what stop an
// enumeration run from simply being made in parallel. The per-address limit is
// the tighter one because it is the one an enumerator cannot avoid — every
// address they want to test needs its own attempt.
const (
	signInAttemptsPerAddress = 5
	signInAttemptsPerIP      = 20
	signInWindow             = 15 * time.Minute
)

// withFloor runs fn and does not return until d has elapsed.
func withFloor(d time.Duration, fn func()) {
	start := time.Now()
	fn()
	if rest := d - time.Since(start); rest > 0 {
		time.Sleep(rest)
	}
}

// signInPage serves the form. It sits outside the authenticated group: someone
// with no session is exactly who needs it.
func (s *Server) signInPage(w http.ResponseWriter, r *http.Request) {
	s.renderSignedOut(w, r, http.StatusOK, "Sign in", SignIn(SignInData{}))
}

// signInRequest mails a link to the address, if it belongs to anyone.
//
// The visitor is answered identically either way — same page, same status, and,
// through the floor, near enough the same time. Everything that differs between
// a registered and an unregistered address happens inside the floor and is
// visible only in the mailbox of whoever owns the address.
func (s *Server) signInRequest(w http.ResponseWriter, r *http.Request) {
	email, err := parseEmail(r.FormValue("email"))
	if err != nil {
		// Answered without the floor on purpose: malformed input is a mistake
		// the visitor can already see, not a fact about who has an account.
		s.renderSignedOut(w, r, http.StatusUnprocessableEntity, "Sign in", SignIn(SignInData{
			Email: r.FormValue("email"),
			Error: err.Error(),
		}))
		return
	}

	// Both counters are asked, not short-circuited, so an address already over
	// its limit still counts against the client that keeps trying it.
	// Lowercased for the counter because the lookup is case-insensitive: without
	// it, alternating the capitalisation of one address buys an unlimited budget.
	addressOK := s.signInLimit.allow("address:"+strings.ToLower(email), signInAttemptsPerAddress)
	clientOK := s.signInLimit.allow("client:"+clientIP(r), signInAttemptsPerIP)
	if !addressOK || !clientOK {
		// Refusal depends on the attempt count and never on whether the address
		// exists, so saying so plainly leaks nothing.
		s.renderSignedOut(w, r, http.StatusTooManyRequests, "Sign in", SignIn(SignInData{
			Email: email,
			Error: "Too many sign-in attempts. Try again in a few minutes.",
		}))
		return
	}

	withFloor(signInFloor, func() { s.mailSignInLink(r.Context(), email) })

	s.renderSignedOut(w, r, http.StatusOK, "Check your mail", CheckMail())
}

// mailSignInLink issues a link and delivers it, or does neither.
//
// Every failure here is swallowed into the log. The caller renders the same
// page regardless: a lookup error or a refused relay that changed the response
// would be an oracle in its own right, answering "is this address registered?"
// with "the server behaved differently".
func (s *Server) mailSignInLink(ctx context.Context, email string) {
	// A fresh install has nobody, and a link is only issued to somebody — so
	// without this the first person can never get in. Creating the account is
	// confined to the one configured address, and only while it has none: after
	// that this does nothing and the address is ordinary.
	s.bootstrapAccount(ctx, email)

	raw, user, existed, err := s.accounts.IssueMagicLink(ctx, email, s.magicTTL)
	if err != nil {
		s.log.Error("issue sign-in link", slog.String("error", err.Error()))
		return
	}
	if !existed {
		return
	}

	// Built from the configured base URL, never from the Host header: a link
	// built from a header is a link an attacker can point at their own server
	// and have Yacht mail to a real person.
	link := s.baseURL + "/auth/" + raw

	if err := s.mailer.Send(ctx, notify.Message{
		To:       user.Email,
		Subject:  "Your Yacht sign-in link",
		TextBody: signInBody(link, s.magicTTL),
	}); err != nil {
		s.log.Error("send sign-in link",
			slog.String("to", user.Email), slog.String("error", err.Error()))
	}
}

// signInCallback spends a magic link and opens a session on it.
//
// The link is the whole credential, so everything downstream follows from
// ConsumeMagicLink succeeding: the person is who the address says, and the
// cookie is issued for them and for a team they are proven to belong to.
func (s *Server) signInCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	user, err := s.accounts.ConsumeMagicLink(ctx, chi.URLParam(r, "token"))
	if err != nil {
		if !errors.Is(err, account.ErrTokenInvalid) {
			s.log.Error("consume sign-in link", slog.String("error", err.Error()))
		}
		// Unknown, expired and already-spent links are answered alike, because
		// "this link has already been used" tells whoever is holding a guessed
		// token that they guessed a real one.
		s.renderSignedOut(w, r, http.StatusUnauthorized, "Sign in", SignIn(SignInData{
			Error: "That sign-in link is no longer valid. Ask for a new one.",
		}))
		return
	}

	if !s.startSession(w, r, user, account.MethodMagicLink) {
		return
	}

	// See other rather than a temporary redirect: the link has been spent, and
	// a browser that repeated this GET from history would meet a dead token.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// startSession opens a session for a person who has just been proved, and puts
// the cookie on this browser.
//
// The single place a verified person becomes a signed-in browser. Every way in
// funnels through here, so the team bootstrap, the cookie attributes and the
// audit line are written once and cannot drift apart between methods. A second
// call site would be a second chance to forget the bootstrap, and forgetting it
// looks like a sign-in that succeeded and landed on a dashboard with nothing
// on it.
//
// It picks the team itself rather than taking one, deliberately. Somebody whose
// only membership was removed, and the configured owner of an install signing in
// for the first time, both need BootstrapOwner — and that is a thing only this
// function knows to ask for.
//
// Reports whether it succeeded, having already rendered the failure. The caller
// redirects on true and returns on false.
func (s *Server) startSession(
	w http.ResponseWriter, r *http.Request, user account.User, method account.Method,
) bool {
	ctx := r.Context()

	failed := func(what string, err error) bool {
		s.log.Error(what,
			slog.String("user", user.ID.String()), slog.String("error", err.Error()))
		s.renderSignedOut(w, r, http.StatusInternalServerError, "Sign in", SignIn(SignInData{
			Error: "Something went wrong signing you in. Try again in a moment.",
		}))
		return false
	}

	team, err := s.activeTeam(ctx, user)
	if err != nil {
		return failed("find a team to sign in to", err)
	}

	raw, err := s.accounts.CreateSession(
		ctx, user.ID, team, r.UserAgent(), clientIP(r), s.sessionTTL)
	if err != nil {
		return failed("create session", err)
	}

	setSessionCookie(w, r, raw, s.sessionTTL)
	// The method is on the log line and nowhere else. An operator reading an
	// unexpected sign-in needs to know which door it came through, and that is
	// the only thing about the method which survives past this point: nothing
	// downstream is allowed to behave differently by it, because that is how two
	// ways in stop producing the same session.
	s.log.Info("signed in",
		slog.String("user", user.ID.String()),
		slog.String("team", team),
		slog.String("method", string(method)))
	return true
}

// signOut ends the session this browser holds.
//
// It sits outside the authenticated group on purpose. A session can already be
// unusable before anyone clicks sign out — revoked from another browser, or
// ended by the person being removed from the team — and a sign-out that first
// demanded a working session would answer 401 and leave that browser holding a
// cookie it has no way to drop.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	raw := sessionToken(r)
	if raw != "" {
		if err := s.accounts.RevokeSession(r.Context(), raw); err != nil {
			s.log.Error("revoke session", slog.String("error", err.Error()))
			// The cookie is left alone. Clearing it here would make the browser
			// forget a session that is still live in the database, so the person
			// would believe they had signed out and have no handle left to try
			// again with.
			http.Error(w, "could not sign you out", http.StatusInternalServerError)
			return
		}
	}
	s.endSession(w, r)
}

// signOutEverywhere ends every session the person holds, on every device.
//
// This is the answer to a cookie that has been copied off a machine. Without it
// a leaked session runs until it expires and its owner can do nothing at all
// about it.
func (s *Server) signOutEverywhere(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if raw := sessionToken(r); raw != "" {
		// Whose sessions to end comes from resolving the presented cookie, never
		// from the request body. A user id read from a form would be a way to sign
		// anybody out by knowing their id.
		sess, err := s.accounts.ResolveSession(ctx, raw)
		switch {
		case errors.Is(err, account.ErrSessionInvalid):
			// The cookie is already dead. There is nothing to revoke, and saying
			// so would only tell the holder of a guessed token that they missed.
		case err != nil:
			s.log.Error("resolve session", slog.String("error", err.Error()))
			http.Error(w, "could not sign you out", http.StatusInternalServerError)
			return
		default:
			if err := s.accounts.RevokeAllSessions(ctx, sess.UserID); err != nil {
				s.log.Error("revoke every session",
					slog.String("user", sess.UserID.String()), slog.String("error", err.Error()))
				// Reported rather than swallowed: this is the request someone makes
				// when they believe a session has been stolen, and a silent failure
				// leaves them sure the thief is locked out when they are not.
				http.Error(w, "could not sign you out everywhere", http.StatusInternalServerError)
				return
			}
			s.log.Info("signed out everywhere", slog.String("user", sess.UserID.String()))
		}
	}
	s.endSession(w, r)
}

// endSession drops the cookie and sends the browser to the only page it can
// still use.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	// See other rather than a temporary redirect: the POST has been carried out,
	// and a browser that repeated it from history would post to a route that no
	// longer has a session to end.
	http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
}

// sessionToken returns the raw token the request presented, or "".
//
// A missing cookie is not an error here: sign-out has to work for a browser
// whose cookie has already gone.
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// activeTeam picks the team a new session acts as.
//
// Someone with no team at all is the case the bootstrap exists for: on an
// install that has just switched accounts on they are the first person in, and
// the team the install was already running as is theirs to inherit. Without it
// they would sign in successfully and resolve to no owner, which every page
// reads as not being signed in at all.
func (s *Server) activeTeam(ctx context.Context, user account.User) (string, error) {
	teams, err := s.accounts.TeamsFor(ctx, user.ID)
	if err != nil {
		return "", err
	}
	if len(teams) == 0 {
		if err := s.accounts.BootstrapOwner(
			ctx, s.bootstrapID, s.bootstrapName, user); err != nil {
			return "", err
		}
		if teams, err = s.accounts.TeamsFor(ctx, user.ID); err != nil {
			return "", err
		}
		if len(teams) == 0 {
			return "", errors.New("web: the bootstrap left the person with no team")
		}
	}
	// Whichever comes first by team name. A session opens onto one team and the
	// switcher moves it; there is nothing here to prefer, and preferring the
	// most recently used would need a column that does not exist.
	return teams[0].TeamID, nil
}

func signInBody(link string, ttl time.Duration) string {
	return "Someone asked to sign in to Yacht with this address.\n\n" +
		"Follow this link to sign in:\n\n" +
		link + "\n\n" +
		"The link works once and expires in " + ttl.String() + ".\n" +
		"If it was not you, nothing has happened and you can ignore this message.\n"
}

// parseEmail accepts what a person types into the form and returns the address.
//
// mail.ParseAddress also accepts `Name <addr>`; only the address is kept, so a
// display name cannot smuggle anything into what is stored or mailed.
func parseEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errEmailRequired
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address == "" {
		return "", errEmailMalformed
	}
	return addr.Address, nil
}

var (
	errEmailRequired  = signInError("Enter the email address you sign in with.")
	errEmailMalformed = signInError("That does not look like an email address.")
)

// signInError is a message written for the person reading the page rather than
// for a log.
type signInError string

func (e signInError) Error() string { return string(e) }

// clientIP is the address the rate limiter counts against.
//
// Best effort: behind a proxy chi's RealIP has already replaced RemoteAddr from
// X-Forwarded-For, which a direct caller can also set. That is why the
// per-address limit exists and is the tighter of the two — it is the one an
// enumerator cannot dodge by rotating a header.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// attemptLimiter counts attempts per key over a sliding window.
//
// In memory, and so per process: a restart forgives everyone, and two replicas
// each keep their own count. That is the honest trade for having no dependency
// here — the limit is a brake on enumeration, not a security boundary, and the
// boundary is that the response says nothing either way.
type attemptLimiter struct {
	mu     sync.Mutex
	window time.Duration
	hits   map[string][]time.Time
	swept  time.Time
}

// maxTrackedKeys bounds the map between sweeps. Keys are attacker-supplied
// addresses, so an unbounded map is a way to spend the process's memory.
const maxTrackedKeys = 50_000

func newAttemptLimiter(window time.Duration) *attemptLimiter {
	return &attemptLimiter{
		window: window,
		hits:   make(map[string][]time.Time),
		swept:  time.Now(),
	}
}

// allow records an attempt against key and reports whether it is within limit.
func (l *attemptLimiter) allow(key string, limit int) bool {
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.swept) >= l.window || len(l.hits) > maxTrackedKeys {
		for k, ts := range l.hits {
			if kept := recent(ts, cutoff); len(kept) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = kept
			}
		}
		l.swept = now
	}

	ts := recent(l.hits[key], cutoff)
	if len(ts) >= limit {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	return true
}

// recent drops the attempts that have fallen out of the window. The slice is
// appended to in order, so the survivors are its tail.
func recent(ts []time.Time, cutoff time.Time) []time.Time {
	for i, t := range ts {
		if t.After(cutoff) {
			return ts[i:]
		}
	}
	return nil
}

// acceptInvitation spends an invitation, or first establishes who is holding it.
//
// The token is not proof of identity. Acceptance is bound to the address the
// invitation names, so a visitor who is not signed in is sent a sign-in link to
// that address rather than let in — which makes a forwarded or intercepted
// invitation useless to anyone who cannot read that mailbox.
//
// Deliberately outside the authenticated group: the whole point is that the
// person following it may have no account yet.
func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	// Signed in already: spend it. AcceptInvitation checks the address itself,
	// so a signed-in stranger is refused here rather than trusted.
	if sess, err := s.accounts.ResolveSession(ctx, sessionToken(r)); err == nil {
		team, role, err := s.accounts.AcceptInvitation(ctx, token, sess.UserID)
		if err != nil {
			s.invitationFailed(w, r, err)
			return
		}
		s.log.Info("invitation accepted",
			slog.String("team", team), slog.String("role", string(role)))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Not signed in. Read the invitation without spending it, and mail a
	// sign-in link to the address it names.
	inv, err := s.accounts.InvitationFor(ctx, token)
	if err != nil {
		s.invitationFailed(w, r, err)
		return
	}

	raw, _, existed, err := s.accounts.IssueMagicLink(ctx, inv.Email, s.magicTTL)
	if err != nil {
		s.log.Error("issue sign-in link for an invitation", slog.String("error", err.Error()))
		http.Error(w, "could not send a sign-in link", http.StatusInternalServerError)
		return
	}
	if existed || raw != "" {
		msg := notify.Message{
			To:      inv.Email,
			Subject: "Sign in to Yacht to accept your invitation",
			TextBody: "Sign in to accept your invitation:\n\n" +
				s.baseURL + "/auth/" + raw + "\n\n" +
				"Then open the invitation link again.\n" +
				"If you were not expecting this, ignore it.",
		}
		if err := s.mailer.Send(ctx, msg); err != nil {
			s.log.Error("send invitation sign-in link", slog.String("error", err.Error()))
		}
	}

	// The address is not echoed back. Whoever is holding this link may not be
	// the person it was sent to, and telling them which mailbox to watch would
	// be the disclosure the binding exists to prevent.
	s.renderSignedOut(w, r, http.StatusOK, "Check your mail", CheckMail())
}

// invitationFailed answers a link that cannot be spent.
//
// Expired, already accepted, revoked and never-existed are one response on
// purpose: distinguishing them tells whoever is holding a stray token which
// kind of stray it is.
func (s *Server) invitationFailed(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, account.ErrTokenInvalid) {
		s.log.Error("accept invitation", slog.String("error", err.Error()))
		http.Error(w, "could not accept the invitation", http.StatusInternalServerError)
		return
	}
	http.Error(w, "this invitation is no longer valid", http.StatusNotFound)
}

// signInRedirect sends a request with no usable session to the sign-in page.
//
// It runs before identity.Middleware rather than replacing it: the middleware
// stays the single place that resolves an owner, and this only decides what an
// unresolved request should see. Answering 401 is correct for a machine and
// useless to a person, who gets a blank page with nothing to click.
//
// It does not carry the attempted path through. A return-to parameter is an
// open-redirect surface, and the benefit — landing back on the page you asked
// for — is small next to getting that wrong.
// Only GET is redirected. A person navigating needs somewhere to go; a form
// post or an API call with a dead session deserves a plain 401, and turning
// that into a 303 to an HTML page is how a caller ends up parsing a sign-in
// form as though it were a result.
func (s *Server) signInRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if _, err := s.accounts.ResolveSession(r.Context(), sessionToken(r)); err != nil {
			http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bootstrapAccount creates the install's configured owner, once.
//
// The sign-in form must not fill the user table, which is why an unknown
// address is silently ignored everywhere else. That rule leaves a fresh
// install unenterable: no user means no link, and no link means no user. This
// is the single exception — one address, named in configuration, and only
// while it has no account.
//
// Failures are swallowed to the log. The caller renders the same page whatever
// happened here, and an error that changed the response would say whether the
// address is the configured one.
func (s *Server) bootstrapAccount(ctx context.Context, email string) {
	if s.bootstrapEmail == "" || !strings.EqualFold(email, s.bootstrapEmail) {
		return
	}
	created, err := s.accounts.EnsureUser(ctx, email, "")
	if err != nil {
		s.log.Error("bootstrap the configured owner",
			slog.String("error", err.Error()))
		return
	}
	s.log.Info("created the configured owner on first sign-in",
		slog.String("user", created.ID.String()))
}
