package k8s

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func cloneBuild() orchestrator.BuildRequest {
	return orchestrator.BuildRequest{
		Owner: "owner-1", App: "web", Image: "registry.test/owner-1/web:d1",
		RepoURL: "https://github.com/acme/web.git", Ref: "main",
	}
}

func uploadBuild() orchestrator.BuildRequest {
	return orchestrator.BuildRequest{
		Owner: "owner-1", App: "web", Image: "registry.test/owner-1/web:d1",
		SourceURL: "https://engine.test/sources/1", SourceToken: "t",
	}
}

// The three steps share one volume as three users in one group: the clone
// and BuildKit as 1000, the lifecycle as 1001. Every step is in group 1000
// and the volume belongs to it, which is what lets each read and write what
// the one before it left.
func TestTheBuildStepsShareOneGroupOnOneVolume(t *testing.T) {
	job := buildJob("b", cloneBuild(), "pull", "", "")
	pod := job.Spec.Template.Spec

	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != cnbGID {
		t.Fatalf("fsGroup = %v, want %d", pod.SecurityContext, cnbGID)
	}
	var workspace *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == buildVolume {
			workspace = &pod.Volumes[i]
		}
	}
	if workspace == nil || workspace.EmptyDir == nil {
		t.Fatalf("the workspace is not an emptyDir: %+v", workspace)
	}

	want := map[string]struct{ uid, gid int64 }{
		cloneContainer:      {buildUID, cnbGID},
		dockerfileContainer: {buildUID, cnbGID},
		buildpackContainer:  {cnbUID, cnbGID},
	}
	steps := append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
	if len(steps) != 3 {
		t.Fatalf("%d steps, want 3", len(steps))
	}
	for _, c := range steps {
		w := want[c.Name]
		sc := c.SecurityContext
		if sc == nil || sc.RunAsUser == nil || sc.RunAsGroup == nil {
			t.Fatalf("%s has no user and group", c.Name)
		}
		if *sc.RunAsUser != w.uid || *sc.RunAsGroup != w.gid {
			t.Errorf("%s runs as %d:%d, want %d:%d", c.Name, *sc.RunAsUser, *sc.RunAsGroup, w.uid, w.gid)
		}
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == buildVolume && m.MountPath == "/workspace" && !m.ReadOnly {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("%s does not mount the workspace writable at /workspace", c.Name)
		}
	}
	// The lifecycle is told the user it runs as, or the layers it writes
	// belong to somebody else.
	script := pod.Containers[0].Args[0]
	if !strings.Contains(script, "-uid=1001 -gid=1000") {
		t.Error("the lifecycle is not given the user its step runs as")
	}
}

// The source is left writable by the build's group, after it is in place,
// whichever way it arrived: npm-install writes node_modules into the app
// directory as 1001, and the checkout belongs to 1000.
func TestTheSourceIsLeftWritableByTheBuildsGroup(t *testing.T) {
	for name, req := range map[string]orchestrator.BuildRequest{
		"clone": cloneBuild(), "upload": uploadBuild(),
	} {
		t.Run(name, func(t *testing.T) {
			c := sourceStep(req, req.SourceURL != "")
			if c.Name != cloneContainer {
				t.Fatalf("the source step is %q", c.Name)
			}
			if got := strings.Join(c.Command, " "); got != "/bin/sh -euc" {
				t.Errorf("command = %q", got)
			}
			script := c.Args[0]
			share := strings.Index(script, shareSourceScript)
			if share < 0 {
				t.Fatalf("the %s step does not share the source with the build's group:\n%s", name, script)
			}
			last := "git clone"
			if name == "upload" {
				last = "tar -xzf"
			}
			if i := strings.LastIndex(script, last); i < 0 || i > share {
				t.Errorf("the source is shared before %q has written it", last)
			}
		})
	}
}

