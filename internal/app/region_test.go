package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// The regions these tests configure. Removed before and after each test, so
// that a test elsewhere in the shared database never sees an install with
// regions it did not make.
var testRegions = []string{"fsn", "dar", "ams"}

// twoRegions is a cluster with a machine in fsn (at site fsn-a) and one in
// dar, each with a core and 2 GiB to give.
func twoRegions() []orchestrator.NodeInfo {
	return []orchestrator.NodeInfo{
		{Name: "fsn-1", Ready: true, Region: "fsn", Site: "fsn-a",
			CPUAllocatableMillis: 1000, MemAllocatableBytes: 2 << 30},
		{Name: "dar-1", Ready: true, Region: "dar",
			CPUAllocatableMillis: 1000, MemAllocatableBytes: 2 << 30},
	}
}

// setNodes makes the service see these machines, forgetting what it read.
func setNodes(s *Service, nodes []orchestrator.NodeInfo) {
	s.room.mu.Lock()
	defer s.room.mu.Unlock()
	s.room.read = func(context.Context) ([]orchestrator.NodeInfo, error) {
		return slices.Clone(nodes), nil
	}
	s.room.at = time.Time{}
}

func clearRegions(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`DELETE FROM regions WHERE name = ANY($1)`, testRegions); err != nil {
		t.Fatalf("clear regions: %v", err)
	}
}

// regionService is a service on a cluster with machines in fsn and dar, both
// configured and open, fsn's volumes from its own StorageClass.
func regionService(t *testing.T, opts Options) (*Service, *convergenceOrchestrator, string) {
	t.Helper()
	s, orch, id := convergenceService(t, opts)
	clearRegions(t, s)
	t.Cleanup(func() {
		s.routesWG.Wait()
		clearRegions(t, s)
	})
	setNodes(s, twoRegions())
	ctx := context.Background()
	for _, r := range []Region{
		{Name: "fsn", DisplayName: "Falkenstein", Open: true, StorageClass: "replicated-fsn",
			Edge: Edge{Host: "edge-fsn.example.test", Addresses: []string{"203.0.113.10"}}},
		{Name: "dar", Open: true},
	} {
		if err := s.SetRegion(ctx, r); err != nil {
			t.Fatalf("SetRegion %s: %v", r.Name, err)
		}
	}
	return s, orch, id
}

// The decisions a region view makes, without a database: what is in use,
// what may be chosen, and where an app goes when nobody says.
func TestARegionViewDecidesWhereAppsGo(t *testing.T) {
	open := func(names ...string) map[string]dbgen.Region {
		out := map[string]dbgen.Region{}
		for _, n := range names {
			out[n] = dbgen.Region{Name: n, Open: true}
		}
		return out
	}
	str := func(s string) *string { return &s }

	t.Run("no labelled machine is no regions", func(t *testing.T) {
		v := regionView{nodesKnown: true, nodes: []orchestrator.NodeInfo{{Name: "a", Ready: true}},
			rows: open("fsn")}
		if v.inUse() {
			t.Fatal("rows alone put a cluster whose machines carry no region in use")
		}
		if got, err := v.resolveNew(""); got != "" || err != nil {
			t.Fatalf("resolveNew(\"\") = %q, %v; want no region", got, err)
		}
		if _, err := v.resolveNew("fsn"); !errors.Is(err, ErrNoRegions) {
			t.Fatalf("asking for a region with none = %v, want ErrNoRegions", err)
		}
		if got := v.effective(nil); got != "" {
			t.Fatalf("an unrecorded app is in %q, want none", got)
		}
	})

	t.Run("an unreadable cluster leans on the rows", func(t *testing.T) {
		v := regionView{rows: open("fsn")}
		if !v.inUse() || !v.choosable("fsn") {
			t.Fatal("with the cluster unreadable a configured open region is not usable")
		}
		if (regionView{}).inUse() {
			t.Fatal("an unreadable cluster with no rows has regions")
		}
	})

	nodes := append(twoRegions(),
		orchestrator.NodeInfo{Name: "ams-1", Ready: true, Region: "ams"},
		orchestrator.NodeInfo{Name: "dar-2", Ready: false, Region: "dar"},
	)
	v := regionView{nodesKnown: true, nodes: nodes, rows: open("fsn", "dar")}
	v.rows["dar"] = dbgen.Region{Name: "dar", Open: false}

	if !v.inUse() {
		t.Fatal("machines with regions are not in use")
	}
	if got := v.names(); !slices.Equal(got, []string{"ams", "dar", "fsn"}) {
		t.Fatalf("names = %v", got)
	}
	for name, want := range map[string]bool{"fsn": true, "dar": false, "ams": false, "nyc": false} {
		if got := v.choosable(name); got != want {
			t.Errorf("choosable(%s) = %v, want %v (closed, unconfigured and unknown are not)", name, got, want)
		}
	}
	if got := v.newAppDefault(); got != "fsn" {
		t.Fatalf("newAppDefault = %q, want the only open region", got)
	}
	if got := v.recordDefault(); got != "fsn" {
		t.Fatalf("recordDefault = %q, want the only open region", got)
	}
	for asked, want := range map[string]error{
		"dar": ErrRegionClosed, "ams": ErrRegionClosed, "nyc": ErrNoSuchRegion,
	} {
		if _, err := v.resolveNew(asked); !errors.Is(err, want) {
			t.Errorf("resolveNew(%s) = %v, want %v", asked, err, want)
		}
	}
	if _, err := v.resolveNew("fsn;id"); err == nil {
		t.Error("a region name a shell would read as two commands was accepted")
	}
	if got := v.effective(str("dar")); got != "dar" {
		t.Errorf("a recorded region is %q, want it as recorded", got)
	}

	// A configured default wins while it can take apps; when it cannot, the
	// first open one does, but an app is still recorded in the default.
	v.configured = "dar"
	if got := v.newAppDefault(); got != "fsn" {
		t.Errorf("a closed default took a new app: %q", got)
	}
	if got := v.recordDefault(); got != "dar" {
		t.Errorf("recordDefault = %q, want the configured default", got)
	}

	// Two open regions and no default named: a new app goes to the first,
	// and nothing is recorded on a guess.
	v.configured = ""
	v.rows["dar"] = dbgen.Region{Name: "dar", Open: true}
	if got := v.newAppDefault(); got != "dar" {
		t.Errorf("newAppDefault = %q, want the first open region", got)
	}
	if got := v.recordDefault(); got != "" {
		t.Errorf("recordDefault guessed %q among several regions", got)
	}

	// A region with no machine ready has room that is known and nothing in it.
	if r := v.roomIn("nyc"); !r.Known || r.CPUMillis != 0 {
		t.Errorf("roomIn(nyc) = %+v, want known and empty", r)
	}
	if r := v.roomIn("dar"); r.Nodes != 2 || r.Schedulable != 1 || r.CPUMillis != 1000 {
		t.Errorf("roomIn(dar) = %+v, want two machines, one ready, its core", r)
	}
}

