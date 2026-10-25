package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// consoleApps is fakeApps with a console. What it opens is a fakeShell, or
// the error it was told to refuse with.
type consoleApps struct {
	*fakeApps
	refuse error

	mu     sync.Mutex
	asked  []app.ConsoleRequest
	shells []*fakeShell
	shell  func() *fakeShell
}

func (c *consoleApps) Console(
	_ context.Context, ownerID, appName string, req app.ConsoleRequest,
) (app.ConsoleSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, req)
	if c.refuse != nil {
		return nil, c.refuse
	}
	if _, err := c.fakeApps.Get(context.Background(), ownerID, appName); err != nil {
		return nil, err
	}
	sh := &fakeShell{pod: cmpStr(req.Pod, appName+"-1"), sizes: make(chan orchestrator.TerminalSize, 8)}
	if c.shell != nil {
		sh = c.shell()
	}
	c.shells = append(c.shells, sh)
	return sh, nil
}

func (c *consoleApps) requests() []app.ConsoleRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]app.ConsoleRequest(nil), c.asked...)
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// fakeShell echoes what is typed, reports sizes, and exits when it reads
// "exit\n" — or blocks until the console ends it, when hang is set.
type fakeShell struct {
	pod   string
	sizes chan orchestrator.TerminalSize
	hang  bool
}

func (f *fakeShell) Pod() string    { return f.pod }
func (f *fakeShell) Pods() []string { return []string{f.pod} }

func (f *fakeShell) Run(ctx context.Context, t app.Terminal) error {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-t.Resize:
				f.sizes <- s
			}
		}
	}()
	if f.hang {
		<-ctx.Done()
		return nil
	}
	buf := make([]byte, 1024)
	for {
		n, err := t.Stdin.Read(buf)
		if n > 0 {
			if string(buf[:n]) == "exit\n" {
				return nil
			}
			if _, werr := t.Stdout.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			return nil
		}
	}
}

const consoleTeam = "web-console"

// consoleServer is the whole router, signed in as role, with a console that
// opens fakeShells.
func consoleServer(t *testing.T, role account.Role, apps *consoleApps) *httptest.Server {
	t.Helper()
	if apps == nil {
		apps = &consoleApps{fakeApps: newFakeApps(sampleApp(consoleTeam, "web"))}
	}
	s, err := New(Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            apps,
		Identity:        identity.NewSingleOwner(identity.Owner{ID: consoleTeam}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: consoleTeam, role: role},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: consoleTeam,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// dialConsole opens the socket the way the dashboard's page does: with its
// session cookie and from its own origin, unless told otherwise.
func dialConsole(
	t *testing.T, srv *httptest.Server, path, origin string,
) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", SessionCookie+"=probe-session")
	if origin != "" {
		h.Set("Origin", origin)
	}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, h)
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, resp, err
}

// readMessage reads one message, failing the test if none comes in time.
func readMessage(t *testing.T, c *websocket.Conn) (int, []byte, error) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return c.ReadMessage()
}

func readControl(t *testing.T, c *websocket.Conn) consoleMessage {
	t.Helper()
	kind, data, err := readMessage(t, c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if kind != websocket.TextMessage {
		t.Fatalf("got a binary frame %q, want the console's own message", data)
	}
	var m consoleMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return m
}

// closeCode reads until the socket closes and reports the code it closed with.
func closeCode(t *testing.T, c *websocket.Conn) int {
	t.Helper()
	for {
		if _, _, err := readMessage(t, c); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				return ce.Code
			}
			t.Fatalf("socket ended with %v, want a close frame", err)
		}
	}
}

// ------------------------------------------------------------------ the gate

