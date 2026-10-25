package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// login signs this machine in: through the browser, which is the default and
// needs nothing copied, or with a token pasted or passed, for a machine with
// no browser.
func (r *runner) login(ctx context.Context, args []string) error {
	fs := r.flags("login")
	host := fs.String("host", "", "the dashboard's address")
	token := fs.String("token", "", "a token to sign in with instead of the browser")
	paste := fs.Bool("paste", false, "paste a token rather than signing in through the browser")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	h := r.host(*host)
	if h == "" {
		v, err := r.prompt("Dashboard address (https://…): ")
		if err != nil {
			return err
		}
		h = r.host(v)
	}
	if h == "" {
		return usageErr{"pass --host with the dashboard's address"}
	}

	raw := strings.TrimSpace(*token)
	if raw == "" && *paste {
		v, err := r.readSecret("Paste a token: ")
		if err != nil {
			return err
		}
		raw = v
	}
	if raw == "" {
		v, err := r.browserLogin(ctx, h)
		if err != nil {
			return err
		}
		raw = v
	}

	var me meView
	if err := r.clientWith(h, raw).getJSON(ctx, "/api/v1/me", &me); err != nil {
		return fmt.Errorf("that token does not work at %s: %w", h, err)
	}
	if err := r.saveUser(userConfig{Host: h, Token: raw, Team: me.Team.Name}); err != nil {
		return fmt.Errorf("signed in, but the token could not be kept: %w", err)
	}
	fmt.Fprintf(r.out, "Signed in to %s as %s of %s.\n", h, me.Role, me.Team.Name)
	return nil
}

// browserLogin is the device flow: ask for a login, show its code, open the
// browser at the page that approves it, and wait for the approval.
func (r *runner) browserLogin(ctx context.Context, host string) (string, error) {
	c := r.clientWith(host, "")
	hostname, _ := os.Hostname()
	var start struct {
		DeviceCode string    `json:"device_code"`
		UserCode   string    `json:"user_code"`
		URL        string    `json:"verification_url"`
		ExpiresAt  time.Time `json:"expires_at"`
		Interval   int       `json:"interval"`
	}
	if err := c.postJSON(ctx, "/api/v1/cli/login", map[string]string{"client": hostname}, &start); err != nil {
		return "", fmt.Errorf("could not start signing in: %w", err)
	}
	fmt.Fprintf(r.out, "Your code is %s\n", start.UserCode)
	fmt.Fprintf(r.out, "Opening %s — check the code there matches this one, then approve.\n", start.URL)
	if err := r.openURL(start.URL); err != nil {
		fmt.Fprintf(r.out, "Could not open a browser; open the address above yourself.\n")
	}
	interval := r.env.PollInterval
	if start.Interval > 0 && r.env.PollInterval == 2*time.Second {
		interval = time.Duration(start.Interval) * time.Second
	}
	deadline := start.ExpiresAt
	if deadline.IsZero() {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		var poll struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}
		err := c.postJSON(ctx, "/api/v1/cli/login/poll", map[string]string{"device_code": start.DeviceCode}, &poll)
		if err != nil {
			return "", err
		}
		if poll.Status == "approved" && poll.Token != "" {
			return poll.Token, nil
		}
	}
	return "", errors.New("the login expired before it was approved — run login again")
}

// readSecret reads a line without echoing it where there is a terminal.
func (r *runner) readSecret(q string) (string, error) {
	if f, ok := r.env.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(r.out, q)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(r.out)
		return strings.TrimSpace(string(b)), err
	}
	return r.prompt(q)
}

func (r *runner) logout() error {
	c := r.loadUser()
	if c.Token == "" {
		fmt.Fprintln(r.out, "Not signed in.")
		return nil
	}
	c.Token, c.Team = "", ""
	if err := r.saveUser(c); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "Signed out on this machine. The token still works until it is revoked on the Team page.")
	return nil
}

func (r *runner) whoami(ctx context.Context) error {
	c, err := r.client("")
	if err != nil {
		return err
	}
	var me meView
	if err := c.getJSON(ctx, "/api/v1/me", &me); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "%s of %s at %s\n", me.Role, me.Team.Name, c.host)
	if me.Token != nil {
		fmt.Fprintf(r.out, "token %q, made by %s\n", me.Token.Name, me.Token.CreatedBy)
	}
	if me.Project != nil {
		fmt.Fprintf(r.out, "reaches project %s only\n", me.Project.Slug)
	}
	return nil
}

