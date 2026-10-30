package web

import (
	"strings"
	"testing"
)

// Settings said "This install runs in single-owner mode", and showed an Owner
// and a bearer token, on an install hosting several teams that sign in by
// email. Single-owner mode is an install without accounts; with them the
// page says it hosts teams, names its operators rather than whichever team
// the operator is signed in as, and says how people sign in.
func TestSettingsDescribesTheInstallItIsOn(t *testing.T) {
	t.Run("single owner", func(t *testing.T) {
		h := testServer(t, Options{Authenticated: true, AppDomain: "apps.test"})
		body := get(t, h, "/settings").Body.String()
		for _, want := range []string{"single-owner mode", "<dt>Owner</dt>", "Bearer token", "*.up.apps.test"} {
			if !strings.Contains(body, want) {
				t.Errorf("a single-owner install's settings are missing %q", want)
			}
		}
	})

	t.Run("teams with named operators", func(t *testing.T) {
		rt := newRoleTeamWith(t, "web-settings-ops", "settings-app", []string{"admin@web.test"},
			func(o *Options) { o.AppDomain = "apps.test" })
		body := rt.getAs(t, "/settings", rt.admin).Body.String()
		for _, want := range []string{
			"hosts teams", "<dt>Operators</dt>", "admin@web.test",
			"Email link or password", "*.up.apps.test",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("settings on an install with teams are missing %q", want)
			}
		}
		for _, stale := range []string{"single-owner", "<dt>Owner</dt>", "Bearer token", ">Magic link<"} {
			if strings.Contains(body, stale) {
				t.Errorf("settings on an install with teams still say %q", stale)
			}
		}
	})

	t.Run("teams with no operators named", func(t *testing.T) {
		rt := newRoleTeam(t, "web-settings-noops", "settings-noops-app")
		body := rt.getAs(t, "/settings", rt.owner).Body.String()
		if !strings.Contains(body, "YACHT_OPERATORS") || !strings.Contains(body, "Each team") {
			t.Error("settings do not warn that every team's owner is an operator")
		}
	})
}