func TestTheConsoleSocketRelaysATerminal(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp(consoleTeam, "web"))}
	srv := consoleServer(t, account.RoleMember, apps)

	c, resp, err := dialConsole(t, srv, "/apps/web/console/ws?pod=web-7", "https://yacht.test")
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	if m := readControl(t, c); m.Type != "connected" || m.Pod != "web-7" {
		t.Fatalf("first message = %+v, want connected to web-7", m)
	}
	if got := apps.requests(); len(got) != 1 || got[0].Pod != "web-7" {
		t.Fatalf("console asked for %+v, want the pod from the query", got)
	}

	// Keystrokes in, output back, both as raw bytes.
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("ls -la\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	kind, data, err := readMessage(t, c)
	if err != nil || kind != websocket.BinaryMessage || string(data) != "ls -la\r" {
		t.Fatalf("got %d %q %v, want the echoed bytes as a binary frame", kind, data, err)
	}

	// A size, said in JSON, reaches the shell.
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":132,"rows":43}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	select {
	case s := <-apps.shells[0].sizes:
		if s.Cols != 132 || s.Rows != 43 {
			t.Fatalf("size = %+v, want 132x43", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the resize never reached the shell")
	}

	// The shell exiting ends the console normally, and says so.
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	if m := readControl(t, c); m.Type != "closed" || !strings.Contains(m.Message, "exited") {
		t.Fatalf("after exit = %+v, want closed because the shell exited", m)
	}
	if code := closeCode(t, c); code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d, want 1000", code)
	}
}

// The handshake is a GET, which csrfProtect lets through, so the socket checks
// the Origin itself. A page on any other origin — an app on the install under
// the same parent domain, say — must not be able to open a shell with the
// visitor's cookie.
func TestTheConsoleSocketRefusesAnotherOrigin(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp(consoleTeam, "web"))}
	srv := consoleServer(t, account.RoleOwner, apps)

	for _, origin := range []string{"https://evil.yacht.test", "http://yacht.test", "null", ""} {
		_, resp, err := dialConsole(t, srv, "/apps/web/console/ws", origin)
		if err == nil {
			t.Fatalf("origin %q opened a console", origin)
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("origin %q = %v, want 403", origin, resp)
		}
	}
	if n := len(apps.requests()); n != 0 {
		t.Fatalf("%d consoles were resolved for a refused origin", n)
	}
}

// Member is the floor, like every other change a redeploy can undo.
func TestTheConsoleSocketNeedsARole(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp(consoleTeam, "web"))}
	srv := consoleServer(t, account.Role(""), apps)

	_, resp, err := dialConsole(t, srv, "/apps/web/console/ws", "https://yacht.test")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a session with no role = %v, %v; want 403", resp, err)
	}
	if n := len(apps.requests()); n != 0 {
		t.Fatal("a console was resolved for somebody with no role")
	}
}

func TestTheConsoleSocketNeedsASession(t *testing.T) {
	denied := identity.ProviderFunc(func(context.Context, *http.Request) (identity.Owner, error) {
		return identity.Owner{}, identity.ErrUnauthenticated
	})
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp(consoleTeam, "web"))}
	s, err := New(Options{
		Orchestrator: orchestrator.NewNoop(), Apps: apps, Identity: denied,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	_, resp, derr := dialConsole(t, srv, "/apps/web/console/ws", srv.URL)
	if derr == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session = %v, %v; want 401", resp, derr)
	}
}

// Another team's app is somebody else's, socket or not.
func TestTheConsoleSocketIsScopedToTheTeam(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp("someone-else", "web"))}
	srv := consoleServer(t, account.RoleOwner, apps)

	c, _, err := dialConsole(t, srv, "/apps/web/console/ws", "https://yacht.test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if m := readControl(t, c); m.Type != "error" {
		t.Fatalf("another team's app = %+v, want an error", m)
	}
	if code := closeCode(t, c); code != consoleCloseNoPod {
		t.Fatalf("close code = %d, want %d", code, consoleCloseNoPod)
	}
	if len(apps.shells) != 0 {
		t.Fatal("a shell was opened in another team's app")
	}
}

// A refusal is said over the socket, because a browser is told nothing about
// a handshake that failed.
func TestASleepingAppsConsoleSaysToWakeIt(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp(consoleTeam, "web")), refuse: app.ErrConsoleAsleep}
	srv := consoleServer(t, account.RoleMember, apps)

	c, _, err := dialConsole(t, srv, "/apps/web/console/ws", "https://yacht.test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if m := readControl(t, c); m.Type != "error" || !strings.Contains(m.Message, "Wake it first") {
		t.Fatalf("asleep = %+v, want an error saying to wake it", m)
	}
	if code := closeCode(t, c); code != consoleCloseRefused {
		t.Fatalf("close code = %d, want %d", code, consoleCloseRefused)
	}
}

// ------------------------------------------------------------- the limits

