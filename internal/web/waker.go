package web

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
)

// The waker: where a sleeping app's hostnames are routed.
//
// A request arrives here, from the ingress controller, for a hostname whose app
// is asleep. The app is woken — one wake however many requests arrive for it —
// and then the request is answered in whichever way suits who sent it:
//
//   - A browser asking for a page gets a small "waking up" page straight away,
//     which reloads itself until the app answers instead. Holding a browser on
//     a blank tab for ten seconds reads as the site being down.
//   - Anything else — an API client, a webhook, a health check — is held until
//     the app is ready, up to the wake timeout, and then handed to it as though
//     it had never slept. A client that retries on a 503 would retry anyway;
//     one that does not would fail on a request that only needed patience.
//
// When the install has no room to wake it, both are told so plainly — busy,
// try again in a minute — rather than left waiting for a wake that will not
// come.
//
// Served on a listener of its own, never the dashboard's. It answers any
// hostname the cluster sends it, and none of those requests should be able to
// reach a dashboard route by naming one.

// browserHold is how long a browser's request waits before it is given the
// waking-up page. A wake that finishes inside it — an app that starts in a
// second — is simply served.
const browserHold = 1500 * time.Millisecond

// wakeState is what the waking-up page says.
type wakeState string

const (
	wakeStarting wakeState = "waking"
	wakeBusy     wakeState = "busy"
	wakeSlow     wakeState = "slow"
)

// Waker is the handler the engine serves on its waker listener. It is
// nothing but the waker: no dashboard route, no session, no identity.
func (s *Server) Waker() http.Handler {
	return http.HandlerFunc(s.serveWake)
}

func (s *Server) serveWake(w http.ResponseWriter, r *http.Request) {
	if s.sleep == nil {
		http.NotFound(w, r)
		return
	}
	a, err := s.sleep.WakeTarget(r.Context(), requestHost(r))
	if err != nil {
		if !errors.Is(err, app.ErrNotFound) {
			s.log.Error("waker: find the app", slog.String("host", r.Host), slog.String("error", err.Error()))
		}
		http.Error(w, "No app is served at this address.", http.StatusNotFound)
		return
	}

	browser := wantsPage(r)
	hold := r.Context()
	if browser {
		var cancel context.CancelFunc
		hold, cancel = context.WithTimeout(hold, browserHold)
		defer cancel()
	}
	err = s.sleep.WakeForRequest(hold, a)
	switch {
	case err == nil:
		s.proxyToApp(w, r, a, browser)
	case errors.Is(err, app.ErrWakeNoRoom):
		s.wakeAnswer(w, r, a, browser, wakeBusy)
	case errors.Is(err, app.ErrWakeTimeout):
		s.wakeAnswer(w, r, a, browser, wakeSlow)
	case browser && errors.Is(err, context.DeadlineExceeded):
		s.wakeAnswer(w, r, a, browser, wakeStarting)
	case r.Context().Err() != nil:
		// The client went away; there is nobody to answer.
	default:
		s.log.Error("waker: wake", slog.String("app", a.Name), slog.String("error", err.Error()))
		s.wakeAnswer(w, r, a, browser, wakeStarting)
	}
}

// proxyToApp hands the request to the app's own Service, now it is awake.
//
// The ingress controller may still be routing here for a moment after the
// route is switched back, which is fine: the request is answered the same way.
// Where the engine cannot reach the Service at all — it runs outside the
// cluster's network — the request is answered as still waking, and its retry
// goes to the app directly.
func (s *Server) proxyToApp(w http.ResponseWriter, r *http.Request, a app.App, browser bool) {
	addr, err := s.sleep.Backend(r.Context(), a)
	if err != nil {
		s.log.Debug("waker: no backend address", slog.String("app", a.Name), slog.String("error", err.Error()))
		s.wakeAnswer(w, r, a, browser, wakeStarting)
		return
	}
	target := &url.URL{Scheme: "http", Host: addr}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The app sees the request as the ingress controller would have
			// sent it: its own hostname, and the forwarding headers the
			// controller set, which Rewrite otherwise strips.
			pr.Out.Host = pr.In.Host
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Debug("waker: hand the request on", slog.String("app", a.Name), slog.String("error", err.Error()))
			s.wakeAnswer(w, r, a, browser, wakeStarting)
		},
	}
	proxy.ServeHTTP(w, r)
}

// wakeAnswer is a request the app could not take yet: the waking-up page for
// a browser, a plain 503 with Retry-After for anything else.
func (s *Server) wakeAnswer(w http.ResponseWriter, r *http.Request, a app.App, browser bool, state wakeState) {
	retry := wakeRetry(state)
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("Cache-Control", "no-store")
	if !browser {
		http.Error(w, wakeSentence(a.Name, state), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	page := WakingPage(WakingData{
		Brand: s.wakerBrand, App: a.Name, State: state, Refresh: retry,
		Sentence: wakeSentence(a.Name, state),
	})
	if err := page.Render(r.Context(), w); err != nil {
		s.log.Debug("waker: render", slog.String("error", err.Error()))
	}
}

// wakeRetry is how many seconds until the answer is worth asking again for.
func wakeRetry(state wakeState) int {
	switch state {
	case wakeBusy:
		return 60
	case wakeSlow:
		return 15
	}
	return 3
}

// wakeSentence says what is happening, for a person or a log.
func wakeSentence(name string, state wakeState) string {
	switch state {
	case wakeBusy:
		return name + " is asleep, and the server is busy — there is no room to wake it right now. " +
			"Try again in a minute."
	case wakeSlow:
		return name + " took too long to start, so it has gone back to sleep. Try again shortly."
	}
	return name + " was asleep and is waking up. This takes a few seconds."
}

// wantsPage reports whether the request is a browser asking for a page, which
// is answered with the waking-up page rather than held.
func wantsPage(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// requestHost is the hostname the request was for, without a port.
func requestHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// WakingData is the waking-up page.
type WakingData struct {
	Brand    string
	App      string
	State    wakeState
	Refresh  int
	Sentence string
}
