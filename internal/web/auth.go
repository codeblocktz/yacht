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

	team, err := s.activeTeam(ctx, user)
	if err != nil {
		s.log.Error("find a team to sign in to",
			slog.String("user", user.ID.String()), slog.String("error", err.Error()))
		s.renderSignedOut(w, r, http.StatusInternalServerError, "Sign in", SignIn(SignInData{
			Error: "Something went wrong signing you in. Try again in a moment.",
		}))
		return
	}

	raw, err := s.accounts.CreateSession(
		ctx, user.ID, team, r.UserAgent(), clientIP(r), s.sessionTTL)
	if err != nil {
		s.log.Error("create session",
			slog.String("user", user.ID.String()), slog.String("error", err.Error()))
		s.renderSignedOut(w, r, http.StatusInternalServerError, "Sign in", SignIn(SignInData{
			Error: "Something went wrong signing you in. Try again in a moment.",
		}))
		return
	}

	setSessionCookie(w, r, raw, s.sessionTTL)
	s.log.Info("signed in",
		slog.String("user", user.ID.String()), slog.String("team", team))

	// See other rather than a temporary redirect: the link has been spent, and
	// a browser that repeated this GET from history would meet a dead token.
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