// An install whose machines carry no region behaves as it always did: no
// region recorded, none in the spec, none offered, none accepted.
func TestAnInstallWithoutRegionsIsUnchanged(t *testing.T) {
	ctx := context.Background()
	s, orch, id := convergenceService(t, Options{Keeper: testKeeper(t), AppDomain: "apps.test"})
	clearRegions(t, s)
	nodes := twoRegions()
	for i := range nodes {
		nodes[i].Region = ""
	}
	setNodes(s, nodes)

	a, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "web", Image: "nginx:1.27", Replicas: 2, Port: 8080})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Region != "" || orch.lastAppSpec().Region != "" {
		t.Fatalf("an install with no regions placed an app in %q / %q", a.Region, orch.lastAppSpec().Region)
	}
	if _, err := s.Create(ctx, id, CreateInput{Name: "api", Image: "nginx:1.27", Region: "fsn"}); !errors.Is(err, ErrNoRegions) {
		t.Fatalf("asking for a region with none = %v, want ErrNoRegions", err)
	}
	if c, err := s.RegionChoice(ctx); err != nil || c.InUse || len(c.Options) > 0 {
		t.Fatalf("RegionChoice = %+v, %v; want nothing to offer", c, err)
	}
	if r, err := s.AppRegion(ctx, id, "web"); err != nil || r != "" {
		t.Fatalf("AppRegion = %q, %v; want none", r, err)
	}
	if err := s.MoveApp(ctx, id, "web", "fsn"); !errors.Is(err, ErrNoRegions) {
		t.Fatalf("MoveApp = %v, want ErrNoRegions", err)
	}

	db, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "db"})
	if err != nil {
		t.Fatalf("Create postgres: %v", err)
	}
	if db.Volumes[0].Class != "" {
		t.Fatalf("a volume on an install with no regions asked for class %q", db.Volumes[0].Class)
	}
	if n, err := s.BackfillRegions(ctx); err != nil || n != 0 {
		t.Fatalf("BackfillRegions = %d, %v; want nothing recorded", n, err)
	}
	routes, err := s.Routes(ctx)
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	found := false
	for _, r := range routes {
		if r.Host == "web.apps.test" {
			found = true
			if r.Region != "" || r.Edge.Set() {
				t.Fatalf("route %+v has a region on an install with none", r)
			}
		}
	}
	if !found {
		t.Fatalf("Routes = %+v, missing web.apps.test", routes)
	}
	snap, err := s.Capacity(ctx)
	if err != nil || len(snap.Regions) != 0 {
		t.Fatalf("Capacity regions = %+v, %v; want none", snap.Regions, err)
	}
}

