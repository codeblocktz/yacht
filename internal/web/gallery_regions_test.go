package web

import (
	"time"

	"github.com/a-h/templ"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/cluster"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The states regions add. Reaching any of them by clicking means machines
// in two places with labels on them, which is why they are drawn here.

// galleryRegionInfos is an install in two regions, fsn and dar, and a third
// machine carrying ams that nobody has configured.
func galleryRegionInfos() []app.RegionInfo {
	return []app.RegionInfo{
		{
			Region: app.Region{Name: "ams"},
			Nodes:  1, Ready: 1, Sites: []string{"ams-a"}, Apps: 0,
		},
		{
			Region: app.Region{Name: "dar", DisplayName: "South", Open: true,
				Edge: app.Edge{Host: "edge-dar.example.test", Addresses: []string{"198.51.100.40"}}},
			Configured: true, Nodes: 2, Ready: 2, Sites: []string{"dar-a", "dar-b"}, Apps: 6, Choosable: true,
		},
		{
			Region: app.Region{Name: "fsn", DisplayName: "North", Open: true, StorageClass: "replicated-fsn",
				Edge: app.Edge{Host: "edge-fsn.example.test", Addresses: []string{"203.0.113.10", "2001:db8::10"}}},
			Configured: true, Nodes: 3, Ready: 2, Sites: []string{"fsn-a"}, Apps: 14,
			Default: true, Choosable: true,
		},
	}
}

func galleryRegionChoice() app.RegionChoice {
	all := galleryRegionInfos()
	return app.RegionChoice{InUse: true, Default: "fsn", Options: []app.RegionInfo{all[1], all[2]}}
}

// regionNodes is the gallery's machines across two regions, with one that
// has not been given a region yet.
func regionNodes(nodes []orchestrator.NodeInfo) []orchestrator.NodeInfo {
	out := sitedNodes(nodes)
	out[0].Region, out[0].Site = "fsn", "fsn-a"
	out[1].Region, out[1].Site = "dar", "dar-a"
	out = append(out, orchestrator.NodeInfo{
		Name: "yacht-dar-2", Ready: true, Roles: []string{"worker"}, Region: "dar", Site: "dar-b",
		Address: "198.51.100.25", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "amd64",
		CPUCapacityMillis: 4000, CPUUsedMillis: 900, MemCapacityBytes: 8 << 30, MemUsedBytes: 2 << 30,
		Pods: 14, PodCapacity: 110, UsageKnown: true,
	})
	return out
}

// regionCapacity is the gallery's install under its policy, region by region:
// fsn nearly full, dar with room.
func regionCapacity(nodes []orchestrator.NodeInfo, now time.Time) app.CapacitySnapshot {
	p := galleryPolicy(2, 10)
	snap := galleryCapacity(nodes, now, p, 1)
	regional := p
	regional.StorageBytes = 0
	in := func(region string) []orchestrator.NodeInfo {
		var out []orchestrator.NodeInfo
		for _, n := range nodes {
			if n.Region == region {
				out = append(out, n)
			}
		}
		return out
	}
	fsn := app.NewCapacitySnapshot(regional, app.RoomOf(in("fsn")),
		app.Usage{Apps: 14, CPUMillis: 6900, MemoryBytes: 6 << 30, StorageBytes: 40 << 30}, 0)
	dar := app.NewCapacitySnapshot(regional, app.RoomOf(in("dar")),
		app.Usage{Apps: 6, CPUMillis: 2100, MemoryBytes: 3 << 30, StorageBytes: 12 << 30}, 0)
	snap.Regions = []app.RegionCapacity{
		{Region: "dar", Label: "South", Open: true, CapacitySnapshot: dar},
		{Region: "fsn", Label: "North", Open: true, CapacitySnapshot: fsn},
	}
	return snap
}

func regionGalleryPages() []galleryPage {
	now := time.Now()

	nodes := regionNodes([]orchestrator.NodeInfo{
		{
			Name: "yacht-cp", Ready: true, Roles: []string{"control-plane", "master"},
			Address: "203.0.113.10", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "arm64",
			CPUCapacityMillis: 4000, CPUUsedMillis: 3100, MemCapacityBytes: 8 << 30, MemUsedBytes: 6 << 30,
			Pods: 41, PodCapacity: 110, UsageKnown: true,
		},
		{
			Name: "yacht-dar-1", Ready: true, Roles: []string{"worker"},
			Address: "198.51.100.24", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "amd64",
			CPUCapacityMillis: 4000, CPUUsedMillis: 1200, MemCapacityBytes: 8 << 30, MemUsedBytes: 3 << 30,
			Pods: 22, PodCapacity: 110, UsageKnown: true,
		},
		{
			Name: "yacht-fsn-2", Ready: false, Roles: []string{"worker"}, Unschedulable: true,
			Address: "203.0.113.11", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "amd64",
			CPUCapacityMillis: 2000, MemCapacityBytes: 4 << 30, Pods: 3, PodCapacity: 110,
		},
	})
	nodes[2].Region, nodes[2].Site = "fsn", "fsn-a"
	summary := orchestrator.ClusterSummary{
		Nodes: 4, NodesReady: 3, Pods: 80, PodCapacity: 440,
		CPUUsedMillis: 5200, CPUCapacityMillis: 14000,
		MemUsedBytes: 11 << 30, MemCapacityBytes: 28 << 30, Volumes: 9, UsageKnown: true,
	}

	web := app.App{
		ID: uuid.New(), OwnerID: "owner-local", Name: "web", Namespace: app.Namespace("owner-local", "web"),
		Image: "nginx:alpine", Replicas: 2, Port: 8080, Host: "web.apps.example.com", TLS: true,
		Region: "fsn", StatusKnown: true, CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-time.Hour),
		Status: orchestrator.AppStatus{Phase: orchestrator.PhaseRunning, Desired: 2, Ready: 2, Available: 2},
	}
	api := web
	api.ID, api.Name, api.Host, api.Region = uuid.New(), "api", "api.apps.example.com", "dar"
	db := web
	db.ID, db.Name, db.Host, db.Image, db.Replicas = uuid.New(), "db", "", "postgres:17-alpine", 1
	db.Source, db.Internal = app.SourcePostgres, true
	db.Volumes = []app.Volume{{Name: "data", MountPath: "/var/lib/postgresql/data", SizeBytes: 10 << 30, Class: "replicated-fsn"}}
	legacy := web
	legacy.ID, legacy.Name, legacy.Host, legacy.Region = uuid.New(), "legacy", "legacy.apps.example.com", ""

	choice := galleryRegionChoice()
	regionOf := func(a app.App, name string) *AppRegionData {
		r := &AppRegionData{Name: name, Label: name, Recorded: a.Region != "", HasStorage: len(a.Volumes) > 0}
		for _, o := range choice.Options {
			if o.Name == name {
				r.Label = o.Label()
				continue
			}
			r.Options = append(r.Options, o)
		}
		return r
	}
	withRegion := func(a app.App, name string) AppDetailData {
		d := settingsGallery(a, 4000, 8<<30)
		d.Region = regionOf(a, name)
		return d
	}

	return []galleryPage{
		{
			// The deploy form on an install in two regions: the default chosen,
			// the open ones offered, and what happens when none is open.
			file: "states-form-regions.html", path: "/apps/new",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "New"}},
			page: stack(
				section("Two regions open", "the default chosen; only open regions with a machine ready",
					NewApp(NewAppData{
						Source: app.SourceImage, Blueprint: mustBlueprint(app.SourceImage),
						Form:    NewAppForm{Name: "web", Image: "nginx:alpine", Port: "8080", Replicas: "2"},
						Regions: choice,
					})),
				section("Nothing open", "every region closed or without a ready machine",
					regionField("region-closed", app.RegionChoice{InUse: true}, "")),
			),
		},
		{
			// An app's page says where it lives; its settings offer a move,
			// or say why a database stays put.
			file: "states-detail-region.html", path: "/apps/web/settings",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "web"}, {Label: "Settings"}},
			page: stack(
				section("Header", "the region beside the status",
					appDetailHeader(withRegion(web, "fsn"))),
				section("Without storage", "can move to another open region",
					appRegionSection(withRegion(web, "fsn"))),
				section("With storage", "stays, and says why",
					appRegionSection(withRegion(db, "fsn"))),
				section("Made before regions", "taken to be in the default until it is recorded",
					appRegionSection(withRegion(legacy, "fsn"))),
				section("In a list", "each app's region; one not recorded yet shows none",
					panelWrap(appRows([]app.App{web, api, db, legacy}))),
			),
		},
		{
			// Machines in two regions, at their sites, with one not ready.
			file: "states-cluster-regions.html", path: "/cluster/nodes",
			crumbs: []Crumb{{Label: "Admin", Href: "/cluster/nodes"}, {Label: "Cluster"}},
			page:   Cluster(ClusterData{Tab: "nodes", OK: true, Summary: summary, Nodes: nodes}),
		},
		{
			// The operator's regions: two configured, one only carried by a
			// machine, with what each holds and sells; and an install with none.
			file: "states-admin-regions.html", path: "/admin/regions",
			crumbs: []Crumb{{Label: "Admin", Href: "/admin/teams"}, {Label: "Regions"}},
			page: func() templ.Component {
				snap := regionCapacity(nodes, now)
				d := RegionsData{OK: true, CanAddNode: true, Regions: galleryRegionInfos(),
					Capacity: map[string]app.RegionCapacity{}}
				for _, rc := range snap.Regions {
					d.Capacity[rc.Region] = rc
				}
				return stack(
					section("Two regions and an unconfigured one", "", AdminRegions(d)),
					section("No regions", "every machine unlabelled — how to add one",
						AdminRegions(RegionsData{OK: true})),
				)
			}(),
		},
		{
			// One region's settings: configured, and one not configured yet.
			file: "states-admin-region.html", path: "/admin/regions/fsn",
			crumbs: []Crumb{{Label: "Admin", Href: "/admin/teams"}, {Label: "Regions", Href: "/admin/regions"}, {Label: "North"}},
			page: stack(
				section("Configured", "", AdminRegion(regionForm(galleryRegionInfos()[2]))),
				section("Unconfigured, refused", "a scheme in the edge",
					AdminRegion(func() RegionFormData {
						f := regionForm(galleryRegionInfos()[0])
						f.EdgeHost = "https://edge-ams.example.test"
						f.Error = `the edge "https://edge-ams.example.test" is not a hostname — a name like edge-fsn.example.test, with no scheme, port or path`
						return f
					}())),
			),
		},
		{
			// Capacity region by region under a policy: fsn nearly full, dar
			// with room. A change is admitted against its own region.
			file: "states-admin-capacity-regions.html", path: "/admin/capacity",
			crumbs: []Crumb{{Label: "Admin", Href: "/admin/teams"}, {Label: "Capacity"}},
			page: func() templ.Component {
				snap := regionCapacity(nodes, now)
				d := CapacityData{OK: true, CanAddNode: true, Form: capacityPolicyForm(snap.Policy)}
				d.addSnapshot(snap)
				d.addTeams(galleryTeams(now))
				return AdminCapacity(d)
			}(),
		},
		{
			// The join command for a machine in a region, at a site.
			file: "states-node-join-region.html", path: "/cluster/nodes/add",
			crumbs: []Crumb{{Label: "Admin", Href: "/cluster/nodes"}, {Label: "Add node"}},
			page: func() templ.Component {
				m := cluster.Machine{Site: "dar-b", Region: "dar", PublicIP: "198.51.100.25"}
				return AddNode(AddNodeData{
					Settings: cluster.Settings{
						ServerURL: "https://203.0.113.10:6443", TokenSet: true, UpdatedAt: "2026-09-01 09:30",
					},
					Configured: true,
					Command:    cluster.BuildCommand("https://203.0.113.10:6443", "K10c0ffee…::server:5ecret", m),
					Machine:    m,
					ClusterOK:  true,
					Nodes:      nodes,
				})
			}(),
		},
	}
}

// mustBlueprint is a source's blueprint with every capability present.
func mustBlueprint(src app.Source) app.Blueprint {
	b, err := app.BlueprintFor(src)
	if err != nil {
		panic(err)
	}
	return b
}