// link writes which app this directory deploys to.
func (r *runner) link(ctx context.Context, args []string) error {
	fs := r.flags("link")
	project := fs.String("project", "", "the project, by slug")
	appFlag := fs.String("app", "", "the app, by name")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := r.client("")
	if err != nil {
		return err
	}
	var projects struct {
		Projects []projectView `json:"projects"`
	}
	if err := c.getJSON(ctx, "/api/v1/projects", &projects); err != nil {
		return err
	}
	var chosen projectView
	if *project != "" {
		for _, p := range projects.Projects {
			if p.Slug == *project || p.Name == *project {
				chosen = p
			}
		}
		if chosen.Slug == "" {
			return fmt.Errorf("no project %q", *project)
		}
	} else {
		names := make([]string, len(projects.Projects))
		for i, p := range projects.Projects {
			names[i] = p.Name + " (" + p.Slug + ")"
		}
		i, err := r.choose("project", names)
		if err != nil {
			return err
		}
		chosen = projects.Projects[i]
	}
	var apps struct {
		Apps []appView `json:"apps"`
	}
	if err := c.getJSON(ctx, "/api/v1/projects/"+url.PathEscape(chosen.Slug)+"/apps", &apps); err != nil {
		return err
	}
	name := *appFlag
	if name == "" {
		names := make([]string, len(apps.Apps))
		for i, a := range apps.Apps {
			names[i] = a.Name
		}
		i, err := r.choose("app", names)
		if err != nil {
			return err
		}
		name = apps.Apps[i].Name
	} else {
		found := false
		for _, a := range apps.Apps {
			found = found || a.Name == name
		}
		if !found {
			return fmt.Errorf("no app %q in %s", name, chosen.Name)
		}
	}
	dir := filepath.Join(r.env.Dir, r.brand.LinkDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(linkConfig{Host: c.host, Project: chosen.Slug, App: name}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Linked %s to %s in %s. `%s up` deploys this directory to it.\n",
		r.env.Dir, name, chosen.Name, r.brand.Name)
	return nil
}

func (r *runner) unlink() error {
	_, dir, ok := r.findLink()
	if !ok {
		fmt.Fprintln(r.out, "Nothing is linked here.")
		return nil
	}
	if err := os.RemoveAll(filepath.Join(dir, r.brand.LinkDir)); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Unlinked %s.\n", dir)
	return nil
}

// up packs the directory, uploads it, and follows the build and the deploy to
// the end — Railway's `railway up`.
func (r *runner) up(ctx context.Context, args []string) error {
	fs := r.flags("up")
	appFlag := fs.String("app", "", "the app to deploy to")
	detach := fs.Bool("detach", false, "upload and return, without following the build")
	fs.BoolVar(detach, "d", false, "shorthand for --detach")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	name, err := r.appName(*appFlag)
	if err != nil {
		return err
	}
	c, err := r.client("")
	if err != nil {
		return err
	}
	dir := r.env.Dir
	if len(rest) > 0 {
		dir = rest[0]
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(r.env.Dir, dir)
		}
	}
	fmt.Fprintf(r.out, "Packing %s\n", dir)
	archive, err := pack(dir, r.brand.LinkDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Uploading %d files (%s, %s compressed)\n",
		archive.Files, humanSize(archive.Expanded), humanSize(int64(len(archive.Data))))

	var res struct {
		Deployment deploymentView `json:"deployment"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(name)+"/up",
		bytes.NewReader(archive.Data), "application/gzip", &res); err != nil {
		return err
	}
	dep := res.Deployment
	fmt.Fprintf(r.out, "Deploying %s — %s (%s)\n", name, dep.Title, dep.ID)
	if *detach {
		fmt.Fprintf(r.out, "Follow it with `%s logs --build`.\n", r.brand.Name)
		return nil
	}
	return r.follow(ctx, c, name, dep.ID)
}

// follow prints a deploy's build log as it is written and then waits for the
// deploy to finish, failing with its reason if it did not succeed.
func (r *runner) follow(ctx context.Context, c *client, name, id string) error {
	base := "/api/v1/apps/" + url.PathEscape(name) + "/deployments/" + url.PathEscape(id)
	resp, err := c.send(ctx, http.MethodGet, base+"/build-log?follow=1", nil, "")
	if err == nil {
		if resp.StatusCode == http.StatusOK {
			_, _ = io.Copy(r.out, resp.Body)
		}
		resp.Body.Close()
	}
	for {
		var d deploymentView
		if err := c.getJSON(ctx, base, &d); err != nil {
			return err
		}
		if d.FinishedAt != nil {
			if d.Status == "succeeded" || d.Status == "active" {
				fmt.Fprintf(r.out, "Deployed %s.\n", name)
				return nil
			}
			msg := d.Message
			if msg == "" {
				msg = d.Status
			}
			return fmt.Errorf("the deploy %s: %s", strings.ToLower(d.Badge), msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.env.PollInterval):
		}
	}
}

func (r *runner) redeploy(ctx context.Context, args []string) error {
	fs := r.flags("redeploy")
	appFlag := fs.String("app", "", "the app")
	detach := fs.Bool("detach", false, "return without following the deploy")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	name, err := r.appName(*appFlag)
	if err != nil {
		return err
	}
	c, err := r.client("")
	if err != nil {
		return err
	}
	var res struct {
		Deployment *deploymentView `json:"deployment"`
	}
	if err := c.postJSON(ctx, "/api/v1/apps/"+url.PathEscape(name)+"/redeploy", nil, &res); err != nil {
		return err
	}
	if res.Deployment == nil || *detach {
		fmt.Fprintf(r.out, "Redeploying %s.\n", name)
		return nil
	}
	fmt.Fprintf(r.out, "Redeploying %s (%s)\n", name, res.Deployment.ID)
	return r.follow(ctx, c, name, res.Deployment.ID)
}

// logs prints an app's output, following it with -f, or a deploy's build log
// with --build.
func (r *runner) logs(ctx context.Context, args []string) error {
	fs := r.flags("logs")
	appFlag := fs.String("app", "", "the app")
	follow := fs.Bool("follow", false, "keep printing as it is written")
	fs.BoolVar(follow, "f", false, "shorthand for --follow")
	tail := fs.Int("tail", 200, "how many lines to print")
	build := fs.Bool("build", false, "the latest deploy's build log rather than the app's output")
	deploy := fs.String("deployment", "", "with --build, a deployment other than the latest")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	name, err := r.appName(*appFlag)
	if err != nil {
		return err
	}
	c, err := r.client("")
	if err != nil {
		return err
	}
	if *build {
		id := *deploy
		if id == "" {
			var a appView
			if err := c.getJSON(ctx, "/api/v1/apps/"+url.PathEscape(name), &a); err != nil {
				return err
			}
			if a.Latest == nil {
				return errors.New("the app has not been deployed")
			}
			id = a.Latest.ID
		}
		q := "/api/v1/apps/" + url.PathEscape(name) + "/deployments/" + url.PathEscape(id) + "/build-log"
		if *follow {
			q += "?follow=1"
		}
		resp, err := c.send(ctx, http.MethodGet, q, nil, "")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return readAPIErr(resp)
		}
		_, err = io.Copy(r.out, resp.Body)
		return err
	}
	q := "/api/v1/apps/" + url.PathEscape(name) + "/logs?tail=" + strconv.Itoa(*tail)
	if *follow {
		q += "&follow=1"
	}
	resp, err := c.send(ctx, http.MethodGet, q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readAPIErr(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var line struct {
			At   time.Time `json:"at"`
			Text string    `json:"text"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		fmt.Fprintf(r.out, "%s  %s\n", line.At.Local().Format("15:04:05"), line.Text)
	}
	return sc.Err()
}

