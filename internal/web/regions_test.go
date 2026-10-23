package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// regionServer is a signed-in owner's dashboard, with regions.
func regionServer(t *testing.T, apps *fakeApps, regions Regions, stacks Stacks) http.Handler {
	t.Helper()
	const team = "owner-1"
	s, err := New(Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            apps,
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: "owner"},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		Stacks:          stacks,
		Regions:         regions,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

// fakeRegions is an install's regions without a database or a cluster.
type fakeRegions struct {
	regions   []app.RegionInfo
	choice    app.RegionChoice
	appRegion string

	set     []app.Region
	setErr  error
	moved   []string
	moveErr error
}

func (f *fakeRegions) Regions(context.Context) ([]app.RegionInfo, error) { return f.regions, nil }

func (f *fakeRegions) Region(_ context.Context, name string) (app.RegionInfo, error) {
	for _, r := range f.regions {
		if r.Name == name {
			return r, nil
		}
	}
	return app.RegionInfo{}, app.ErrNoSuchRegion
}

func (f *fakeRegions) SetRegion(_ context.Context, r app.Region) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if f.setErr != nil {
		return f.setErr
	}
	f.set = append(f.set, r)
	return nil
}

func (f *fakeRegions) RegionChoice(context.Context) (app.RegionChoice, error) { return f.choice, nil }

func (f *fakeRegions) AppRegion(context.Context, string, string) (string, error) {
	return f.appRegion, nil
}

func (f *fakeRegions) MoveApp(_ context.Context, _, name, region string) error {
	if f.moveErr != nil {
		return f.moveErr
	}
	f.moved = append(f.moved, name+"->"+region)
	return nil
}

// twoRegionsFake is an install with fsn, open and the default, and dar,
// open; and ams, which machines carry and nobody configured.
func twoRegionsFake() *fakeRegions {
	fsn := app.RegionInfo{
		Region: app.Region{Name: "fsn", DisplayName: "Falkenstein", Open: true, StorageClass: "replicated-fsn",
			Edge: app.Edge{Host: "edge-fsn.example.test"}},
		Configured: true, Nodes: 2, Ready: 2, Sites: []string{"fsn-a", "fsn-b"}, Apps: 3,
		Default: true, Choosable: true,
	}
	dar := app.RegionInfo{
		Region:     app.Region{Name: "dar", DisplayName: "Dar es Salaam", Open: true},
		Configured: true, Nodes: 1, Ready: 1, Apps: 1, Choosable: true,
	}
	ams := app.RegionInfo{Region: app.Region{Name: "ams"}, Nodes: 1, Ready: 1}
	return &fakeRegions{
		regions: []app.RegionInfo{ams, dar, fsn},
		choice:  app.RegionChoice{InUse: true, Default: "fsn", Options: []app.RegionInfo{dar, fsn}},
	}
}

