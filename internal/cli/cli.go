// Package cli is the command line for the engine's API: sign in, link a
// directory to an app, and `up` — tar the directory, upload it, and build and
// deploy it — with logs, variables and status beside it. Railway's CLI, for
// whatever product the engine is shipped as.
//
// Nothing here is the engine's name. Brand carries what the binary is called,
// the product it talks to, the host it talks to by default and where it keeps
// its files, and a wrapper builds the same code under its own; cmd/yacht-cli
// sets them from the linker, so `go build -ldflags "-X main.name=datahuru"`
// is the whole of the difference.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Brand is what the CLI is called and who it talks to.
type Brand struct {
	// Name is the binary: what help text tells people to type.
	Name string
	// Product is what the platform is called in sentences.
	Product string
	// DefaultHost is the dashboard's URL, used until somebody logs in to
	// another. Empty makes login ask for one.
	DefaultHost string
	// LinkDir is the directory `link` writes the project and app into,
	// beside the code: ".datahuru" holds config.json.
	LinkDir string
	// EnvPrefix begins the variables the CLI reads: <PREFIX>_TOKEN for a
	// token in CI, <PREFIX>_HOST for the install, <PREFIX>_APP for the app.
	EnvPrefix string
	Version   string
}

// withDefaults fills what a Brand left empty from its Name.
func (b Brand) withDefaults() Brand {
	if b.Name == "" {
		b.Name = "yacht"
	}
	if b.Product == "" {
		b.Product = strings.ToUpper(b.Name[:1]) + b.Name[1:]
	}
	if b.LinkDir == "" {
		b.LinkDir = "." + b.Name
	}
	if b.EnvPrefix == "" {
		b.EnvPrefix = strings.ToUpper(strings.ReplaceAll(b.Name, "-", "_"))
	}
	if b.Version == "" {
		b.Version = "dev"
	}
	return b
}

// Env is everything the CLI touches outside itself, so a test can run it
// against a server of its own with nothing of the machine's leaking in.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads a variable; nil is os.Getenv.
	Getenv func(string) string
	// ConfigDir is where the signed-in token is kept; empty is the user's
	// config directory.
	ConfigDir string
	// Dir is the directory commands run in; empty is the working directory.
	Dir string
	// OpenBrowser opens a URL; nil tries the platform's own opener.
	OpenBrowser func(string) error
	// HTTP sends requests; nil is a client with a sensible timeout.
	HTTP *http.Client
	// PollInterval overrides how often login and up look for news.
	PollInterval time.Duration
}

// Main runs the CLI against the real machine and exits with its status.
func Main(b Brand) {
	os.Exit(Run(context.Background(), b, os.Args[1:], Env{}))
}

// Run runs one command and returns its exit status.
func Run(ctx context.Context, b Brand, args []string, env Env) int {
	b = b.withDefaults()
	r := &runner{brand: b, env: env}
	r.fill()
	if len(args) == 0 {
		r.usage()
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "login":
		err = r.login(ctx, rest)
	case "logout":
		err = r.logout()
	case "whoami":
		err = r.whoami(ctx)
	case "link":
		err = r.link(ctx, rest)
	case "unlink":
		err = r.unlink()
	case "up":
		err = r.up(ctx, rest)
	case "redeploy":
		err = r.redeploy(ctx, rest)
	case "logs":
		err = r.logs(ctx, rest)
	case "variables", "vars", "variable":
		err = r.variables(ctx, rest)
	case "status":
		err = r.status(ctx, rest)
	case "open":
		err = r.open(ctx, rest)
	case "version", "--version", "-v":
		fmt.Fprintf(r.out, "%s %s\n", b.Name, b.Version)
	case "help", "--help", "-h":
		r.usage()
	default:
		fmt.Fprintf(r.errOut, "%s: unknown command %q\n\n", b.Name, cmd)
		r.usage()
		return 2
	}
	if err != nil {
		var ue usageErr
		if errors.As(err, &ue) {
			fmt.Fprintf(r.errOut, "%s %s: %s\n", b.Name, cmd, ue.msg)
			return 2
		}
		fmt.Fprintf(r.errOut, "%s: %s\n", b.Name, err)
		return 1
	}
	return 0
}