// variables lists, sets and deletes an app's variables.
func (r *runner) variables(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := r.flags("variables " + sub)
	appFlag := fs.String("app", "", "the app")
	secret := fs.Bool("secret", false, "with set, keep the values secret")
	reveal := fs.Bool("reveal", false, "with list, show secrets' values (an admin's token only)")
	asJSON := fs.Bool("json", false, "with list, print JSON")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	name, err := r.appName(*appFlag)
	if err != nil {
		return err
	}
	c, err := r.client("")
	if err != nil {
		return err
	}
	base := "/api/v1/apps/" + url.PathEscape(name) + "/variables"
	switch sub {
	case "list", "ls":
		q := base
		if *reveal {
			q += "?reveal=1"
		}
		var res struct {
			Variables []variableView `json:"variables"`
		}
		if err := c.getJSON(ctx, q, &res); err != nil {
			return err
		}
		sort.Slice(res.Variables, func(i, j int) bool { return res.Variables[i].Key < res.Variables[j].Key })
		if *asJSON {
			out := map[string]string{}
			for _, v := range res.Variables {
				out[v.Key] = v.Value
			}
			raw, _ := json.MarshalIndent(out, "", "  ")
			fmt.Fprintln(r.out, string(raw))
			return nil
		}
		if len(res.Variables) == 0 {
			fmt.Fprintln(r.out, "No variables.")
		}
		for _, v := range res.Variables {
			value := v.Value
			if v.Masked {
				value = "******** (secret)"
			}
			staged := ""
			if v.Staged != "" {
				staged = "  [" + v.Staged + ", not deployed]"
			}
			fmt.Fprintf(r.out, "%s=%s%s\n", v.Key, value, staged)
		}
		return nil
	case "set":
		if len(rest) == 0 {
			return usageErr{"give KEY=VALUE"}
		}
		for _, kv := range rest {
			key, value, ok := strings.Cut(kv, "=")
			if !ok || key == "" {
				return usageErr{fmt.Sprintf("%q is not KEY=VALUE", kv)}
			}
			raw, _ := json.Marshal(map[string]any{"value": value, "secret": *secret})
			var res struct {
				Staged bool `json:"staged"`
			}
			if err := c.do(ctx, http.MethodPut, base+"/"+url.PathEscape(key), bytes.NewReader(raw), "application/json", &res); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			if res.Staged {
				fmt.Fprintf(r.out, "Set %s — staged; deploy to apply it.\n", key)
			} else {
				fmt.Fprintf(r.out, "Set %s.\n", key)
			}
		}
		return nil
	case "delete", "rm", "unset":
		if len(rest) == 0 {
			return usageErr{"give the KEY to delete"}
		}
		for _, key := range rest {
			if err := c.do(ctx, http.MethodDelete, base+"/"+url.PathEscape(key), nil, "", nil); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			fmt.Fprintf(r.out, "Deleted %s.\n", key)
		}
		return nil
	}
	return usageErr{fmt.Sprintf("unknown variables command %q — list, set or delete", sub)}
}

