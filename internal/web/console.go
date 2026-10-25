package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Console opens a shell in one of an app's containers.
//
// Asserted for on Apps rather than given an option of its own, the way the
// app service asserts for an orchestrator's optional parts: the service that
// owns an app is the one that can say whether a shell may be opened in it,
// and an Apps that cannot — a wrapping application's, a test's — leaves the
// socket off the router without anybody having to remember to.
type Console interface {
	Console(
		ctx context.Context, ownerID, appName string, req app.ConsoleRequest,
	) (app.ConsoleSession, error)
}

// consoler is the server's Console, nil when its Apps has none.
func (s *Server) consoler() Console {
	c, _ := s.apps.(Console)
	return c
}

// mountConsole puts the console tab, and the socket behind it, on a router.
//
// Called from the member group. A console is a shell inside the app's
// container, which sounds like more than a member should hold — but a member
// can already set the app's variables and deploy any image as it, and either
// runs whatever code they like in that same container with the same
// identity. A console gives them nothing those do not; it only makes the
// thing they could already do take seconds rather than a deploy.
func (s *Server) mountConsole(r chi.Router) {
	r.Get("/apps/{name}/console", s.appConsole)
	if s.consoler() != nil {
		r.Get("/apps/{name}/console/ws", s.appConsoleSocket)
	}
}

// consoleView is what the console tab needs that the panel does not carry:
// whether a console can be opened at all, and which pod was asked for.
//
// On the context rather than on AppDetailData, so the tab is the only thing
// that knows the console exists.
type consoleView struct {
	On  bool
	Pod string
}

type consoleViewKey struct{}

func consoleViewFrom(ctx context.Context) consoleView {
	v, _ := ctx.Value(consoleViewKey{}).(consoleView)
	return v
}

// appConsole draws the app's panel open on its Console tab.
func (s *Server) appConsole(w http.ResponseWriter, r *http.Request) {
	v := consoleView{On: s.consoler() != nil, Pod: r.URL.Query().Get("pod")}
	ctx := context.WithValue(r.Context(), consoleViewKey{}, v)
	s.renderCanvasWith(w, r.WithContext(ctx), "", chi.URLParam(r, "name"), "console", nil)
}

// consoleLimits bound one console.
type consoleLimits struct {
	// Idle ends a console nobody has typed into. A forgotten tab is a shell
	// left open inside a production container, logged in as whoever opened
	// it, for as long as the laptop stays awake.
	Idle time.Duration

	// Max ends any console, busy or not. A session that never ends is one
	// whose holder's access was revoked hours ago and still works.
	Max time.Duration

	// Ping is how often the far end is asked whether it is still there. A
	// laptop that sleeps sends no close, and without asking, the shell would
	// sit open until Idle noticed.
	Ping time.Duration
}

var defaultConsoleLimits = consoleLimits{
	Idle: 15 * time.Minute,
	Max:  2 * time.Hour,
	Ping: 30 * time.Second,
}

const (
	// consoleWriteWait is how long one frame may take to leave. A browser that
	// stops reading is a console that should end, not a goroutine that waits.
	consoleWriteWait = 10 * time.Second

	// consoleReadLimit is the largest frame accepted. console.js sends input in
	// pieces well under it, so only something other than the dashboard sends
	// more — and a paste is not a reason to buffer megabytes per connection.
	consoleReadLimit = 64 << 10
)

// Close codes a console ends with. The message sent just before says why in
// words; the code is what console.js decides whether to reconnect by.
const (
	consoleCloseExited  = websocket.CloseNormalClosure
	consoleCloseFailed  = websocket.CloseInternalServerErr
	consoleCloseRefused = 4000
	consoleCloseIdle    = 4001
	consoleCloseMax     = 4002
	consoleCloseNoPod   = 4004
)

// consoleMessage is the console's own say, sent as a text frame. Terminal
// output is binary frames and nothing else, so the two can never be confused
// — a program that prints JSON is still only printing.
type consoleMessage struct {
	Type    string   `json:"type"` // connected, error or closed
	Pod     string   `json:"pod,omitempty"`
	Pods    []string `json:"pods,omitempty"`
	Message string   `json:"message,omitempty"`
}