type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

type runner struct {
	brand  Brand
	env    Env
	in     *bufio.Reader
	out    io.Writer
	errOut io.Writer
}

func (r *runner) fill() {
	if r.env.Stdin == nil {
		r.env.Stdin = os.Stdin
	}
	if r.env.Stdout == nil {
		r.env.Stdout = os.Stdout
	}
	if r.env.Stderr == nil {
		r.env.Stderr = os.Stderr
	}
	if r.env.Getenv == nil {
		r.env.Getenv = os.Getenv
	}
	if r.env.Dir == "" {
		r.env.Dir, _ = os.Getwd()
	}
	if r.env.ConfigDir == "" {
		if d, err := os.UserConfigDir(); err == nil {
			r.env.ConfigDir = filepath.Join(d, r.brand.Name)
		}
	}
	if r.env.HTTP == nil {
		// No overall timeout: logs follow for as long as somebody watches.
		// Connecting is bounded by the transport.
		r.env.HTTP = &http.Client{}
	}
	if r.env.PollInterval <= 0 {
		r.env.PollInterval = 2 * time.Second
	}
	r.in = bufio.NewReader(r.env.Stdin)
	r.out, r.errOut = r.env.Stdout, r.env.Stderr
}

func (r *runner) usage() {
	n := r.brand.Name
	fmt.Fprintf(r.out, `%[1]s — deploy to %[2]s from your terminal

Usage:
  %[1]s login [--host URL] [--token TOKEN]   sign in, through the browser or with a token
  %[1]s logout                               forget the token on this machine
  %[1]s whoami                               which team and role you are signed in as
  %[1]s link [--project SLUG] [--app NAME]   link this directory to an app
  %[1]s unlink                               forget the link
  %[1]s up [--app NAME] [--detach]            upload this directory, build it and deploy it
  %[1]s redeploy [--app NAME]                 deploy the app again
  %[1]s logs [--app NAME] [-f] [--tail N]     the app's output
  %[1]s logs --build [--app NAME]             the latest deploy's build log
  %[1]s variables [--app NAME] [--reveal]     list the app's variables
  %[1]s variables set KEY=VALUE... [--secret]
  %[1]s variables delete KEY...
  %[1]s status [--app NAME]                   the app and its latest deploy
  %[1]s open [--app NAME]                     the app on the dashboard

In CI, set %[3]s_TOKEN to a token from Team › API tokens or a project's settings,
and %[3]s_HOST to the dashboard's address.
`, n, r.brand.Product, r.brand.EnvPrefix)
}

// userConfig is what login keeps: the host and the token. The file is the
// user's alone, as an SSH key is.
type userConfig struct {
	Host  string `json:"host"`
	Token string `json:"token"`
	Team  string `json:"team,omitempty"`
}

func (r *runner) userConfigPath() string { return filepath.Join(r.env.ConfigDir, "config.json") }

