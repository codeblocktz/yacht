package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
)

// The storage tab is the only way to attach a volume without reaching for SQL.
// Everything under it is proven; these are about the surface.

func storageHarness(t *testing.T, team string) (*liveHarness, *http.Cookie) {
	t.Helper()
	h := newLiveHarnessOwnedBy(t, team, "founder-store@web.test")
	if code := postSignIn(h.handler, "founder-store@web.test", "198.51.100.7:2000").Code; code != http.StatusOK {
		t.Fatalf("sign-in = %d", code)
	}
	sent := h.mailer.messages()
	c := sessionCookie(h.follow(t, linkIn(t, sent[len(sent)-1].TextBody)))
	if c == nil {
		t.Fatal("no session")
	}
	if _, err := h.apps.Create(context.Background(), team, app.CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return h, c
}

func TestStorageTabListsAnAppsVolumes(t *testing.T) {
	h, c := storageHarness(t, "web-store-list")

	if rec := h.postFormAs(t, "/apps/web/storage", c, url.Values{
		"name": {"data"}, "mount_path": {"/var/lib/data"}, "size_gb": {"2"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("attach = %d, want 303\n%s", rec.Code, rec.Body.String())
	}

	body := h.getAs(t, "/apps/web/storage", c).Body.String()
	for _, want := range []string{"data", "/var/lib/data"} {
		if !strings.Contains(body, want) {
			t.Errorf("storage tab does not show %q", want)
		}
	}
}

// Storage means one replica and a deploy with downtime. Both are consequences
// the person is choosing, so the page has to say them before they choose.
func TestStorageTabWarnsAboutTheTradeoff(t *testing.T) {
	h, c := storageHarness(t, "web-store-warn")

	body := h.getAs(t, "/apps/web/storage", c).Body.String()
	if !strings.Contains(strings.ToLower(body), "one replica") {
		t.Error("the storage tab does not mention the one-replica limit")
	}
	if !strings.Contains(strings.ToLower(body), "downtime") {
		t.Error("the storage tab does not mention that deploys recreate")
	}
}

func TestStorageRejectsAShrink(t *testing.T) {
	h, c := storageHarness(t, "web-store-shrink")

	if rec := h.postFormAs(t, "/apps/web/storage", c, url.Values{
		"name": {"data"}, "mount_path": {"/var/lib/data"}, "size_gb": {"4"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("attach = %d", rec.Code)
	}
	rec := h.postFormAs(t, "/apps/web/storage/data/resize", c, url.Values{"size_gb": {"2"}})
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a shrink was accepted")
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "grow") {
		t.Errorf("the refusal does not explain why: %s", rec.Body.String())
	}
}

// Deleting storage is not a side effect of tidying up. The form has to ask.
func TestDeletingStorageRequiresConfirmation(t *testing.T) {
	h, c := storageHarness(t, "web-store-del")

	if rec := h.postFormAs(t, "/apps/web/storage", c, url.Values{
		"name": {"data"}, "mount_path": {"/var/lib/data"}, "size_gb": {"1"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("attach = %d", rec.Code)
	}

	if rec := h.postFormAs(t, "/apps/web/storage/data/delete", c, url.Values{}); rec.Code == http.StatusSeeOther {
		t.Fatal("storage was deleted without confirmation")
	}
	if rec := h.postFormAs(t, "/apps/web/storage/data/delete", c,
		url.Values{"confirm": {"data"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirmed delete = %d, want 303", rec.Code)
	}
}

// A member deploys; changing what an app stores is administration.
func TestMemberCannotChangeStorage(t *testing.T) {
	rt := newRoleTeam(t, "web-store-role", "role-app")

	for _, path := range []string{
		"/apps/role-app/storage",
		"/apps/role-app/storage/data/resize",
		"/apps/role-app/storage/data/delete",
	} {
		if code := rt.postAs(t, path, rt.member).Code; code != http.StatusForbidden {
			t.Errorf("POST %s as a member = %d, want 403", path, code)
		}
	}
}
