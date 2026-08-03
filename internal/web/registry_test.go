package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/registry"
)

type fakeRegistries struct {
	set      registry.Settings
	canStore bool

	lastPassword string
	sets         int
	cleared      bool
	err          error
}

func (f *fakeRegistries) Settings(context.Context) (registry.Settings, error) {
	return f.set, nil
}

func (f *fakeRegistries) Set(
	_ context.Context, _ uuid.UUID, host, repository, username, password string, insecure bool,
) error {
	if f.err != nil {
		return f.err
	}
	f.sets++
	f.lastPassword = password
	f.set = registry.Settings{
		Host: host, Repository: repository, Username: username, Insecure: insecure,
	}
	return nil
}

func (f *fakeRegistries) Clear(context.Context) error {
	f.cleared = true
	f.set = registry.Settings{}
	return nil
}

func (f *fakeRegistries) CanStore() bool { return f.canStore }

// hrefRE pulls every link out of a page.
var hrefRE = regexp.MustCompile(`href="([^"]+)"`)

// The sidebar cannot offer a page that was never mounted.
//
// This is the bug DNS shipped with: /cluster/dns had a handler, no nav entry,
// and a custom-domain panel telling people to go to it. The fix has to be
// checked in both directions, because a nav entry for an unmounted route is
// the same defect wearing the opposite sign — a link to a 404 reads as a
// broken feature rather than an absent one.
func TestTheSidebarOnlyOffersPagesThatExist(t *testing.T) {
	// Without a registry.
	h := testServer(t, Options{})
	body := get(t, h, "/").Body.String()
	for _, link := range hrefRE.FindAllStringSubmatch(body, -1) {
		if link[1] == "/cluster/registry" {
			t.Error("the sidebar links to the registry on an install that has none")
		}
	}

	// With one.
	h = testServer(t, Options{Registries: &fakeRegistries{canStore: true}})
	body = get(t, h, "/").Body.String()
	if !strings.Contains(body, `href="/cluster/registry"`) {
		t.Error("an install with a registry does not link to it")
	}
}

// Every link the sidebar draws resolves.
//
// The general form of the same check: whatever the nav offers, ask for it and
// see that something answers. A test naming one route only catches the one
// route somebody remembered.
func TestEverySidebarLinkResolves(t *testing.T) {
	h := testServer(t, Options{Registries: &fakeRegistries{canStore: true}})
	body := get(t, h, "/").Body.String()

	seen := map[string]bool{}
	for _, m := range hrefRE.FindAllStringSubmatch(body, -1) {
		href := m[1]
		if !strings.HasPrefix(href, "/") || seen[href] {
			continue
		}
		seen[href] = true

		rec := get(t, h, href)
		if rec.Code == http.StatusNotFound {
			t.Errorf("the sidebar offers %s and nothing serves it", href)
		}
	}
	if len(seen) < 5 {
		t.Fatalf("only %d links found — the scan is broken", len(seen))
	}
}

// An empty password box leaves the stored one alone.
//
// It cannot be read back to prefill, so the box is empty on every visit.
// Treating that as "set it to nothing" means correcting a typo in the
// repository field silently wipes the credential, and the next build is what
// finds out.
func TestSavingWithAnEmptyPasswordDoesNotWipeIt(t *testing.T) {
	reg := &fakeRegistries{
		canStore: true,
		set: registry.Settings{
			Host: "ghcr.io", Repository: "acme", Username: "bot",
		},
	}
	h := testServer(t, Options{Registries: reg})

	rec := httptest.NewRecorder()
	form := strings.NewReader("host=ghcr.io&repository=other&username=bot&password=")
	req := httptest.NewRequest(http.MethodPost, "/cluster/registry", form)
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)

	if reg.sets != 0 {
		t.Fatalf("the registry was written with an empty password (%d writes)", reg.sets)
	}
	if reg.set.Repository != "acme" {
		t.Errorf("the stored settings changed: %+v", reg.set)
	}
	if !strings.Contains(rec.Body.String(), "password again") {
		t.Error("nothing explains why the save did not take")
	}
}

// A first save with a password goes through.
func TestAFirstRegistryIsStored(t *testing.T) {
	reg := &fakeRegistries{canStore: true}
	h := testServer(t, Options{Registries: reg})

	rec := httptest.NewRecorder()
	form := strings.NewReader("host=ghcr.io&repository=acme&username=bot&password=hunter2")
	req := httptest.NewRequest(http.MethodPost, "/cluster/registry", form)
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)

	if reg.sets != 1 || reg.lastPassword != "hunter2" {
		t.Fatalf("not stored: %d writes, password %q", reg.sets, reg.lastPassword)
	}
}

// The page never prints the credential.
//
// Settings has no password field, so this cannot happen by construction — but
// the page also renders a form, and a value attribute is the easy way to put a
// secret in HTML without noticing.
func TestTheRegistryPageDoesNotPrintTheCredential(t *testing.T) {
	reg := &fakeRegistries{
		canStore: true,
		set:      registry.Settings{Host: "ghcr.io", Username: "bot"},
	}
	reg.lastPassword = "hunter2"
	h := testServer(t, Options{Registries: reg})

	body := get(t, h, "/cluster/registry").Body.String()
	if strings.Contains(body, "hunter2") {
		t.Error("the password is in the page")
	}
	if !strings.Contains(body, `type="password"`) {
		t.Error("the password box is not a password field")
	}
}

// Without an encryption key the form says so instead of offering to store one.
func TestWithNoKeyTheFormSaysWhyRatherThanFailingLater(t *testing.T) {
	h := testServer(t, Options{Registries: &fakeRegistries{canStore: false}})
	body := get(t, h, "/cluster/registry").Body.String()

	if !strings.Contains(body, "YACHT_SECRET_KEY") {
		t.Error("nothing names the missing key")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("the form is offered as if it would work")
	}
}