// A new app lives in the region it was given, or the default; its pods are
// required there, together with its team's pool.
func TestNewAppsArePlacedInARegion(t *testing.T) {
	ctx := context.Background()
	s, orch, id := regionService(t, Options{DefaultRegion: "dar"})

	a, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Region != "dar" || orch.lastAppSpec().Region != "dar" {
		t.Fatalf("an app given no region is in %q, applied in %q; want the default dar",
			a.Region, orch.lastAppSpec().Region)
	}

	if err := s.SetTeamPool(ctx, id, "pro"); err != nil {
		t.Fatalf("SetTeamPool: %v", err)
	}
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "api", Image: "nginx:1.27", Replicas: 1, Port: 8080, Region: " fsn ",
	}); err != nil {
		t.Fatalf("Create in fsn: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Region != "fsn" || spec.Pool != "pro" {
		t.Fatalf("applied in %q on %q; want fsn and the team's pool, both", spec.Region, spec.Pool)
	}
	if r, err := s.AppRegion(ctx, id, "api"); err != nil || r != "fsn" {
		t.Fatalf("AppRegion = %q, %v", r, err)
	}

	// Closed, unconfigured, unknown and malformed regions take nothing.
	if err := s.SetRegion(ctx, Region{Name: "dar", Open: false}); err != nil {
		t.Fatalf("close dar: %v", err)
	}
	setNodes(s, append(twoRegions(), orchestrator.NodeInfo{Name: "ams-1", Ready: true, Region: "ams"}))
	for region, want := range map[string]error{
		"dar": ErrRegionClosed, "ams": ErrRegionClosed, "nyc": ErrNoSuchRegion,
	} {
		_, err := s.Create(ctx, id, CreateInput{Name: "x-" + region, Image: "nginx:1.27", Region: region})
		if !errors.Is(err, want) {
			t.Errorf("Create in %s = %v, want %v", region, err, want)
		}
	}
	if _, err := s.Create(ctx, id, CreateInput{Name: "bad", Image: "nginx:1.27", Region: "fsn$(id)"}); err == nil {
		t.Error("a malformed region was accepted")
	}
	if n, _ := s.Count(ctx, id); n != 2 {
		t.Fatalf("refused creates left %d apps, want 2", n)
	}

	// The default closed, a new app goes to the region that is open.
	b, err := s.Create(ctx, id, CreateInput{Name: "next", Image: "nginx:1.27"})
	if err != nil || b.Region != "fsn" {
		t.Fatalf("with the default closed a new app went to %q, %v; want fsn", b.Region, err)
	}

	// Nothing open at all is said, rather than placing an app nowhere.
	if err := s.SetRegion(ctx, Region{Name: "fsn", Open: false}); err != nil {
		t.Fatalf("close fsn: %v", err)
	}
	if _, err := s.Create(ctx, id, CreateInput{Name: "none", Image: "nginx:1.27"}); !errors.Is(err, ErrNoRegionOpen) {
		t.Fatalf("Create with every region closed = %v, want ErrNoRegionOpen", err)
	}
}

// A volume made in a region comes from the region's StorageClass and keeps
// it: a class changed later reaches new volumes only.
func TestVolumesInARegionUseItsStorageClass(t *testing.T) {
	ctx := context.Background()
	s, orch, id := regionService(t, Options{Keeper: testKeeper(t)})

	db, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "db", Region: "fsn"})
	if err != nil {
		t.Fatalf("Create postgres in fsn: %v", err)
	}
	if len(db.Volumes) != 1 || db.Volumes[0].Class != "replicated-fsn" {
		t.Fatalf("a database in fsn has volumes %+v, want its region's class", db.Volumes)
	}
	spec := orch.lastAppSpec()
	if spec.Region != "fsn" || len(spec.Volumes) != 1 || spec.Volumes[0].Class != "replicated-fsn" {
		t.Fatalf("applied %+v in %q, want the claim from replicated-fsn in fsn", spec.Volumes, spec.Region)
	}

	if err := s.SetRegion(ctx, Region{Name: "fsn", Open: true, StorageClass: "replicated-fsn-2"}); err != nil {
		t.Fatalf("SetRegion: %v", err)
	}
	if _, err := s.AttachVolume(ctx, id, "db", VolumeInput{Name: "wal", MountPath: "/wal", SizeBytes: 1 << 30}); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if _, err := s.AttachVolume(ctx, id, "db", VolumeInput{
		Name: "fast", MountPath: "/fast", SizeBytes: 1 << 30, Class: "local-nvme",
	}); err != nil {
		t.Fatalf("AttachVolume with a class: %v", err)
	}
	classes := map[string]string{}
	for _, v := range mustGet(t, s, id, "db").Volumes {
		classes[v.Name] = v.Class
	}
	want := map[string]string{"data": "replicated-fsn", "wal": "replicated-fsn-2", "fast": "local-nvme"}
	for name, class := range want {
		if classes[name] != class {
			t.Errorf("volume %s has class %q, want %q", name, classes[name], class)
		}
	}

	// dar names no class: the cluster's default, as before regions.
	other, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "db2", Region: "dar"})
	if err != nil {
		t.Fatalf("Create postgres in dar: %v", err)
	}
	if other.Volumes[0].Class != "" {
		t.Fatalf("a volume in a region with no class asked for %q", other.Volumes[0].Class)
	}
}

