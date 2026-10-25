package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
)

// referenceApps is the fake service with the optional reference listing.
type referenceApps struct {
	*fakeApps
	targets []app.ReferenceTarget
}

func (r referenceApps) ReferenceTargets(context.Context, string, string) ([]app.ReferenceTarget, error) {
	return r.targets, nil
}

// The picker offers the other apps' variables; choosing one stores the
// reference under the same name, or the one given.
func TestAReferenceIsPickedFromAnotherApp(t *testing.T) {
	web := sampleApp("owner-1", "web")
	apps := referenceApps{fakeApps: newFakeApps(web), targets: []app.ReferenceTarget{{
		App: "db", Keys: []app.ReferenceKey{{Key: "DATABASE_URL", Secret: true}, {Key: app.RefPrivateHost}},
	}}}
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps/web/variables").Body.String()
	for _, want := range []string{"Add a reference", `value="db.DATABASE_URL"`, `value="db.PRIVATE_HOST"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the variables tab does not offer %q", want)
		}
	}

	post(t, h, "/apps/web/variables", url.Values{"ref": {"db.DATABASE_URL"}})
	post(t, h, "/apps/web/variables", url.Values{"ref": {"db.PRIVATE_HOST"}, "key": {"DB_HOST"}})
	got := apps.setVars
	if len(got) != 2 ||
		got[0] != (app.VariableInput{Key: "DATABASE_URL", Value: "${{db.DATABASE_URL}}"}) ||
		got[1] != (app.VariableInput{Key: "DB_HOST", Value: "${{db.PRIVATE_HOST}}"}) {
		t.Errorf("set = %+v", got)
	}
}

// A reference variable shows what it points at, linked, and never as a
// value field somebody could mistake for the resolved one.
func TestAReferenceVariableShowsWhereItComesFrom(t *testing.T) {
	web := sampleApp("owner-1", "web")
	web.Variables = []app.Variable{{Key: "DATABASE_URL", Value: "${{db.DATABASE_URL}}", Reference: true}}
	h := testServer(t, Options{Apps: newFakeApps(web)})
	body := get(t, h, "/apps/web/variables").Body.String()
	if !strings.Contains(body, `href="/apps/db/variables"`) || !strings.Contains(body, "ref-chip") {
		t.Error("the reference does not link to the app it comes from")
	}
}

// Saved while another deploy runs is a save, said as one.
func TestAVariableSavedWhileDeployingIsNotAnError(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	apps.setVarErr = app.ErrSavedWhileDeploying
	h := testServer(t, Options{Apps: apps})
	rec := post(t, h, "/apps/web/variables", url.Values{"key": {"A"}, "value": {"1"}})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("saved while deploying = %d, want a redirect", rec.Code)
	}
}

// The canvas carries its graph for the script: every card, and one edge per
// pair of apps however many variables join them.
func TestTheCanvasCarriesItsGraph(t *testing.T) {
	d := layout(
		[]app.App{{Name: "db"}, {Name: "api"}, {Name: "web"}},
		[]app.Link{
			{From: "api", To: "db", Via: "DATABASE_URL"},
			{From: "api", To: "db", Via: "DB_HOST"},
			{From: "web", To: "api", Via: "API_URL"},
		},
	)
	g := d.Graph()
	if len(g.Nodes) != 3 || len(g.Edges) != 2 {
		t.Fatalf("graph = %+v", g)
	}
	for _, e := range g.Edges {
		if e.From == "api" && strings.Join(e.Via, ",") != "DATABASE_URL,DB_HOST" {
			t.Errorf("api → db via %v", e.Via)
		}
	}

	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})
	body := get(t, h, "/projects/default").Body.String()
	m := regexp.MustCompile(`(?s)<script id="canvas-graph" type="application/json">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the canvas page has no graph for the script")
	}
	var parsed canvasGraph
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &parsed); err != nil || len(parsed.Nodes) != 1 {
		t.Errorf("graph = %s (%v)", m[1], err)
	}
}
