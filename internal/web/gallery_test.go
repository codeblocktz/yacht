package web

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A gallery of every visual state the dashboard can be in.
//
// Two jobs. It renders documentation images without needing a cluster or a
// database, and it is the scaffolding for visual regression: the states that
// are hardest to reach by clicking — a degraded workload, a failed deploy, a
// node at 95% — are exactly the ones that silently rot, because nobody sees
// them until a customer does.
//
// Writes files only when YACHT_GALLERY_OUT is set, so the normal test run stays
// a normal test run:
//
//	YACHT_GALLERY_OUT=/tmp/gallery go test ./internal/web -run Gallery
func TestGallery(t *testing.T) {
	out := os.Getenv("YACHT_GALLERY_OUT")
	if out == "" {
		t.Skip("set YACHT_GALLERY_OUT to write gallery HTML")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for _, g := range galleryPages() {
		path := filepath.Join(out, g.file)
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		slots := DefaultSlots{}.Slots(context.Background(),
			httptest.NewRequest("GET", g.path, nil))
		slots.Breadcrumb = g.crumbs
		if err := Layout(slots, g.page).Render(context.Background(), f); err != nil {
			f.Close()
			t.Fatalf("render %s: %v", g.file, err)
		}
		f.Close()
		t.Logf("wrote %s", path)
	}
}

type galleryPage struct {
	file   string
	path   string
	crumbs []Crumb
	page   templ.Component
}

// section renders a labelled band so several states can share one image.
func section(title, note string, body templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		fmt.Fprintf(w, `<div class="mb-8"><div class="mb-2 flex items-baseline gap-3">`+
			`<span class="micro">%s</span>`+
			`<span class="text-[11.5px] text-muted-foreground">%s</span></div>`,
			templ.EscapeString(title), templ.EscapeString(note))
		if err := body.Render(ctx, w); err != nil {
			return err
		}
		_, err := w.Write([]byte(`</div>`))
		return err
	})
}