// limitedConsole serves the socket with limits short enough to watch.
func limitedConsole(t *testing.T, lim consoleLimits, shell func() *fakeShell) *httptest.Server {
	t.Helper()
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp(consoleTeam, "web")), shell: shell}
	s, err := New(Options{
		Orchestrator: orchestrator.NewNoop(), Apps: apps,
		Identity: identity.NewSingleOwner(identity.Owner{ID: consoleTeam}),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := chi.NewRouter()
	r.Use(identity.Middleware(identity.NewSingleOwner(identity.Owner{ID: consoleTeam})))
	r.Get("/apps/{name}/console/ws", func(w http.ResponseWriter, r *http.Request) {
		s.serveConsole(w, r, lim)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestAnIdleConsoleIsClosed(t *testing.T) {
	srv := limitedConsole(t,
		consoleLimits{Idle: 150 * time.Millisecond, Max: time.Minute, Ping: time.Second},
		func() *fakeShell { return &fakeShell{pod: "web-1", hang: true, sizes: make(chan orchestrator.TerminalSize, 8)} })

	c, _, err := dialConsole(t, srv, "/apps/web/console/ws", srv.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if m := readControl(t, c); m.Type != "connected" {
		t.Fatalf("first message = %+v, want connected", m)
	}
	if m := readControl(t, c); m.Type != "closed" || !strings.Contains(m.Message, "without input") {
		t.Fatalf("after idling = %+v, want closed for idling", m)
	}
	if code := closeCode(t, c); code != consoleCloseIdle {
		t.Fatalf("close code = %d, want %d", code, consoleCloseIdle)
	}
}

// Typing keeps a console open, but not for ever.
func TestABusyConsoleStillEnds(t *testing.T) {
	srv := limitedConsole(t,
		consoleLimits{Idle: time.Minute, Max: 300 * time.Millisecond, Ping: time.Second},
		func() *fakeShell { return &fakeShell{pod: "web-1", sizes: make(chan orchestrator.TerminalSize, 8)} })

	c, _, err := dialConsole(t, srv, "/apps/web/console/ws", srv.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if m := readControl(t, c); m.Type != "connected" {
		t.Fatalf("first message = %+v, want connected", m)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				if c.WriteMessage(websocket.BinaryMessage, []byte("x")) != nil {
					return
				}
			}
		}
	}()

	for {
		kind, data, err := readMessage(t, c)
		if err != nil {
			t.Fatalf("socket ended with %v before saying why", err)
		}
		if kind == websocket.BinaryMessage {
			continue // the echo
		}
		var m consoleMessage
		_ = json.Unmarshal(data, &m)
		if m.Type != "closed" || !strings.Contains(m.Message, "longest") {
			t.Fatalf("after the limit = %+v, want closed for running too long", m)
		}
		break
	}
	if code := closeCode(t, c); code != consoleCloseMax {
		t.Fatalf("close code = %d, want %d", code, consoleCloseMax)
	}
}

// ---------------------------------------------------------------- the tab

// consolePage renders an app's Console tab with its pods applied, so the panel
// has replicas to offer.
func consolePage(t *testing.T, path string, apps Apps, replicas int32, operators ...string) string {
	t.Helper()
	orch := orchestrator.NewNoop()
	a := sampleApp("owner-1", "web")
	if err := orch.ApplyApp(context.Background(), orchestrator.AppSpec{
		Ref: a.Ref(), Image: a.Image, Replicas: replicas, Port: a.Port,
	}); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	h := testServer(t, Options{Orchestrator: orch, Apps: apps, Operators: operators})
	rec := get(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestTheConsoleTabSitsAfterMetrics(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}
	body := consolePage(t, "/apps/web/console", apps, 1)

	// The panel's own tab strip, not the sidebar, which has a Settings too.
	start := strings.Index(body, "canvas-sheet-tabs")
	if start < 0 {
		t.Fatal("no tab strip on the page")
	}
	tabs := body[start:]
	tabs = tabs[:strings.Index(tabs, "</nav>")]
	metrics := strings.Index(tabs, ">Metrics<")
	console := strings.Index(tabs, ">Console<")
	settings := strings.Index(tabs, ">Settings<")
	if metrics < 0 || console < 0 || settings < 0 || !(metrics < console && console < settings) {
		t.Fatalf("tabs at metrics=%d console=%d settings=%d, want Console between Metrics and Settings",
			metrics, console, settings)
	}
	if !strings.Contains(body, `href="/apps/web/console" class="tab tab-active"`) {
		t.Error("the Console tab is not the active one on its own page")
	}
	for _, want := range []string{
		`data-console-url="/apps/web/console/ws"`,
		`data-console-pod="web-0"`,
		"/assets/js/xterm.js", "/assets/js/addon-fit.js", "/assets/js/console.js",
		"/assets/css/xterm.css", "Full screen",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the console tab is missing %q", want)
		}
	}
	// One replica needs no picker.
	if strings.Contains(body, "Choose a replica") {
		t.Error("a single replica was offered a picker")
	}
}

// A terminal emulator is the largest script there is, and no other tab draws
// one.
func TestTheTerminalLoadsOnlyOnTheConsoleTab(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}
	for _, path := range []string{"/apps/web", "/apps/web/metrics", "/apps/web/settings"} {
		if body := consolePage(t, path, apps, 1); strings.Contains(body, "xterm") {
			t.Errorf("GET %s loads xterm", path)
		}
	}
}

func TestSeveralReplicasGetAPicker(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}
	body := consolePage(t, "/apps/web/console?pod=web-1", apps, 3)

	if !strings.Contains(body, "Choose a replica") {
		t.Fatal("three replicas and no picker")
	}
	for _, pod := range []string{"web-0", "web-1", "web-2"} {
		if !strings.Contains(body, `value="`+pod+`"`) {
			t.Errorf("the picker is missing %s", pod)
		}
	}
	if !strings.Contains(body, `data-console-pod="web-1"`) {
		t.Error("the replica asked for is not the one the console opens")
	}

	// A pod that is not the app's is not echoed into the page.
	body = consolePage(t, "/apps/web/console?pod=victim-db-0", apps, 3)
	if strings.Contains(body, "victim-db-0") {
		t.Error("a pod from the query that is not the app's reached the page")
	}
}

func TestTheConsoleTabSaysWhenNothingCanOpen(t *testing.T) {
	asleep := sampleApp("owner-1", "web")
	asleep.Sleep.State = app.SleepAsleep
	cases := []struct {
		why      string
		apps     Apps
		replicas int32
		want     string
	}{
		{"asleep", &consoleApps{fakeApps: newFakeApps(asleep)}, 1, "Wake it first"},
		{"scaled to zero", &consoleApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}, 0, "Nothing running"},
		{"no console on this install", newFakeApps(sampleApp("owner-1", "web")), 1, "No console on this install"},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			body := consolePage(t, "/apps/web/console", tc.apps, tc.replicas)
			if !strings.Contains(body, tc.want) {
				t.Errorf("the tab does not say %q", tc.want)
			}
			if strings.Contains(body, "data-console-url") {
				t.Error("a terminal was drawn where none can open")
			}
		})
	}
}