// consoleInput is a text frame from the browser. Keystrokes arrive as binary
// frames; the only thing said in text is the terminal's size.
type consoleInput struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// consoleOriginAllowed reports whether the upgrade comes from the dashboard.
//
// A WebSocket handshake is a GET, so csrfProtect waves it through, and a
// browser attaches the session cookie to a cross-site socket on a shared
// parent domain the way it does to a form — which is exactly the case that
// check exists for. Without this, any page an app on the install serves could
// open a shell in that visitor's apps. The rule is csrfProtect's: the Origin
// must be the configured dashboard origin, or this request's own where none is
// configured. Browsers always send Origin on a handshake, so a missing one is
// refused rather than excused.
func (s *Server) consoleOriginAllowed(r *http.Request) bool {
	want := originOf(s.baseURL)
	if want == "" {
		want = requestOrigin(r)
	}
	got := strings.TrimSpace(r.Header.Get("Origin"))
	return got != "" && want != "" && originOf(got) == want
}

// appConsoleSocket is the terminal: bytes in, bytes out, until the shell ends.
func (s *Server) appConsoleSocket(w http.ResponseWriter, r *http.Request) {
	s.serveConsole(w, r, defaultConsoleLimits)
}

func (s *Server) serveConsole(w http.ResponseWriter, r *http.Request, lim consoleLimits) {
	if !s.consoleOriginAllowed(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "the console is a WebSocket", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	sess, err := s.consoler().Console(ctx, owner.ID, name, app.ConsoleRequest{
		Pod: r.URL.Query().Get("pod"),
		// The person where there is one, so the record names an operator
		// acting for a customer rather than the customer.
		Actor: cmp.Or(s.PersonEmail(r), displayName(owner)),
	})

	// Upgraded even to refuse. A browser is told nothing about a handshake that
	// failed — no status, no body — so a refusal sent as an HTTP error would
	// reach the person as "could not connect", whatever the reason. Only the
	// checks above, which no page of ours fails, answer before the upgrade.
	up := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4 << 10,
		WriteBufferSize:  16 << 10,
		CheckOrigin:      s.consoleOriginAllowed,
	}
	ws, uerr := up.Upgrade(w, r, nil)
	if uerr != nil {
		// Upgrade has already answered with an HTTP error.
		return
	}
	c := &consoleConn{ws: ws}
	defer ws.Close() //nolint:errcheck // the connection is finished either way

	if err != nil {
		code, msg := consoleRefusal(err)
		if code == consoleCloseFailed {
			s.log.Error("open console", slog.String("app", name), slog.String("error", err.Error()))
		}
		c.end(code, msg)
		return
	}
	c.relay(sess, lim, s.log)
}

// consoleRefusal says why a console could not be opened, in words for the
// person and a code for the script.
func consoleRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, app.ErrConsoleAsleep):
		return consoleCloseRefused, "This app is asleep, so nothing is running to open a " +
			"shell in. Wake it first, then reconnect."
	case errors.Is(err, app.ErrConsoleNoPods):
		return consoleCloseRefused, "This app has no running pods. Deploy it or scale it " +
			"up, then reconnect."
	case errors.Is(err, app.ErrConsoleUnavailable), errors.Is(err, orchestrator.ErrNotSupported):
		return consoleCloseRefused, "A console is not available on this install."
	case errors.Is(err, orchestrator.ErrNotStarted):
		return consoleCloseRefused, "That container is not running yet. Its status on the " +
			"Deployments tab says why; reconnect once it is up."
	case errors.Is(err, app.ErrNotFound), errors.Is(err, orchestrator.ErrNotFound):
		// Most often a pod a deploy has just replaced, which the script
		// answers by reconnecting to whichever pod is running now.
		return consoleCloseNoPod, "That pod is no longer one of this app's — a deploy may " +
			"have replaced it."
	}
	return consoleCloseFailed, "The cluster would not open a shell. The engine's log has the detail."
}

