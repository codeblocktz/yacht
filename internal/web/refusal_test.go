package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
)

// refusingApps refuses a replica count and a volume as the service does.
type refusingApps struct {
	*fakeApps
}

func (f *refusingApps) Scale(context.Context, string, string, int32) (app.App, error) {
	return app.App{}, errors.New("app: replicas must be between 0 and 50")
}

func (f *refusingApps) AttachVolume(context.Context, string, string, app.VolumeInput) (app.Volume, error) {
	return app.Volume{}, errors.New("app: volume name must be lowercase letters, numbers and dashes")
}

// htmxPost is a form posted as htmx posts it.
func htmxPost(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("HX-Request", "true")
	return req
}

// assertRefusedInPlace checks a refused Settings form came back as CONFIG-15
// asks: the canvas with the panel on Settings, the reason beside the form
// rather than at the top of the tab, and what was typed still in the form.
// From htmx it is the Settings tab alone, to swap in where it was.
func assertRefusedInPlace(
	t *testing.T, h http.Handler, path string, form url.Values,
	reason, section, field string, kept ...string,
) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	rec := do(h, req)
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	for _, want := range append([]string{`class="canvas`, `id="canvas-sheet"`, `id="settings-tab"`, reason}, kept...) {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal is missing %q", want)
		}
	}
	if strings.Contains(body, "Switch app") {
		t.Error("the refusal is the app's old page of its own")
	}
	tab := body[max(strings.Index(body, `id="settings-tab"`), 0):]
	at, from, input := strings.Index(tab, reason), strings.Index(tab, section), strings.Index(tab, field)
	if at < 0 || from < 0 || input < 0 || at < from || at > input {
		t.Errorf("the reason is not beside the form: %q at %d, %q at %d, %q at %d", reason, at, section, from, field, input)
	}

	rec = do(h, htmxPost(path, form))
	body = rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.HasPrefix(strings.TrimSpace(body), `<div id="settings-tab">`) {
		t.Errorf("from htmx = %d, starting %.60q; want 422 and the tab alone", rec.Code, body)
	}
	if strings.Contains(body, "<html") || !strings.Contains(body, reason) {
		t.Error("from htmx, the fragment is a whole page or lost the reason")
	}
	for _, want := range kept {
		if !strings.Contains(body, want) {
			t.Errorf("from htmx, the fragment lost %q", want)
		}
	}
}

// A refused custom domain keeps the host typed, under Custom domains.
func TestARefusedCustomDomainKeepsTheHost(t *testing.T) {
	nets := &fakeNets{
		err: errors.New(`"10.0.0.9" is not a valid hostname: an IP address cannot be a custom domain`),
		net: app.Networking{Managed: "web.apps.test", Target: "edge.yacht.test"},
	}
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web")), Nets: nets})
	assertRefusedInPlace(t, h, "/apps/web/domains", url.Values{"host": {"10.0.0.9"}},
		"an IP address cannot be a custom domain", "Custom domains", `id="host"`,
		`value="10.0.0.9"`, `hx-post="/apps/web/domains"`)

	nets.err = nil
	rec := do(h, htmxPost("/apps/web/domains", url.Values{"host": {"shop.example.com"}}))
	if rec.Header().Get("HX-Redirect") != "/apps/web/domains" {
		t.Errorf("a domain added from htmx = %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}

// A refused name for the generated domain keeps the edit open on what was
// typed, name and port.
func TestARefusedGeneratedDomainKeepsTheEdit(t *testing.T) {
	nets := &fakeNets{
		err: errors.New(`"bad label!" is not a valid name: lowercase letters, numbers and dashes`),
		net: app.Networking{Public: app.PublicDomain{
			Host: "web-production.up.apps.test", Label: "web-production", Domain: "up.apps.test",
			Generated: true, Routed: true, Port: 3000,
		}},
	}
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web")), Nets: nets})
	assertRefusedInPlace(t, h, "/apps/web/networking/domain",
		url.Values{"tab": {"settings"}, "label": {"bad label!"}, "port": {"8081"}},
		"is not a valid name", "Edit domain or port", `id="public-label"`,
		`value="bad label!"`, `value="8081"`, `data-public-domain-edit open`)
}

// A refused replica count keeps the number typed, under Replicas.
func TestARefusedReplicaCountKeepsTheNumber(t *testing.T) {
	apps := &refusingApps{newFakeApps(sampleApp("owner-1", "web"))}
	h := testServer(t, Options{Apps: apps})
	assertRefusedInPlace(t, h, "/apps/web/scale", url.Values{"replicas": {"-1"}},
		"replicas must be between 0 and 50", `id="replicas"`, `name="replicas"`,
		`value="-1"`)

	// Not a number at all is said too, rather than redirected past.
	rec := post(t, h, "/apps/web/scale", url.Values{"replicas": {"two"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "a whole number") {
		t.Errorf("replicas=two = %d", rec.Code)
	}
}

// A refused volume keeps its name, mount path and size, under Add a volume.
func TestARefusedVolumeKeepsTheForm(t *testing.T) {
	apps := &refusingApps{newFakeApps(sampleApp("owner-1", "web"))}
	h := testServer(t, Options{Apps: apps})
	assertRefusedInPlace(t, h, "/apps/web/storage",
		url.Values{"name": {"BAD NAME"}, "mount_path": {"relative"}, "size_gb": {"3"}},
		"volume name must be lowercase letters", "Add a volume", `id="vol-name"`,
		`value="BAD NAME"`, `value="relative"`, `value="3"`)
}