func stack(parts ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		for _, p := range parts {
			if err := p.Render(ctx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

func galleryPages() []galleryPage {
	now := time.Now()

	mk := func(name, image string, replicas, ready int32, phase orchestrator.Phase,
		known bool, msg string) app.App {
		return app.App{
			ID: uuid.New(), OwnerID: "owner-local", Name: name,
			Namespace: app.Namespace("owner-local", name),
			Image:     image, Replicas: replicas, Port: 8080,
			Variables: varsFrom(map[string]string{"LOG_LEVEL": "info", "NODE_ENV": "production"}),
			CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-11 * time.Minute),
			StatusKnown: known,
			Status: orchestrator.AppStatus{
				Phase: phase, Desired: replicas, Ready: ready,
				Available: ready, Message: msg,
			},
		}
	}

	running := mk("web", "nginx:alpine", 2, 2, orchestrator.PhaseRunning, true, "")
	degraded := mk("api", "ghcr.io/codeblocktz/api:v2.4.1", 3, 1,
		orchestrator.PhaseDegraded, true, "1/3 replicas ready")
	pending := mk("worker", "ghcr.io/codeblocktz/worker:v1", 2, 0,
		orchestrator.PhasePending, true, "")
	stopped := mk("batch", "ghcr.io/codeblocktz/batch:v3", 0, 0,
		orchestrator.PhaseStopped, true, "")
	unknown := mk("cache", "valkey/valkey:8-alpine", 1, 0, "", false, "")
	failing := mk("mailer", "ghcr.io/codeblocktz/mailer:v9", 1, 0,
		orchestrator.PhasePending, true,
		`Back-off pulling image "ghcr.io/codeblocktz/mailer:v9": ErrImagePull`)

	// Hostnames on some apps and not others, over both schemes: an install
	// without wildcard TLS is a state that must look right too.
	running.Host, running.TLS = "web.apps.example.com", true
	degraded.Host = "api.apps.example.com"

	allApps := []app.App{running, degraded, pending, stopped, unknown, failing}

	pods := []orchestrator.PodInfo{
		{Name: "web-7d9f4b6c85-2xk9p", Namespace: "yacht-a1b2", Phase: "Running",
			Node: "yacht-cp", Ready: 1, Total: 1, CreatedAt: now.Add(-4 * time.Hour)},
		{Name: "api-5c8b7d94f6-hq2mz", Namespace: "yacht-c3d4", Phase: "Running",
			Node: "yacht-w1", Ready: 1, Total: 2, Restarts: 4, CreatedAt: now.Add(-time.Hour)},
		{Name: "worker-64b9c7f8d2-pl4vn", Namespace: "yacht-e5f6", Phase: "Pending",
			Node: "", Ready: 0, Total: 1, CreatedAt: now.Add(-90 * time.Second)},
		{Name: "mailer-8f7d6c5b4a-zz91k", Namespace: "yacht-0718", Phase: "Failed",
			Node: "yacht-w1", Ready: 0, Total: 1, Restarts: 12, CreatedAt: now.Add(-20 * time.Minute)},
	}

	nodes := []orchestrator.NodeInfo{
		{
			Name: "yacht-cp", Ready: true, Roles: []string{"control-plane", "master"},
			Address: "10.0.0.4", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "arm64",
			CPUCapacityMillis: 4000, CPUUsedMillis: 760,
			MemCapacityBytes: 8 << 30, MemUsedBytes: 3<<30 + 512<<20,
			Pods: 23, PodCapacity: 110, UsageKnown: true, Pool: "system",
		},
		{
			Name: "yacht-w1", Ready: true, Roles: []string{"worker"},
			Address: "10.0.0.5", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "arm64",
			CPUCapacityMillis: 2000, CPUUsedMillis: 1640,
			MemCapacityBytes: 4 << 30, MemUsedBytes: 3<<30 + 300<<20,
			Pods: 31, PodCapacity: 110, UsageKnown: true, Pool: "apps",
		},
		{
			Name: "yacht-w2", Ready: false, Roles: []string{"worker"},
			Address: "10.0.0.6", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "amd64",
			CPUCapacityMillis: 2000, CPUUsedMillis: 1960,
			MemCapacityBytes: 4 << 30, MemUsedBytes: 3<<30 + 900<<20,
			Pods: 8, PodCapacity: 110, UsageKnown: true, Unschedulable: true,
		},
	}

	summary := orchestrator.ClusterSummary{
		Nodes: 3, NodesReady: 2, Pods: 62, PodCapacity: 330,
		CPUUsedMillis: 4360, CPUCapacityMillis: 8000,
		MemUsedBytes: 10<<30 + 700<<20, MemCapacityBytes: 16 << 30,
		Volumes: 13, UsageKnown: true,
	}

	deployments := []app.Deployment{
		{ID: uuid.New(), Image: "nginx:alpine", Revision: "initial", Status: "running",
			StartedAt: now.Add(-12 * time.Minute)},
		{ID: uuid.New(), Image: "nginx:1.27", Revision: "redeploy", Status: "succeeded",
			StartedAt: now.Add(-6 * 24 * time.Hour)},
		{ID: uuid.New(), Image: "nginx:1.26", Revision: "scale:4", Status: "failed",
			Message: "readiness probe never passed", StartedAt: now.Add(-7 * 24 * time.Hour)},
		{ID: uuid.New(), Image: "nginx:1.25", Revision: "redeploy", Status: "cancelled",
			StartedAt: now.Add(-21 * 24 * time.Hour)},
	}

	return []galleryPage{
		{
			file: "states-apps.html", path: "/apps",
			crumbs: []Crumb{{Label: "Apps"}},
			page: stack(
				section("App states", "running · degraded · pending · stopped · unknown · image pull failure",
					panelWrap(appRows(allApps))),
				section("Empty state", "no workloads yet", panelWrap(emptyApps())),
				section("Cluster unreachable", "records still render; live status does not",
					clusterCallout(`Get "https://10.0.0.4:6443/version": dial tcp: i/o timeout`)),
			),
		},
		{
			file: "states-detail.html", path: "/apps/web",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "web"}},
			page: AppDetail(AppDetailData{
				App: running, Siblings: allApps, Tab: "",
				Pods: pods[:2], Deployments: deployments,
			}),
		},
		{
			file: "states-detail-degraded.html", path: "/apps/api",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "api"}},
			page: AppDetail(AppDetailData{
				App: degraded, Siblings: allApps, Tab: "metrics", Pods: pods[1:3],
				Deployments: deployments[2:],
			}),
		},
		{
			file: "states-cluster.html", path: "/cluster/nodes",
			crumbs: []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "Cluster"}},
			page: Cluster(ClusterData{
				Tab: "nodes", OK: true, Summary: summary, Nodes: nodes,
			}),
		},
		{
			file: "states-pods.html", path: "/cluster/pods",
			crumbs: []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "Cluster"}},
			page: Cluster(ClusterData{
				Tab: "pods", OK: true, Summary: summary, Pods: pods,
			}),
		},
		{
			file: "states-form.html", path: "/apps/new",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "New"}},
			page: NewApp(NewAppData{
				Error: `app: name already in use`,
				Form: NewAppForm{
					Name: "web", Image: "nginx:alpine", Port: "8080", Replicas: "2",
					Env: "LOG_LEVEL=info\nNODE_ENV=production",
				},
			}),
		},
		{
			file: "states-settings.html", path: "/settings",
			crumbs: []Crumb{{Label: "Settings"}},
			page: Settings(SettingsData{
				OwnerID: "owner-local", OwnerName: "Eric", Authenticated: true,
				Version: "v0.1.0", ClusterOK: true,
				AppDomain: "apps.example.com", WildcardTLS: true,
			}),
		},
		{
			file: "states-sign-in.html", path: "/sign-in",
			page: stack(
				section("Sign in", "the form, and the same form after a rejected address",
					stack(
						SignIn(SignInData{}),
						SignIn(SignInData{
							Email: "not an address",
							Error: "That does not look like an email address.",
						}),
					)),
				section("Check your mail", "the same page whether or not the address is registered",
					CheckMail()),
			),
		},
		{
			file: "states-overview.html", path: "/",
			crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(OverviewData{
				OwnerName: "Eric", ClusterOK: true, AppCount: 6,
				Summary: summary, Apps: allApps,
			}),
		},
	}
}

// panelWrap puts a component inside the standard panel chrome, so gallery
// sections match how the component appears in the product.
func panelWrap(inner templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := w.Write([]byte(`<div class="panel">`)); err != nil {
			return err
		}
		if err := inner.Render(ctx, w); err != nil {
			return err
		}
		_, err := w.Write([]byte(`</div>`))
		return err
	})
}