// A stack goes to one region whole.
func TestATemplateIsDeployedInOneRegion(t *testing.T) {
	ctx := context.Background()
	s, _, id := regionService(t, Options{Keeper: testKeeper(t)})

	if _, err := s.DeployTemplateIn(ctx, id, "postgres-app", "shop", "fsn"); err != nil {
		t.Fatalf("DeployTemplateIn: %v", err)
	}
	for _, name := range []string{"shop-db", "shop-web"} {
		if a := mustGet(t, s, id, name); a.Region != "fsn" {
			t.Errorf("%s is in %q, want fsn", name, a.Region)
		}
	}
	if v := mustGet(t, s, id, "shop-db").Volumes; v[0].Class != "replicated-fsn" {
		t.Errorf("the stack's database volume has class %q", v[0].Class)
	}
	if _, err := s.DeployTemplateIn(ctx, id, "postgres-app", "other", "nyc"); !errors.Is(err, ErrNoSuchRegion) {
		t.Fatalf("a stack in an unknown region = %v, want ErrNoSuchRegion", err)
	}
	if n, _ := s.Count(ctx, id); n != 2 {
		t.Fatalf("a refused stack left %d apps, want 2", n)
	}
}

// An app without storage moves: it is versioned, and the reconciler applies
// it in the new region. One with a volume is refused and stays put.
func TestMovingAnAppToAnotherRegion(t *testing.T) {
	ctx := context.Background()
	var (
		mu      sync.Mutex
		changes []RouteChange
	)
	s, orch, id := regionService(t, Options{
		Keeper: testKeeper(t),
		Routes: RouteObserverFunc(func(_ context.Context, c RouteChange) {
			mu.Lock()
			changes = append(changes, c)
			mu.Unlock()
		}),
	})

	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 2, Port: 8080, Region: "fsn",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := orch.applyCalls.Load()
	if err := s.MoveApp(ctx, id, "web", "dar"); err != nil {
		t.Fatalf("MoveApp: %v", err)
	}
	if got := orch.applyCalls.Load(); got != before {
		t.Fatalf("the request applied %d workloads; the reconciler should", got-before)
	}
	moved := mustGet(t, s, id, "web")
	if moved.Region != "dar" || moved.ConfigVersion <= a.ConfigVersion {
		t.Fatalf("moved app is in %q at version %d, want dar past %d", moved.Region, moved.ConfigVersion, a.ConfigVersion)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Name != "web" || spec.Region != "dar" || spec.Replicas != 2 {
		t.Fatalf("reconciled %s in %q x%d, want web in dar x2", spec.Name, spec.Region, spec.Replicas)
	}
	// Moving to where it is does nothing.
	if err := s.MoveApp(ctx, id, "web", "dar"); err != nil {
		t.Fatalf("MoveApp to its own region: %v", err)
	}
	if got := mustGet(t, s, id, "web").ConfigVersion; got != moved.ConfigVersion {
		t.Fatalf("moving to its own region versioned it %d -> %d", moved.ConfigVersion, got)
	}

	// Closed and unknown targets are refused.
	if err := s.SetRegion(ctx, Region{Name: "fsn", Open: false}); err != nil {
		t.Fatalf("close fsn: %v", err)
	}
	if err := s.MoveApp(ctx, id, "web", "fsn"); !errors.Is(err, ErrRegionClosed) {
		t.Fatalf("a move into a closed region = %v, want ErrRegionClosed", err)
	}
	if err := s.MoveApp(ctx, id, "web", "nyc"); !errors.Is(err, ErrNoSuchRegion) {
		t.Fatalf("a move into an unknown region = %v, want ErrNoSuchRegion", err)
	}
	if err := s.SetRegion(ctx, Region{Name: "fsn", Open: true}); err != nil {
		t.Fatalf("open fsn: %v", err)
	}

	// Storage keeps an app where it is.
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "db", Region: "fsn"}); err != nil {
		t.Fatalf("Create postgres: %v", err)
	}
	version := mustGet(t, s, id, "db").ConfigVersion
	err = s.MoveApp(ctx, id, "db", "dar")
	if !errors.Is(err, ErrMoveHasStorage) || !strings.Contains(err.Error(), "volumes live on that region's machines") {
		t.Fatalf("moving a database = %v, want ErrMoveHasStorage saying why", err)
	}
	if db := mustGet(t, s, id, "db"); db.Region != "fsn" || db.ConfigVersion != version {
		t.Fatalf("a refused move left the database in %q at %d", db.Region, db.ConfigVersion)
	}

	s.routesWG.Wait()
	mu.Lock()
	defer mu.Unlock()
	var moves int
	for _, c := range changes {
		if c.Kind == RouteChangeApp && c.AppID == a.ID {
			moves++
		}
	}
	if moves < 2 {
		t.Fatalf("route changes for web = %d in %+v, want its create and its move", moves, changes)
	}
}

