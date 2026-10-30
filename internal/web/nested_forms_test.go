package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// nestedForms finds forms inside forms. A browser ignores the inner start
// tag and lets its end tag close the outer form, so every field after it
// belongs to no form: it is drawn, and never sent.
func nestedForms(t *testing.T, page string) []string {
	t.Helper()
	z := html.NewTokenizer(strings.NewReader(page))
	var open []string
	var nested []string
	for {
		switch z.Next() {
		case html.ErrorToken:
			return nested
		case html.StartTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "form" {
				continue
			}
			action := ""
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if string(k) == "action" {
					action = string(v)
				}
			}
			if len(open) > 0 {
				nested = append(nested, action+" inside "+open[len(open)-1])
			}
			open = append(open, action)
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "form" && len(open) > 0 {
				open = open[:len(open)-1]
			}
		}
	}
}

// A Git app's settings, with a repository connected and so the source
// card's Disconnect, are one form whose fields all reach it.
func TestTheSettingsPageHasNoFormInsideAForm(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.Source = app.SourceGit
	a.Repo = app.Repo{URL: "https://github.com/acme/web.git", Branch: "main"}
	h := testServer(t, Options{Apps: newFakeApps(a)})

	body := get(t, h, "/apps/web/settings").Body.String()
	if nested := nestedForms(t, body); len(nested) > 0 {
		t.Errorf("forms inside forms: %v", nested)
	}
	for _, want := range []string{`form="source-disconnect"`, `id="source-disconnect"`, `name="port"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page does not have %q", want)
		}
	}
}

// Saving settings says what happened: a change that reaches the cluster is
// rolling out, and a save that changed nothing says so and stays put.
func TestSavingSettingsSaysWhatHappened(t *testing.T) {
	a := sampleApp("owner-1", "web")
	h := testServer(t, Options{Apps: newFakeApps(a)})
	form := url.Values{"image": {a.Image}, "port": {"8080"}}

	rec := post(t, h, "/apps/web/runtime", form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/web/settings" {
		t.Errorf("a save that changed nothing = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	form.Set("port", "4000")
	rec = post(t, h, "/apps/web/runtime", form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/web" {
		t.Errorf("a port change = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
}

// The Deploy section's two commands are fields of the settings form: shown as
// saved, sent with the rest, and a change to either rolls the app out.
func TestTheSettingsFormSavesTheDeployCommands(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.PreDeployCommand, a.StartCommand = "bin/migrate", "bin/web start"
	apps := newFakeApps(a)
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps/web/settings").Body.String()
	for _, want := range []string{
		`name="pre_deploy_command"`, `value="bin/migrate"`,
		`name="start_command"`, `value="bin/web start"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page does not have %q", want)
		}
	}

	form := url.Values{
		"image": {a.Image}, "port": {"8080"},
		"pre_deploy_command": {"bin/app eval 'App.Release.migrate()'"},
		"start_command":      {"bin/app start"},
	}
	rec := post(t, h, "/apps/web/runtime", form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/web" {
		t.Errorf("a command change = %d to %q, want it rolling out", rec.Code, rec.Header().Get("Location"))
	}
	if len(apps.updated) != 1 {
		t.Fatalf("updates = %d, want 1", len(apps.updated))
	}
	got := apps.updated[0]
	if got.PreDeployCommand != "bin/app eval 'App.Release.migrate()'" || got.StartCommand != "bin/app start" {
		t.Errorf("saved pre-deploy %q and start %q", got.PreDeployCommand, got.StartCommand)
	}
}

