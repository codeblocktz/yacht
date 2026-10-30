package k8s

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Opt-in like the rollout test: it runs real builds, which need a scheduler,
// the builder image and a registry, and then runs what they built. A Node app
// and a static site, neither with a Dockerfile, go through the buildpacks
// step and are deployed; each has to come up ready on "/".
//
// The source is an upload served by this test, so the cluster has to reach
// the machine running it, and the build pushes to a plain-HTTP registry that
// the cluster's nodes can pull from under the same name:
//
//	docker run -d --name bp-registry.localhost --network k3d-<cluster> \
//	  -e REGISTRY_HTTP_ADDR=0.0.0.0:5000 registry:2
//	(and a registries.yaml mirroring bp-registry.localhost:5000 over http)
//	YACHT_TEST_K3S=1 YACHT_TEST_BUILD_REGISTRY=bp-registry.localhost:5000 \
//	YACHT_TEST_BUILD_HOST=192.168.5.2 \
//	  go test ./internal/orchestrator/k8s -run K3sBuildpacks -v -timeout 30m
//
// YACHT_TEST_BUILD_HOST is this machine as a pod sees it: 192.168.5.2 under
// colima.
func TestK3sBuildpacksBuildANodeAppAndAStaticSite(t *testing.T) {
	registry := os.Getenv("YACHT_TEST_BUILD_REGISTRY")
	host := os.Getenv("YACHT_TEST_BUILD_HOST")
	if os.Getenv("YACHT_TEST_K3S") == "" || registry == "" || host == "" {
		t.Skip("set YACHT_TEST_K3S=1, YACHT_TEST_BUILD_REGISTRY and YACHT_TEST_BUILD_HOST to build on a disposable K3s cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, err := New(ctx, Config{}, log)
	if err != nil {
		t.Fatalf("connect to K3s: %v", err)
	}

	sources := map[string]map[string]string{
		// No dependencies, and still a lockfile: npm-install runs "npm ci",
		// which writes node_modules and a scratch file into the app
		// directory. That write is what failed.
		"node": {
			"package.json": `{"name":"bp-node","version":"1.0.0","scripts":{"start":"node server.js"}}`,
			"package-lock.json": `{"name":"bp-node","version":"1.0.0","lockfileVersion":3,"requires":true,` +
				`"packages":{"":{"name":"bp-node","version":"1.0.0"}}}`,
			"server.js": `require("http").createServer((q, r) => r.end("node ok\n"))` +
				`.listen(process.env.PORT || 8080)`,
		},
		// The site the finding was about, in miniature: a page and its
		// bundles. Not app.js or index.js, which Paketo's node-start takes
		// for a Node entrypoint.
		"static": {
			"index.html": "<!doctype html><title>bp</title><link rel=stylesheet href=bundle.css>" +
				"<script src=bundle.js></script><p>static ok</p>\n",
			"bundle.js":  "console.log('static')\n",
			"bundle.css": "p{color:teal}\n",
		},
	}
	archives := map[string][]byte{}
	for name, files := range sources {
		archives[name] = tarball(t, files)
	}

	// Listening on every interface, since the request comes from a pod.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer source-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		b, ok := archives[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	ns := fmt.Sprintf("yacht-bp-%d", time.Now().UnixNano())
	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{Owner: "bp-test", Name: ns}); err != nil {
		t.Fatalf("ensure namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if err := o.DeleteNamespace(cleanup, ns); err != nil {
			t.Errorf("delete integration namespace: %v", err)
		}
	})

	for _, name := range []string{"node", "static"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var out strings.Builder
			image := fmt.Sprintf("%s/bp-test/%s:%d", registry, name, time.Now().Unix())
			result, err := o.Build(ctx, orchestrator.BuildRequest{
				Owner: "bp-test", App: name, Image: image, Insecure: true,
				RegistryAuth: []byte(`{"auths":{}}`),
				SourceURL:    fmt.Sprintf("http://%s:%d/%s", host, port, name),
				SourceToken:  "source-token",
				Log: func(s string) {
					mu.Lock()
					defer mu.Unlock()
					out.WriteString(s)
				},
			})
			log := out.String()
			if testing.Verbose() {
				t.Logf("build log:\n%s", lastLines(log, 40))
			}
			if err != nil {
				t.Fatalf("build: %v\n%s", err, lastLines(log, 40))
			}
			if strings.Contains(log, "permission denied") {
				t.Errorf("the build log has a permission error:\n%s", lastLines(log, 40))
			}

			ref := orchestrator.Ref{Owner: "bp-test", Namespace: ns, Name: name}
			if err := o.ApplyApp(ctx, orchestrator.AppSpec{
				Ref: ref, Image: image, Replicas: 1, Port: 8080,
				RunAsUser: result.RunAsUser, HealthPath: "/",
				CPURequest: "50m", MemoryRequest: "64Mi",
			}); err != nil {
				t.Fatalf("apply %s: %v", name, err)
			}
			// Ready means the readiness probe got an answer on "/" at PORT.
			// Its own deadline, so an image that crashes fails here rather
			// than when the whole test runs out.
			ready, stop := context.WithTimeout(ctx, 5*time.Minute)
			defer stop()
			waitAvailable(t, ready, o, ref)
		})
	}
}

// tarball is an upload as the CLI makes one: a gzipped tar of the files.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