// Admission holds a change to the room in its own region: a full region
// refuses, whatever room the others have.
func TestAdmissionIsHeldToTheRegion(t *testing.T) {
	ctx := context.Background()
	s, _, id := regionService(t, Options{})
	t.Cleanup(func() {
		s.refusals.Wait()
		if err := s.SetCapacityPolicy(context.Background(), DefaultCapacityPolicy()); err != nil {
			t.Errorf("restore the capacity policy: %v", err)
		}
	})
	enforce(t, s, true)

	// fsn's one core holds 600m and no more; dar's holds the same.
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "a", Image: "nginx:1.27", Replicas: 1, CPULimit: "600m", MemoryLimit: "128Mi", Region: "fsn",
	}); err != nil {
		t.Fatalf("a create inside fsn's room: %v", err)
	}
	_, err := s.Create(ctx, id, CreateInput{
		Name: "b", Image: "nginx:1.27", Replicas: 1, CPULimit: "600m", MemoryLimit: "128Mi", Region: "fsn",
	})
	wantCapacityRefusal(t, "600m more in a region with 400m free", err, "0.2 vCPU more CPU")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "b", Image: "nginx:1.27", Replicas: 1, CPULimit: "600m", MemoryLimit: "128Mi", Region: "dar",
	}); err != nil {
		t.Fatalf("the same app in dar, which has the room: %v", err)
	}
	// Scaling counts in the app's own region.
	_, err = s.Scale(ctx, id, "b", 2)
	wantCapacityRefusal(t, "a second 600m replica in dar", err, "CPU")

	// A move needs the room where it is going.
	err = s.MoveApp(ctx, id, "b", "fsn")
	wantCapacityRefusal(t, "a move into a full fsn", err, "CPU")
	if b := mustGet(t, s, id, "b"); b.Region != "dar" {
		t.Fatalf("a refused move left the app in %q", b.Region)
	}

	snap, err := s.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	byName := map[string]RegionCapacity{}
	for _, rc := range snap.Regions {
		byName[rc.Region] = rc
	}
	fsn, dar := byName["fsn"], byName["dar"]
	if fsn.Label != "Falkenstein" || fsn.Nodes != 1 || fsn.CPU.Allocatable != 1000 || fsn.CPU.Committed != 600 {
		t.Fatalf("fsn's capacity = %+v, want its one core and the 600m in it", fsn)
	}
	if dar.CPU.Committed != 600 || !dar.Open {
		t.Fatalf("dar's capacity = %+v, want 600m committed and open", dar)
	}
}