// The rest of Deploy, and Build for a Git app, save with the form: a
// schedule, a restart policy and its retries, a healthcheck timeout, a
// Dockerfile path and watch paths.
func TestTheSettingsFormSavesTheDeployAndBuildSettings(t *testing.T) {
	a := sampleApp("owner-1", "web")
	a.Source = app.SourceGit
	a.Repo = app.Repo{URL: "https://github.com/acme/web.git", Branch: "main"}
	a.CronSchedule, a.RestartPolicy, a.RestartMaxRetries = "0 3 * * *", orchestrator.RestartNever, 0
	a.WatchPaths = []string{"/backend/**", "!/backend/**/*.md"}
	apps := newFakeApps(a)
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps/web/settings").Body.String()
	for _, want := range []string{
		`id="build"`, `name="dockerfile_path"`, `name="watch_paths"`,
		`name="cron_schedule"`, `value="0 3 * * *"`, `name="restart_policy"`,
		`name="restart_max_retries"`, `name="health_timeout"`,
		"/backend/**\n!/backend/**/*.md</textarea>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page does not have %q", want)
		}
	}

	form := url.Values{
		"port": {"8080"}, "repo_url": {a.Repo.URL}, "repo_branch": {"main"},
		"cron_schedule": {"*/30 * * * *"}, "restart_policy": {"on_failure"},
		"restart_max_retries": {"5"}, "health_timeout": {"120"},
		"dockerfile_path": {"docker/web.Dockerfile"}, "watch_paths": {"/web/**"},
	}
	post(t, h, "/apps/web/runtime", form)
	if len(apps.updated) != 1 {
		t.Fatalf("updates = %d, want 1", len(apps.updated))
	}
	got := apps.updated[0]
	if got.CronSchedule != "*/30 * * * *" || got.RestartPolicy != orchestrator.RestartOnFailure ||
		got.RestartMaxRetries != 5 || got.HealthTimeoutSeconds != 120 ||
		got.DockerfilePath != "docker/web.Dockerfile" || len(got.WatchPaths) != 1 || got.WatchPaths[0] != "/web/**" {
		t.Errorf("saved %+v", got)
	}

	// The policy's picker is filled by script; a form sent without it keeps
	// the policy rather than resetting it.
	form.Del("restart_policy")
	post(t, h, "/apps/web/runtime", form)
	if got := apps.updated[1].RestartPolicy; got != orchestrator.RestartOnFailure {
		t.Errorf("policy without the field = %q, want the app's kept", got)
	}
}

func TestACronAppSaysWhenItNextRuns(t *testing.T) {
	now := time.Date(2026, 10, 25, 2, 50, 0, 0, time.UTC)
	st := orchestrator.AppStatus{Phase: orchestrator.PhaseScheduled, Schedule: "0 3 * * *"}
	if got, want := cronStatusAt(st, now), "Cron · next run in 10 minutes"; got != want {
		t.Errorf("cronStatusAt = %q, want %q", got, want)
	}
	st.Message = "the last run did not succeed"
	if got := cronStatusAt(st, now); !strings.HasSuffix(got, "· the last run did not succeed") {
		t.Errorf("after a failed run = %q, want it said", got)
	}
}

// Two tabs on the same settings: the later save is refused rather than
// putting back what the earlier one changed, and the page reloads to show
// what is there now (CONFIG-10).
func TestAStaleSettingsSaveIsRefused(t *testing.T) {
	a := sampleApp("owner-1", "web")
	apps := newFakeApps(a)
	h := testServer(t, Options{Apps: apps})

	versionRE := regexp.MustCompile(`name="settings_version" value="([0-9a-f]+)"`)
	version := func() string {
		t.Helper()
		m := versionRE.FindStringSubmatch(get(t, h, "/apps/web/settings").Body.String())
		if m == nil {
			t.Fatal("the settings form carries no version")
		}
		return m[1]
	}
	opened := version() // both tabs open the page
	tab := func(change func(url.Values)) url.Values {
		form := url.Values{"image": {a.Image}, "port": {"8080"}, "settings_version": {opened}}
		change(form)
		return form
	}

	tabA := tab(func(f url.Values) { f.Set("pre_deploy_command", "echo from-tab-A") })
	if rec := post(t, h, "/apps/web/runtime", tabA); rec.Code != http.StatusSeeOther || len(apps.updated) != 1 {
		t.Fatalf("tab A's save = %d, %d updates", rec.Code, len(apps.updated))
	}

	tabB := tab(func(f url.Values) { f.Set("health_timeout", "120") })
	rec := post(t, h, "/apps/web/runtime", tabB)
	if len(apps.updated) != 1 {
		t.Fatalf("tab B's stale save was applied: pre-deploy now %q", apps.updated[len(apps.updated)-1].PreDeployCommand)
	}
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/web/settings" {
		t.Errorf("a stale save = %d to %q, want back to the settings", rec.Code, rec.Header().Get("Location"))
	}
	if c := rec.Header().Get("Set-Cookie"); !strings.Contains(c, FlashCookie+"=") {
		t.Errorf("a stale save said nothing: %q", c)
	}

	// Reloaded, the page has the new version, and the save goes through.
	if v := version(); v == opened {
		t.Fatal("tab A's change did not change the version")
	} else {
		tabB.Set("settings_version", v)
		tabB.Set("pre_deploy_command", "echo from-tab-A")
	}
	post(t, h, "/apps/web/runtime", tabB)
	if len(apps.updated) != 2 || apps.updated[1].HealthTimeoutSeconds != 120 ||
		apps.updated[1].PreDeployCommand != "echo from-tab-A" {
		t.Errorf("the save after a reload = %+v", apps.updated)
	}
}