// status says how the app is and how its latest deploy went.
func (r *runner) status(ctx context.Context, args []string) error {
	fs := r.flags("status")
	appFlag := fs.String("app", "", "the app")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	name, err := r.appName(*appFlag)
	if err != nil {
		return err
	}
	c, err := r.client("")
	if err != nil {
		return err
	}
	var a appView
	if err := c.getJSON(ctx, "/api/v1/apps/"+url.PathEscape(name), &a); err != nil {
		return err
	}
	phase := a.Status.Phase
	if !a.Status.Known {
		phase = "unknown — the cluster did not answer"
	}
	fmt.Fprintf(r.out, "%s  %s (%d/%d ready)\n", a.Name, phase, a.Status.Ready, a.Status.Desired)
	if a.URL != "" {
		fmt.Fprintf(r.out, "  %s\n", a.URL)
	}
	if d := a.Latest; d != nil {
		fmt.Fprintf(r.out, "  latest: %s — %s, %s\n", d.Title, d.Badge, d.StartedAt.Local().Format("2 Jan 15:04"))
		if d.Message != "" && d.Badge == "FAILED" {
			fmt.Fprintf(r.out, "  %s\n", d.Message)
		}
	}
	return nil
}

// open opens the app on the dashboard.
func (r *runner) open(ctx context.Context, args []string) error {
	fs := r.flags("open")
	appFlag := fs.String("app", "", "the app")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	name, err := r.appName(*appFlag)
	if err != nil {
		return err
	}
	c, err := r.client("")
	if err != nil {
		return err
	}
	var a appView
	if err := c.getJSON(ctx, "/api/v1/apps/"+url.PathEscape(name), &a); err != nil {
		return err
	}
	u := a.Dashboard
	if u == "" {
		u = c.host + "/apps/" + url.PathEscape(name)
	}
	fmt.Fprintln(r.out, u)
	return r.openURL(u)
}