// Apps made before regions are recorded where they run, without anything
// restarting; an app with storage that is not running waits.
func TestAppsMadeBeforeRegionsAreRecordedWhereTheyRun(t *testing.T) {
	ctx := context.Background()
	var (
		mu      sync.Mutex
		changes []RouteChange
	)
	s, orch, id := convergenceService(t, Options{
		Keeper: testKeeper(t),
		Routes: RouteObserverFunc(func(_ context.Context, c RouteChange) {
			mu.Lock()
			changes = append(changes, c)
			mu.Unlock()
		}),
	})
	clearRegions(t, s)
	t.Cleanup(func() {
		s.routesWG.Wait()
		clearRegions(t, s)
	})
	unlabelled := twoRegions()
	for i := range unlabelled {
		unlabelled[i].Region = ""
	}
	setNodes(s, unlabelled)

	for _, in := range []CreateInput{
		{Name: "running", Image: "nginx:1.27", Replicas: 1, Port: 8080},
		{Name: "stopped", Image: "nginx:1.27", Replicas: 0},
		{Name: "data", Source: SourcePostgres},
		{Name: "data-running", Source: SourcePostgres},
	} {
		if _, err := createAndDeploy(t, s, ctx, id, in); err != nil {
			t.Fatalf("Create %s: %v", in.Name, err)
		}
	}
	versions := map[string]int64{}
	for _, name := range []string{"running", "stopped", "data", "data-running"} {
		a := mustGet(t, s, id, name)
		if a.Region != "" {
			t.Fatalf("%s was placed in %q before the install had regions", name, a.Region)
		}
		versions[name] = a.ConfigVersion
	}

	// The machines are labelled, and the default is named.
	setNodes(s, twoRegions())
	s.opts.DefaultRegion = "fsn"
	ns := func(name string) string { return mustGet(t, s, id, name).Namespace }
	pods := &podsOrchestrator{convergenceOrchestrator: orch, pods: []orchestrator.PodInfo{
		{Name: "running-1", Namespace: ns("running"), App: "running", Node: "dar-1", Phase: "Running"},
		{Name: "data-running-1", Namespace: ns("data-running"), App: "data-running", Node: "dar-1", Phase: "Running"},
	}}
	s.orch = pods

	n, err := s.BackfillRegions(ctx)
	if err != nil {
		t.Fatalf("BackfillRegions: %v", err)
	}
	want := map[string]string{
		"running":      "dar", // where its pod is, not the default
		"data-running": "dar", // its volume is wherever it is running
		"stopped":      "fsn", // nothing running and nothing stored: the default
		"data":         "",    // storage and not running: nobody can say where
	}
	for name, region := range want {
		a := mustGet(t, s, id, name)
		if a.Region != region {
			t.Errorf("%s recorded in %q, want %q", name, a.Region, region)
		}
		if a.ConfigVersion != versions[name] {
			t.Errorf("recording %s versioned it %d -> %d; nothing should restart", name, versions[name], a.ConfigVersion)
		}
	}
	if n < 3 {
		t.Fatalf("BackfillRegions recorded %d, want at least this test's 3", n)
	}
	// The default region is given a row, open, so new apps can go there.
	if c, err := s.RegionChoice(ctx); err != nil || c.Default != "fsn" {
		t.Fatalf("RegionChoice = %+v, %v; want the default fsn offered", c, err)
	}
	// What is recorded is where AppRegion says; what is not, where it will be.
	if r, _ := s.AppRegion(ctx, id, "data"); r != "fsn" {
		t.Errorf("an unrecorded app is taken to be in %q, want the default", r)
	}

	// Recorded once: a second pass finds nothing of this test's to record,
	// and an app with a recorded region keeps it.
	if _, err := s.BackfillRegions(ctx); err != nil {
		t.Fatalf("second BackfillRegions: %v", err)
	}
	if a := mustGet(t, s, id, "running"); a.Region != "dar" {
		t.Fatalf("a second pass moved running to %q", a.Region)
	}

	s.routesWG.Wait()
	mu.Lock()
	defer mu.Unlock()
	recorded := 0
	for _, c := range changes {
		if c.Kind == RouteChangeApp && c.OwnerID == id &&
			(c.App == "running" || c.App == "stopped" || c.App == "data-running") {
			recorded++
		}
	}
	// Each create announced itself, and each recording again.
	if recorded < 6 {
		t.Fatalf("route changes = %+v, want each recording announced", changes)
	}
}

// podsOrchestrator answers Pods with a fixed list, where the Noop's pods
// are on no machine at all.
type podsOrchestrator struct {
	*convergenceOrchestrator
	pods []orchestrator.PodInfo
}

func (p *podsOrchestrator) Pods(context.Context, orchestrator.PodListOptions) ([]orchestrator.PodInfo, error) {
	return p.pods, nil
}

