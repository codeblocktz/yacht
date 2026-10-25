package web

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
)

// The deployments tab always polls, and a poll that finds nothing changed
// is answered with no body, so htmx leaves the tab as it is.
func TestTheDeploymentsTabPollsCheaply(t *testing.T) {
	a := sampleApp("owner-1", "web")
	apps := newFakeApps(a)
	apps.deploys = []app.Deployment{{ID: uuid.New(), AppID: a.ID, Image: "nginx:alpine", Revision: "initial",
		Status: app.DeploySucceeded, IsActive: true, StartedAt: time.Now()}}
	h := testServer(t, Options{Apps: apps})
	body := get(t, h, "/apps/web").Body.String()
	m := regexp.MustCompile(`hx-get="(/apps/web/deployments/fragment\?token=[0-9a-f]+)"`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, `hx-trigger="every`) {
		t.Fatal("the deployments tab does not poll")
	}
	if rec := get(t, h, m[1]); rec.Code != http.StatusNoContent {
		t.Errorf("an unchanged poll = %d, want 204", rec.Code)
	}
	if rec := get(t, h, "/apps/web/deployments/fragment?token=stale"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `id="app-deployments"`) {
		t.Errorf("a changed poll = %d", rec.Code)
	}
}

// The canvas asks for each card's state, in the words the card uses.
func TestTheCanvasStatusIsEachCardsState(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})
	rec := get(t, h, "/projects/default/status")
	var out struct {
		Apps map[string]struct{ Label, Class string } `json:"apps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status = %s: %v", rec.Body.String(), err)
	}
	if got := out.Apps["web"]; got.Label != "Online" || got.Class != "card-status-ok" {
		t.Errorf("web = %+v, want Online", got)
	}
}