// On an install with no regions the deploy form says nothing about them; on
// one with regions it offers the open ones with the default chosen, and the
// choice reaches the create.
func TestTheDeployFormOffersRegionsOnlyWhereThereAreSome(t *testing.T) {
	apps := newFakeApps()
	none := testServer(t, Options{Apps: apps, Regions: &fakeRegions{}})
	body := do(none, signedIn(http.MethodGet, "/apps/new?source=image")).Body.String()
	if strings.Contains(body, `name="region"`) || strings.Contains(body, "Where it runs") {
		t.Fatal("an install with no regions drew a region field")
	}

	regions := twoRegionsFake()
	h := regionServer(t, apps, regions, nil)
	body = do(h, signedIn(http.MethodGet, "/apps/new?source=image")).Body.String()
	for _, want := range []string{`name="region"`, "Falkenstein", "Dar es Salaam", `value="fsn"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the form is missing %q", want)
		}
	}
	if strings.Contains(body, `value="ams"`) {
		t.Error("an unconfigured region was offered")
	}

	form := url.Values{"source": {"image"}, "name": {"web"}, "image": {"nginx:alpine"}, "region": {"dar"}}
	rec := do(h, signedInForm(http.MethodPost, "/apps", form.Encode()))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create = %d, want a redirect", rec.Code)
	}
	if len(apps.created) != 1 || apps.created[0].Region != "dar" {
		t.Fatalf("created %+v, want web in dar", apps.created)
	}

	// No region open says so, rather than offering an empty list.
	h = testServer(t, Options{Apps: apps, Regions: &fakeRegions{choice: app.RegionChoice{InUse: true}}})
	body = do(h, signedIn(http.MethodGet, "/apps/new?source=image")).Body.String()
	if !strings.Contains(body, "No region is open for new apps") {
		t.Error("with every region closed the form does not say so")
	}
}

// A stack is placed in one region, chosen on the templates page.
func TestATemplateIsDeployedInTheChosenRegion(t *testing.T) {
	stacks := &fakeStacks{}
	h := regionServer(t, newFakeApps(), twoRegionsFake(), stacks)
	body := do(h, signedIn(http.MethodGet, "/templates")).Body.String()
	if !strings.Contains(body, `name="region"`) {
		t.Fatal("the templates page offers no region on an install with regions")
	}
	form := url.Values{"template": {"postgres-app"}, "stack": {"shop"}, "region": {"dar"}}
	if rec := do(h, signedInForm(http.MethodPost, "/templates", form.Encode())); rec.Code != http.StatusSeeOther {
		t.Fatalf("deploy = %d", rec.Code)
	}
	if len(stacks.regions) != 1 || stacks.regions[0] != "dar" {
		t.Fatalf("stack deployed in %v, want dar", stacks.regions)
	}
}

// The operator's list shows every region, configured or not, with what is in
// it; one with nothing configured is called that and offered configuring.
func TestTheRegionsPageListsEveryRegion(t *testing.T) {
	h := regionServer(t, newFakeApps(), twoRegionsFake(), nil)
	body := do(h, signedIn(http.MethodGet, "/admin/regions")).Body.String()
	for _, want := range []string{
		"Falkenstein", "Dar es Salaam", "unconfigured", "default",
		"edge-fsn.example.test", "replicated-fsn", "fsn-a, fsn-b",
		`href="/admin/regions/ams"`, ">Configure<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the regions page is missing %q", want)
		}
	}

	empty := testServer(t, Options{Apps: newFakeApps(), Regions: &fakeRegions{}})
	body = do(empty, signedIn(http.MethodGet, "/admin/regions")).Body.String()
	if !strings.Contains(body, "This install has no regions") ||
		!strings.Contains(body, "topology.kubernetes.io/region") {
		t.Error("an install with no regions is not told how to add one")
	}
}

// Saving a region keeps what was typed; a refusal says why and keeps it too.
func TestARegionIsSavedFromItsPage(t *testing.T) {
	regions := twoRegionsFake()
	h := regionServer(t, newFakeApps(), regions, nil)

	body := do(h, signedIn(http.MethodGet, "/admin/regions/ams")).Body.String()
	if !strings.Contains(body, "Not configured yet") || !strings.Contains(body, `name="edge_host"`) {
		t.Fatal("an unconfigured region's page does not offer configuring it")
	}
	if code := do(h, signedIn(http.MethodGet, "/admin/regions/AMS%3Bid")).Code; code != http.StatusNotFound {
		t.Errorf("a malformed region's page = %d, want 404", code)
	}

	form := url.Values{
		"display_name": {"Amsterdam"}, "edge_host": {"edge-ams.example.test"},
		"edge_addresses": {"203.0.113.20\n2001:db8::20"}, "storage_class": {"replicated-ams"}, "open": {"1"},
	}
	rec := do(h, signedInForm(http.MethodPost, "/admin/regions/ams", form.Encode()))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d, want a redirect", rec.Code)
	}
	if len(regions.set) != 1 {
		t.Fatalf("saved %+v", regions.set)
	}
	got := regions.set[0]
	if got.Name != "ams" || got.DisplayName != "Amsterdam" || !got.Open || got.StorageClass != "replicated-ams" ||
		got.Edge.Host != "edge-ams.example.test" || len(got.Edge.Addresses) != 2 {
		t.Fatalf("saved %+v", got)
	}

	form.Set("edge_host", "https://edge-ams.example.test")
	rec = do(h, signedInForm(http.MethodPost, "/admin/regions/ams", form.Encode()))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a bad edge = %d, want 422", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "is not a hostname") || !strings.Contains(body, "replicated-ams") {
		t.Error("a refused save did not say why, or lost what was typed")
	}
}

// The app page says where an app lives, offers moving one without storage,
// and explains why one with storage stays.
func TestTheAppPageShowsItsRegion(t *testing.T) {
	a := sampleApp("owner-1", "web")
	regions := twoRegionsFake()
	regions.appRegion = "fsn"
	a.Region = "fsn"
	h := regionServer(t, newFakeApps(a), regions, nil)

	body := do(h, signedIn(http.MethodGet, "/apps/web/settings")).Body.String()
	for _, want := range []string{"Lives in Falkenstein", `href="#region"`, `action="/apps/web/region"`, "Dar es Salaam"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page is missing %q", want)
		}
	}

	rec := do(h, signedInForm(http.MethodPost, "/apps/web/region", url.Values{"region": {"dar"}}.Encode()))
	if rec.Code != http.StatusSeeOther || len(regions.moved) != 1 || regions.moved[0] != "web->dar" {
		t.Fatalf("move = %d, moved %v", rec.Code, regions.moved)
	}

	db := sampleApp("owner-1", "db")
	db.Region = "fsn"
	db.Volumes = []app.Volume{{Name: "data", MountPath: "/data", SizeBytes: 1 << 30}}
	h = regionServer(t, newFakeApps(db), regions, nil)
	body = do(h, signedIn(http.MethodGet, "/apps/db/settings")).Body.String()
	if !strings.Contains(body, "An app with storage stays in its region") || strings.Contains(body, `action="/apps/db/region"`) {
		t.Error("an app with storage was offered a move, or not told why it has none")
	}
	regions.moveErr = app.ErrMoveHasStorage
	rec = do(h, signedInForm(http.MethodPost, "/apps/db/region", url.Values{"region": {"dar"}}.Encode()))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a refused move = %d, want a redirect with the reason flashed", rec.Code)
	}

	// No regions, no section.
	h = testServer(t, Options{Apps: newFakeApps(a), Regions: &fakeRegions{}})
	body = do(h, signedIn(http.MethodGet, "/apps/web/settings")).Body.String()
	if strings.Contains(body, `id="region"`) || strings.Contains(body, "Lives in") {
		t.Error("an install with no regions drew an app's region")
	}
}

// The app list says which region each app is in, where it has one.
func TestTheAppListShowsRegions(t *testing.T) {
	a, b := sampleApp("owner-1", "web"), sampleApp("owner-1", "api")
	a.Region = "fsn"
	h := regionServer(t, newFakeApps(a, b), twoRegionsFake(), nil)
	body := do(h, signedIn(http.MethodGet, "/apps")).Body.String()
	if !strings.Contains(body, `title="Region">fsn<`) {
		t.Error("an app's region is not on the list")
	}
}