// Routes are every hostname with its app's region and that region's edge,
// and the observer hears of each change that moves one.
func TestRoutesCarryTheRegionAndItsEdge(t *testing.T) {
	ctx := context.Background()
	var (
		mu      sync.Mutex
		changes []RouteChange
	)
	s, _, id := regionService(t, Options{
		AppDomain: "apps.test",
		Routes: RouteObserverFunc(func(_ context.Context, c RouteChange) {
			mu.Lock()
			changes = append(changes, c)
			mu.Unlock()
		}),
	})
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO platform_dns (id, cname_target) VALUES (1, 'edge.example.test')
		ON CONFLICT (id) DO UPDATE SET cname_target = excluded.cname_target`); err != nil {
		t.Fatalf("set the install's target: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM platform_dns`); err != nil {
			t.Errorf("clear the install's target: %v", err)
		}
	})

	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080, Region: "fsn",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "api", Image: "nginx:1.27", Replicas: 1, Port: 8080, Region: "dar",
	}); err != nil {
		t.Fatalf("Create api: %v", err)
	}

	// A custom domain on an app in fsn is told to point at fsn's edge, and
	// one in dar, which has none, at the install's.
	n, err := s.Networking(ctx, id, "web")
	if err != nil || n.Target != "edge-fsn.example.test" {
		t.Fatalf("web's custom domain target = %q, %v; want fsn's edge", n.Target, err)
	}
	if err := s.AddDomain(ctx, id, "web", "shop.example.test"); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	custom, err := domain.ListCustom(ctx, s.q, id, a.ID)
	if err != nil || len(custom) != 1 || custom[0].Target != "edge-fsn.example.test" {
		t.Fatalf("the claim records target %+v, %v; want fsn's edge", custom, err)
	}
	if n, _ := s.Networking(ctx, id, "api"); n.Target != "edge.example.test" {
		t.Fatalf("api's target = %q, want the install's", n.Target)
	}

	routes, err := s.Routes(ctx)
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	got := map[string]Route{}
	for _, r := range routes {
		if r.OwnerID == id {
			got[r.Host] = r
		}
	}
	web, shop, api := got["web.apps.test"], got["shop.example.test"], got["api.apps.test"]
	if web.Region != "fsn" || web.Custom || !web.Routable || web.App != "web" ||
		web.Edge.Host != "edge-fsn.example.test" || !slices.Equal(web.Edge.Addresses, []string{"203.0.113.10"}) {
		t.Errorf("web's route = %+v", web)
	}
	if shop.Region != "fsn" || !shop.Custom || shop.Routable || shop.AppID != a.ID {
		t.Errorf("the unproven custom domain's route = %+v", shop)
	}
	if api.Region != "dar" || api.Edge.Set() {
		t.Errorf("api's route = %+v, want dar with no edge of its own", api)
	}
	if len(got) != 3 {
		t.Errorf("routes = %+v, want three", got)
	}

	// A region's edge changing is announced; saving it unchanged is not.
	s.routesWG.Wait()
	mu.Lock()
	changes = nil
	mu.Unlock()
	if err := s.SetRegion(ctx, Region{Name: "dar", Open: true, Edge: Edge{Host: "Edge-Dar.Example.Test."}}); err != nil {
		t.Fatalf("SetRegion: %v", err)
	}
	if err := s.SetRegion(ctx, Region{Name: "dar", Open: true, Edge: Edge{Host: "edge-dar.example.test"}}); err != nil {
		t.Fatalf("SetRegion unchanged: %v", err)
	}
	s.routesWG.Wait()
	mu.Lock()
	if len(changes) != 1 || changes[0].Kind != RouteChangeRegion || changes[0].Region != "dar" {
		t.Fatalf("changes = %+v, want one region change for dar", changes)
	}
	mu.Unlock()
	if r, _ := s.Region(ctx, "dar"); r.Edge.Host != "edge-dar.example.test" {
		t.Fatalf("dar's edge = %q, want it lowercased without the trailing dot", r.Edge.Host)
	}

	// A panicking observer takes nothing down with it.
	s.opts.Routes = RouteObserverFunc(func(context.Context, RouteChange) { panic("observer") })
	if err := s.MoveApp(ctx, id, "api", "fsn"); err != nil {
		t.Fatalf("MoveApp with a panicking observer: %v", err)
	}
	s.routesWG.Wait()
}