// The kubectl equivalent is for the people who hold the cluster's
// credentials; to anybody else it is a command that cannot work.
func TestOnlyOperatorsAreShownKubectl(t *testing.T) {
	apps := &consoleApps{fakeApps: newFakeApps(sampleApp("owner-1", "web"))}

	// Without accounts the single owner runs the install.
	body := consolePage(t, "/apps/web/console", apps, 1)
	ns := app.Namespace("owner-1", "web")
	if !strings.Contains(body, "kubectl exec -it -n "+ns+" web-0 -c web") {
		t.Errorf("the operator is not shown the kubectl command")
	}

	srv := consoleServer(t, account.RoleMember, nil)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/apps/web/console", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "probe-session"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member GET = %d", resp.StatusCode)
	}
	if strings.Contains(string(b), "kubectl") {
		t.Error("a member who runs nothing on the cluster was shown kubectl")
	}
}

// Safari does not read 'self' as covering a same-host socket, so the
// dashboard's own ws origin is named — and nothing wider.
func TestThePolicyAdmitsTheConsoleSocket(t *testing.T) {
	h := testServer(t, Options{})
	csp := get(t, h, "/healthz").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' ws://example.com;") {
		t.Errorf("connect-src does not name the socket's origin: %s", csp)
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := consoleSocketSource("https://dash.example.com", r); got != " wss://dash.example.com" {
		t.Errorf("configured https origin = %q, want wss", got)
	}
	r.Host = "evil.test;script-src *"
	if got := consoleSocketSource("", r); got != "" {
		t.Errorf("a Host carrying a directive was written into the policy: %q", got)
	}
}