var (
	errConsoleGone = errors.New("console: the browser went away")
	errConsoleIdle = errors.New("console: idle")
	errConsoleMax  = errors.New("console: open too long")
)

// consoleConn is one browser's socket.
//
// Writes are serialised here because the socket allows one writer at a time
// and three things write: the shell's output, the console's own messages, and
// the keepalive. Control frames are the exception the library makes itself.
type consoleConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

// Write sends terminal output, one binary frame per write.
func (c *consoleConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(consoleWriteWait))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *consoleConn) send(m consoleMessage) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(consoleWriteWait))
	_ = c.ws.WriteMessage(websocket.TextMessage, b)
}

// end says why the console is over and closes the socket with code.
func (c *consoleConn) end(code int, msg string) {
	kind := "closed"
	if code != consoleCloseExited && code != consoleCloseIdle && code != consoleCloseMax {
		kind = "error"
	}
	c.send(consoleMessage{Type: kind, Message: msg})
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, ""), time.Now().Add(consoleWriteWait))
}

// relay runs the shell against the socket until one of them ends.
func (c *consoleConn) relay(sess app.ConsoleSession, lim consoleLimits, log *slog.Logger) {
	// Not the request's context. Once the connection is hijacked nothing
	// cancels that when the browser leaves, so what ends a console is decided
	// here: the browser going, the shell exiting, or a limit.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	idle := time.AfterFunc(lim.Idle, func() { cancel(errConsoleIdle) })
	defer idle.Stop()
	deadline := time.AfterFunc(lim.Max, func() { cancel(errConsoleMax) })
	defer deadline.Stop()

	stdin, typed := io.Pipe()
	// Closed once the shell is over, so a keystroke still being handed over
	// fails rather than waiting on a reader that has gone.
	defer stdin.Close() //nolint:errcheck // a pipe's close cannot fail
	// Ended with the console, too, so a shell still reading input sees the end
	// of it rather than waiting on a browser the console has already let go.
	stopInput := context.AfterFunc(ctx, func() { _ = typed.Close() })
	defer stopInput()
	resize := make(chan orchestrator.TerminalSize, 1)

	go c.readInput(ctx, cancel, lim, idle, typed, resize)
	go c.keepAlive(ctx, lim.Ping)

	c.send(consoleMessage{Type: "connected", Pod: sess.Pod(), Pods: sess.Pods()})
	err := sess.Run(ctx, app.Terminal{Stdin: stdin, Stdout: c, Resize: resize})

	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errConsoleGone):
		// Nobody left to tell.
	case errors.Is(cause, errConsoleIdle):
		c.end(consoleCloseIdle, "Closed after "+lim.Idle.String()+" without input. "+
			"Reconnect to open a new shell.")
	case errors.Is(cause, errConsoleMax):
		c.end(consoleCloseMax, "Closed after "+lim.Max.String()+", the longest a console "+
			"stays open. Reconnect to open a new shell.")
	case err != nil:
		code, msg := consoleRefusal(err)
		if code == consoleCloseFailed {
			log.Error("console", slog.String("pod", sess.Pod()), slog.String("error", err.Error()))
		}
		c.end(code, msg)
	default:
		c.end(consoleCloseExited, "The shell exited.")
	}
}