// What the script does, and not only that it is there: a checkout with the
// modes the default umask leaves becomes writable by the group, directories
// stay searchable, and files do not become executable.
func TestShareSourceScriptMakesTheCheckoutGroupWritable(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An archive can bring a mode the group cannot even read.
	if err := os.WriteFile(filepath.Join(src, "lib", "private.js"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{src, filepath.Join(src, "lib")} {
		_ = os.Chmod(p, 0o755)
	}

	script := strings.ReplaceAll(shareSourceScript, "/workspace", root)
	if out, err := exec.Command("/bin/sh", "-euc", script).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	for path, want := range map[string]os.FileMode{
		src:                                  0o775,
		filepath.Join(src, "lib"):            0o775,
		filepath.Join(src, "package.json"):   0o664,
		filepath.Join(src, "lib/private.js"): 0o660,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", strings.TrimPrefix(path, root), got, want)
		}
	}
}

// runBuildpackStep runs the buildpack step's script against a stand-in
// lifecycle, with /workspace and /cnb moved into a temporary directory. The
// stand-in records each run's platform environment and exits with what
// exits says for that run.
func runBuildpackStep(t *testing.T, files map[string]string, env map[string]string, exits ...string) (string, []string, error) {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	for name, body := range files {
		p := filepath.Join(ws, "src", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := filepath.Join(root, "build-args")
	if err := os.MkdirAll(args, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range env {
		if err := os.WriteFile(filepath.Join(args, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lifecycle := filepath.Join(root, "cnb", "lifecycle")
	if err := os.MkdirAll(lifecycle, 0o755); err != nil {
		t.Fatal(err)
	}
	// Run n exits with the nth code given, and appends what it was given
	// as BP_* platform variables to runs.
	creator := `#!/bin/sh
n=$(( $(cat "$ROOT/count" 2>/dev/null || echo 0) + 1 ))
echo $n > "$ROOT/count"
line="run$n"
for f in "$ROOT"/workspace/platform/env/BP_*; do
  [ -f "$f" ] && line="$line ${f##*/}=$(cat "$f")"
done
echo "$line" >> "$ROOT/runs"
echo "platform NODE_ENV=$(cat "$ROOT"/workspace/platform/env/NODE_ENV 2>/dev/null)"
set -- ` + strings.Join(exits, " ") + `
shift $(( n - 1 ))
exit "${1:-0}"
`
	if err := os.WriteFile(filepath.Join(lifecycle, "creator"), []byte(creator), 0o755); err != nil {
		t.Fatal(err)
	}

	c := buildpackStep(orchestrator.BuildRequest{Image: "r/x:1"}, true)
	script := c.Args[0]
	script = strings.ReplaceAll(script, "/workspace", ws)
	script = strings.ReplaceAll(script, "/cnb/lifecycle", lifecycle)

	cmd := exec.Command(c.Command[0], append(c.Command[1:], script)...)
	cmd.Env = append(os.Environ(), "ROOT="+root, "IMAGE=r/x:1", "SUBDIR=", "BUILD_ARGS="+args)
	out, err := cmd.CombinedOutput()
	runs, _ := os.ReadFile(filepath.Join(root, "runs"))
	return string(out), strings.Split(strings.TrimSpace(string(runs)), "\n"), err
}

// A repository nothing recognises and that has an index.html is served by
// httpd: the lifecycle is run again, asked for a web server rooted where the
// page is.
func TestAStaticSiteIsServedByHttpdWhenNothingElseIsRecognised(t *testing.T) {
	for _, tc := range []struct {
		name, page, root string
		code             string
	}{
		{"at the top", "index.html", ".", "21"},
		{"in public", "public/index.html", "public", "20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, runs, err := runBuildpackStep(t, map[string]string{tc.page: "<p>hi</p>"}, nil, tc.code, "0")
			if err != nil {
				t.Fatalf("the step failed: %v\n%s", err, out)
			}
			want := []string{"run1", "run2 BP_WEB_SERVER=httpd BP_WEB_SERVER_ROOT=" + tc.root}
			if strings.Join(runs, "|") != strings.Join(want, "|") {
				t.Errorf("runs = %q, want %q", runs, want)
			}
			if !strings.Contains(out, "static site with Apache httpd") || !strings.Contains(out, "Pushed r/x:1") {
				t.Errorf("the log does not say what happened:\n%s", out)
			}
		})
	}
}

// Asking for a web server only once nothing else matched is the point: a Node app
// that also has a public/index.html is built as a Node app.
func TestARecognisedAppIsBuiltOnce(t *testing.T) {
	out, runs, err := runBuildpackStep(t, map[string]string{
		"package.json": "{}", "public/index.html": "<p>hi</p>",
	}, nil, "0")
	if err != nil {
		t.Fatalf("the step failed: %v\n%s", err, out)
	}
	if len(runs) != 1 || runs[0] != "run1" {
		t.Errorf("runs = %q, want one run with no web server asked for", runs)
	}
}

// The app's own choice stands: a BP_WEB_SERVER the team set is never
// replaced, and a set root is kept.
func TestAWebServerTheAppChoseIsLeftAlone(t *testing.T) {
	out, runs, err := runBuildpackStep(t, map[string]string{"index.html": "x"},
		map[string]string{"BP_WEB_SERVER": "nginx"}, "21")
	if err == nil {
		t.Fatalf("the step passed a failed detection:\n%s", out)
	}
	if len(runs) != 1 {
		t.Errorf("runs = %q, want one", runs)
	}

	_, runs, err = runBuildpackStep(t, map[string]string{"index.html": "x"},
		map[string]string{"BP_WEB_SERVER_ROOT": "site"}, "21", "0")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[1] != "run2 BP_WEB_SERVER=httpd BP_WEB_SERVER_ROOT=site" {
		t.Errorf("runs = %q", runs)
	}
}

// With nothing recognised and no page to serve, the build fails with the
// lifecycle's own code and one line saying what would work.
func TestNothingRecognisedSaysWhatWouldBe(t *testing.T) {
	out, runs, err := runBuildpackStep(t, map[string]string{"README.md": "x"}, nil, "21")
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 21 {
		t.Fatalf("err = %v, want exit 21\n%s", err, out)
	}
	if len(runs) != 1 {
		t.Errorf("runs = %q, want one", runs)
	}
	if !strings.Contains(out, "Add a Dockerfile, or a Procfile") {
		t.Errorf("the log does not say what to add:\n%s", out)
	}
	if strings.Contains(out, "Pushed") {
		t.Errorf("a failed build says it pushed:\n%s", out)
	}
}

// Any other failure is the build's, not a missing static site: it is not
// retried, and its code is the step's.
func TestABuildThatFailsIsNotRetriedAsAStaticSite(t *testing.T) {
	out, runs, err := runBuildpackStep(t, map[string]string{"index.html": "x", "package.json": "{}"}, nil, "51")
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 51 {
		t.Fatalf("err = %v, want exit 51\n%s", err, out)
	}
	if len(runs) != 1 {
		t.Errorf("runs = %q, want one", runs)
	}
}

// A buildpacks build runs with NODE_ENV=production, as Next.js needs to
// prerender, unless the app set NODE_ENV to something else.
func TestABuildpacksBuildIsAProductionBuild(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"by default", nil, "production"},
		{"unless the app says otherwise", map[string]string{"NODE_ENV": "staging"}, "staging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := runBuildpackStep(t, map[string]string{"package.json": "{}"}, tc.env, "0")
			if err != nil {
				t.Fatalf("the step failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, "platform NODE_ENV="+tc.want+"\n") {
				t.Errorf("the lifecycle was not given NODE_ENV=%s:\n%s", tc.want, out)
			}
		})
	}
}

// A buildpacks build that pushed ends there. The uid probe needs BuildKit,
// which the builder image does not have, so running it only ever added an
// error to a build that had none.
func TestABuildpacksBuildEndsWithItsPush(t *testing.T) {
	out, _, err := runBuildpackStep(t, map[string]string{"package.json": "{}"}, nil, "0")
	if err != nil {
		t.Fatalf("the step failed: %v\n%s", err, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "Pushed r/x:1") {
		t.Errorf("the log does not end with the push:\n%s", out)
	}
	if strings.Contains(out, "Could not work out") || strings.Contains(out, "buildctl") {
		t.Errorf("a good build reports a failure:\n%s", out)
	}
}