func (r *runner) loadUser() userConfig {
	var c userConfig
	raw, err := os.ReadFile(r.userConfigPath())
	if err == nil {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

func (r *runner) saveUser(c userConfig) error {
	if r.env.ConfigDir == "" {
		return errors.New("there is no config directory to keep the token in")
	}
	if err := os.MkdirAll(r.env.ConfigDir, 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	// Written to a temporary file and renamed, so a half-written file never
	// replaces a good one; 0600 because it holds a token.
	tmp := r.userConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.userConfigPath())
}

// linkConfig is what link writes beside the code. No token: the directory
// may be committed, and a token in it would be a token in the repository.
type linkConfig struct {
	Host    string `json:"host,omitempty"`
	Project string `json:"project,omitempty"`
	App     string `json:"app"`
}

// findLink looks for the link directory here and in every parent, so a
// command run from a subdirectory finds the app its project is linked to.
func (r *runner) findLink() (linkConfig, string, bool) {
	dir := r.env.Dir
	for {
		p := filepath.Join(dir, r.brand.LinkDir, "config.json")
		if raw, err := os.ReadFile(p); err == nil {
			var c linkConfig
			if json.Unmarshal(raw, &c) == nil && c.App != "" {
				return c, dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return linkConfig{}, "", false
		}
		dir = parent
	}
}

// host is the install to talk to: a flag, then the environment, then the
// link, then what login kept, then the brand's own.
func (r *runner) host(flagHost string) string {
	for _, h := range []string{
		flagHost, r.env.Getenv(r.brand.EnvPrefix + "_HOST"), r.linkHost(), r.loadUser().Host, r.brand.DefaultHost,
	} {
		if h = strings.TrimRight(strings.TrimSpace(h), "/"); h != "" {
			if !strings.Contains(h, "://") {
				h = "https://" + h
			}
			return h
		}
	}
	return ""
}

func (r *runner) linkHost() string {
	if c, _, ok := r.findLink(); ok {
		return c.Host
	}
	return ""
}

// client is an API client with the token: the environment's, for CI, or the
// one login kept.
func (r *runner) client(flagHost string) (*client, error) {
	host := r.host(flagHost)
	if host == "" {
		return nil, fmt.Errorf("no host — run `%s login --host https://…`", r.brand.Name)
	}
	token := strings.TrimSpace(r.env.Getenv(r.brand.EnvPrefix + "_TOKEN"))
	if token == "" {
		token = r.loadUser().Token
	}
	if token == "" {
		return nil, fmt.Errorf("not signed in — run `%s login`, or set %s_TOKEN", r.brand.Name, r.brand.EnvPrefix)
	}
	return r.clientWith(host, token), nil
}

func (r *runner) clientWith(host, token string) *client {
	return &client{
		host: host, token: token, http: r.env.HTTP,
		agent: r.brand.Name + "/" + r.brand.Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")",
	}
}

// appName is the app a command acts on: a flag, the environment, or the link.
func (r *runner) appName(flagApp string) (string, error) {
	if flagApp != "" {
		return flagApp, nil
	}
	if a := strings.TrimSpace(r.env.Getenv(r.brand.EnvPrefix + "_APP")); a != "" {
		return a, nil
	}
	if c, _, ok := r.findLink(); ok {
		return c.App, nil
	}
	return "", fmt.Errorf("no app — run `%s link` here, or pass --app", r.brand.Name)
}

// flags is a flag set that reports to the command's error stream and lets
// flags come after arguments, the way people type them.
func (r *runner) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(r.brand.Name+" "+name, flag.ContinueOnError)
	fs.SetOutput(r.errOut)
	return fs
}

// parse parses flags anywhere in args, returning the rest in order.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			return rest, nil
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
}

// prompt asks a question and reads a line.
func (r *runner) prompt(q string) (string, error) {
	fmt.Fprint(r.out, q)
	line, err := r.in.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no answer — pass the choice as a flag instead")
	}
	return strings.TrimSpace(line), nil
}

// choose asks somebody to pick one of a list by number, or by name.
func (r *runner) choose(what string, names []string) (int, error) {
	if len(names) == 0 {
		return 0, fmt.Errorf("there is no %s to choose", what)
	}
	if len(names) == 1 {
		fmt.Fprintf(r.out, "Using %s %s.\n", what, names[0])
		return 0, nil
	}
	for i, n := range names {
		fmt.Fprintf(r.out, "  %d) %s\n", i+1, n)
	}
	ans, err := r.prompt(fmt.Sprintf("Choose a %s [1-%d]: ", what, len(names)))
	if err != nil {
		return 0, err
	}
	if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(names) {
		return n - 1, nil
	}
	for i, n := range names {
		if n == ans {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%q is not one of them", ans)
}

func (r *runner) openURL(u string) error {
	if r.env.OpenBrowser != nil {
		return r.env.OpenBrowser(u)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