// readInput hands keystrokes to the shell and sizes to the terminal, and is
// what notices the browser has gone.
func (c *consoleConn) readInput(
	ctx context.Context, cancel context.CancelCauseFunc, lim consoleLimits,
	idle *time.Timer, typed *io.PipeWriter, resize chan orchestrator.TerminalSize,
) {
	defer typed.Close() //nolint:errcheck // a pipe's close cannot fail

	// Two missed pings is gone. One can be a slow network.
	alive := func() { _ = c.ws.SetReadDeadline(time.Now().Add(2 * lim.Ping)) }
	c.ws.SetReadLimit(consoleReadLimit)
	alive()
	c.ws.SetPongHandler(func(string) error { alive(); return nil })

	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			cancel(errConsoleGone)
			return
		}
		alive()
		// Only input keeps a console open. Pongs are answered by the
		// browser on its own, so a tab nobody is looking at would otherwise
		// count as somebody using it.
		idle.Reset(lim.Idle)

		switch kind {
		case websocket.BinaryMessage:
			if _, err := typed.Write(data); err != nil {
				return
			}
		case websocket.TextMessage:
			var in consoleInput
			if json.Unmarshal(data, &in) != nil || in.Type != "resize" {
				continue
			}
			if in.Cols < 1 || in.Rows < 1 || in.Cols > 1000 || in.Rows > 1000 {
				continue
			}
			size := orchestrator.TerminalSize{Cols: uint16(in.Cols), Rows: uint16(in.Rows)}
			// Only the latest size matters. One still waiting is replaced
			// rather than queued behind, so dragging a window edge does not
			// replay every size it passed through.
			select {
			case <-resize:
			default:
			}
			select {
			case resize <- size:
			case <-ctx.Done():
				return
			}
		}
	}
}

// keepAlive pings until the console is over.
func (c *consoleConn) keepAlive(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(consoleWriteWait))
		}
	}
}

// consolePods is every pod of the app a console could open in.
//
// Filtered by the app's label as well as its namespace, the same line the
// app service draws: a namespace that held something else would otherwise
// offer it in the picker, and the socket would then refuse it.
func consolePods(d AppDetailData) []string {
	var out []string
	for _, p := range d.Pods {
		if p.App == d.App.Name {
			out = append(out, p.Name)
		}
	}
	return out
}

// consolePod is the pod the tab opens: the one asked for if it is the app's,
// otherwise the first running one — what the socket picks when it is asked for
// none, so the label is right before the script connects. Empty when nothing
// is running.
func consolePod(d AppDetailData, pods []string, asked string) string {
	for _, p := range pods {
		if p == asked {
			return p
		}
	}
	for _, p := range d.Pods {
		if p.App == d.App.Name && p.Phase == "Running" {
			return p.Name
		}
	}
	return ""
}

// kubectlExec is the command an operator runs for the same shell.
func kubectlExec(d AppDetailData, pod string) string {
	return "kubectl exec -it -n " + d.App.Namespace + " " + pod + " -c " + d.App.Name +
		" -- " + strings.Join(orchestrator.ShellCommand[:2], " ") +
		" '" + orchestrator.ShellCommand[2] + "'"
}

// consoleLimitLabel says a limit the way a person would.
func consoleLimitLabel(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	case d >= time.Minute && d%time.Minute == 0:
		return plural(int(d/time.Minute), "minute")
	}
	return d.String()
}

// socketHost is what a host may look like to be written into a policy: a
// name or an address, an optional port, nothing that could end the directive.
var socketHost = regexp.MustCompile(`^(\[[0-9a-f:.]+\]|[a-z0-9.-]+)(:[0-9]{1,5})?$`)

// consoleSocketSource is the dashboard's own origin as a WebSocket source, to
// sit beside 'self' in connect-src — or nothing, where it cannot be named.
//
// Needed because 'self' is not enough everywhere. CSP Level 3 makes it match
// ws: and wss: on the page's host, Chrome and Firefox follow that, and WebKit
// has carried the bug for years: in Safari the console's socket is refused
// while the policy reads as allowing it. Naming the one origin admits exactly
// what 'self' was meant to and nothing more — not a bare wss:, which would let
// a script on the page talk to any host at all.
//
// The origin is the configured one where there is one, as csrfProtect's is.
// Otherwise it is this request's Host, which is written into this response
// only, and checked against socketHost first so it can never carry a second
// directive with it.
func consoleSocketSource(baseURL string, r *http.Request) string {
	origin := originOf(baseURL)
	if origin == "" {
		origin = requestOrigin(r)
	}
	scheme, host, ok := strings.Cut(origin, "://")
	if !ok || !socketHost.MatchString(host) {
		return ""
	}
	switch scheme {
	case "https":
		return " wss://" + host
	case "http":
		return " ws://" + host
	}
	return ""
}