// Every field of a region ends somewhere strict, so each is checked.
func TestARegionIsValidated(t *testing.T) {
	ctx := context.Background()
	s, _, _ := regionService(t, Options{})
	for _, r := range []Region{
		{Name: ""},
		{Name: "FSN"},
		{Name: "fsn;id"},
		{Name: "fsn", DisplayName: strings.Repeat("x", 81)},
		{Name: "fsn", DisplayName: "two\nlines"},
		{Name: "fsn", Edge: Edge{Host: "https://edge.example.test"}},
		{Name: "fsn", Edge: Edge{Host: "edge.example.test:443"}},
		{Name: "fsn", Edge: Edge{Host: "localhost"}},
		{Name: "fsn", Edge: Edge{Addresses: []string{"203.0.113.007"}}},
		{Name: "fsn", Edge: Edge{Addresses: []string{"edge.example.test"}}},
		{Name: "fsn", StorageClass: "Longhorn"},
		{Name: "fsn", StorageClass: "longhorn fsn"},
	} {
		if err := s.SetRegion(ctx, r); err == nil {
			t.Errorf("SetRegion(%+v) was accepted", r)
		}
	}
	ok := Region{Name: "fsn", DisplayName: "Falkenstein", Open: true, StorageClass: "longhorn-fsn",
		Edge: Edge{Host: "edge-fsn.example.test", Addresses: []string{" 2001:db8::10 ", "203.0.113.10", "203.0.113.10"}}}
	if err := s.SetRegion(ctx, ok); err != nil {
		t.Fatalf("SetRegion: %v", err)
	}
	r, err := s.Region(ctx, "fsn")
	if err != nil {
		t.Fatalf("Region: %v", err)
	}
	if !r.Configured || r.Label() != "Falkenstein" || r.Nodes != 1 || r.Ready != 1 ||
		!slices.Equal(r.Sites, []string{"fsn-a"}) || !slices.Equal(r.Edge.Addresses, []string{"2001:db8::10", "203.0.113.10"}) {
		t.Fatalf("Region = %+v", r)
	}

	// A region a machine carries and nobody configured is listed, and not
	// open; one configured with no machine is listed, and cannot be chosen.
	setNodes(s, append(twoRegions(), orchestrator.NodeInfo{Name: "ams-1", Ready: true, Region: "ams"}))
	if _, err := s.pool.Exec(ctx, `DELETE FROM regions WHERE name = 'dar'`); err != nil {
		t.Fatal(err)
	}
	setNodes(s, []orchestrator.NodeInfo{twoRegions()[0], {Name: "ams-1", Ready: true, Region: "ams"}})
	if err := s.SetRegion(ctx, Region{Name: "dar", Open: true}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Regions(ctx)
	if err != nil {
		t.Fatalf("Regions: %v", err)
	}
	byName := map[string]RegionInfo{}
	for _, r := range all {
		byName[r.Name] = r
	}
	if ams := byName["ams"]; ams.Configured || ams.Choosable || ams.Nodes != 1 {
		t.Errorf("an unconfigured region = %+v", ams)
	}
	if dar := byName["dar"]; !dar.Configured || dar.Choosable || dar.Nodes != 0 {
		t.Errorf("a region with no machines = %+v", dar)
	}
	if fsn := byName["fsn"]; !fsn.Choosable || !fsn.Default {
		t.Errorf("fsn = %+v, want the only open region, and so the default", fsn)
	}
	if _, err := s.Region(ctx, "nyc"); !errors.Is(err, ErrNoSuchRegion) {
		t.Errorf("Region(nyc) = %v, want ErrNoSuchRegion", err)
	}
}

// A sleeping app stays in its region while it sleeps, wakes there, and is
// refused a wake its region has no room for — whatever room is elsewhere.
func TestASleeperWakesInItsOwnRegion(t *testing.T) {
	ctx := context.Background()
	s, orch, pool := sleepService(t, Options{Waker: testWaker})
	clearRegions(t, s)
	t.Cleanup(func() {
		s.refusals.Wait()
		clearRegions(t, s)
		if err := s.SetCapacityPolicy(context.Background(), DefaultCapacityPolicy()); err != nil {
			t.Errorf("restore the capacity policy: %v", err)
		}
	})
	setNodes(s, twoRegions())
	for _, name := range []string{"fsn", "dar"} {
		if err := s.SetRegion(ctx, Region{Name: name, Open: true}); err != nil {
			t.Fatalf("SetRegion: %v", err)
		}
	}
	id := owner(t, s, pool, "sleep-region")
	a, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "sleepy-fsn", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		CPULimit: "500m", MemoryLimit: "256Mi", Region: "fsn",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SleepNow(ctx, id, a.Name); err != nil {
		t.Fatalf("SleepNow: %v", err)
	}
	if err := s.ReconcileApps(ctx); err != nil {
		t.Fatalf("ReconcileApps: %v", err)
	}
	if spec := orch.lastAppSpec(); spec.Name != a.Name || spec.Replicas != 0 || spec.Region != "fsn" {
		t.Fatalf("asleep, applied %s x%d in %q; want it at zero, still in fsn", spec.Name, spec.Replicas, spec.Region)
	}

	// fsn is filled by an app of another team's; dar is empty.
	other := owner(t, s, pool, "sleep-region-other")
	if _, err := createAndDeploy(t, s, ctx, other, CreateInput{
		Name: "hog", Image: "nginx:alpine", Replicas: 1, CPULimit: "800m", MemoryLimit: "256Mi", Region: "fsn",
	}); err != nil {
		t.Fatalf("Create the hog: %v", err)
	}
	enforce(t, s, true)
	if err := s.WakeForRequest(ctx, mustGet(t, s, id, a.Name)); !errors.Is(err, ErrWakeNoRoom) {
		t.Fatalf("a wake into a full fsn = %v, want ErrWakeNoRoom though dar is empty", err)
	}
	if got := mustGet(t, s, id, a.Name); got.Sleep.State != SleepAsleep {
		t.Fatalf("after a refused wake the app is %s", got.Sleep.State)
	}
}
