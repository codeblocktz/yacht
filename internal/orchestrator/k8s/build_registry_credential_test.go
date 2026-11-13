package k8s

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

const testRegistryPassword = "registry-p4ssw0rd-SECRET"

func registryBuild() orchestrator.BuildRequest {
	req := cloneBuild()
	auth := base64.StdEncoding.EncodeToString([]byte("yacht:" + testRegistryPassword))
	req.RegistryAuth = []byte(`{"auths":{"registry.test":{"username":"yacht","password":"` +
		testRegistryPassword + `","auth":"` + auth + `"}}}`)
	return req
}

// holdsRegistryCredential reports whether a step can reach the registry
// credential: a mount of its volume, or anything in its environment that
// names the volume's path or the Secret.
func holdsRegistryCredential(c corev1.Container) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == registryVolume || strings.HasPrefix(m.MountPath, registryPath) {
			return true
		}
	}
	for _, e := range c.Env {
		if e.Name == "DOCKER_CONFIG" || e.Name == "CNB_REGISTRY_AUTH" ||
			strings.Contains(e.Value, registryPath) {
			return true
		}
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil &&
			e.ValueFrom.SecretKeyRef.Name == buildRegistrySecret {
			return true
		}
	}
	for _, from := range c.EnvFrom {
		if from.SecretRef != nil && from.SecretRef.Name == buildRegistrySecret {
			return true
		}
	}
	return false
}

// The registry credential is the install's: one account holds every team's
// images. So no step that runs the repository's code, or anything a
// repository can influence, may reach it. Only analyze and export, which run
// the lifecycle and nothing of the repository's, hold it. A Dockerfile build
// holds it nowhere: the engine pushes.
func TestOnlyStepsThatRunNoRepositoryCodeHoldTheRegistryCredential(t *testing.T) {
	for name, req := range map[string]orchestrator.BuildRequest{
		"clone": registryBuild(), "upload": uploadBuild(),
	} {
		t.Run(name, func(t *testing.T) {
			req.Insecure = true
			job := buildJob("b", req, buildRegistrySecret, "b-repo", "b-args")
			pod := job.Spec.Template.Spec

			var vol *corev1.Volume
			for i := range pod.Volumes {
				if pod.Volumes[i].Name == registryVolume {
					vol = &pod.Volumes[i]
				}
			}
			if vol == nil || vol.Secret == nil || vol.Secret.SecretName != buildRegistrySecret {
				t.Fatalf("registry volume = %+v", vol)
			}

			holds := map[string]bool{analyzeContainer: true, exportContainer: true}
			for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
				if got := holdsRegistryCredential(c); got != holds[c.Name] {
					t.Errorf("%s can reach the registry credential: %t, want %t", c.Name, got, holds[c.Name])
				}
				if holds[c.Name] {
					for _, m := range c.VolumeMounts {
						if m.Name == registryVolume && !m.ReadOnly {
							t.Errorf("%s mounts the credential writable", c.Name)
						}
					}
				}
			}
		})
	}
}

// The steps that run the repository's code are named, and checked by name,
// so a rename cannot quietly hand one of them the credential.
func TestTheStepsThatRunRepositoryCodeAreTheOnesWithoutTheCredential(t *testing.T) {
	req := registryBuild()
	for _, c := range []corev1.Container{
		dockerfileStep(req, true), detectStep(req, true), lifecycleBuildStep(req), pushStep(), restoreStep(req),
	} {
		if holdsRegistryCredential(c) {
			t.Errorf("%s holds the registry credential", c.Name)
		}
	}
	// What each of them runs is the repository's: the Dockerfile and the
	// buildpacks' build.
	if !strings.Contains(dockerfileStep(req, false).Args[0], `--opt filename="$(basename "$dockerfile")"`) {
		t.Error("the Dockerfile step does not build the repository's Dockerfile")
	}
	if !strings.Contains(lifecycleBuildStep(req).Args[0], "/cnb/lifecycle/builder") {
		t.Error("the build step does not run the lifecycle's builder")
	}
}

// The lifecycle runs a phase a step, and never as creator, which would run
// the build in the same container as the push.
func TestTheLifecycleRunsAPhaseAStep(t *testing.T) {
	req := registryBuild()
	job := buildJob("b", req, buildRegistrySecret, "", "")
	pod := job.Spec.Template.Spec
	want := map[string]string{
		analyzeContainer: "/cnb/lifecycle/analyzer",
		detectContainer:  "/cnb/lifecycle/detector",
		restoreContainer: "/cnb/lifecycle/restorer",
		buildContainer:   "/cnb/lifecycle/builder",
		exportContainer:  "/cnb/lifecycle/exporter",
	}
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		script := c.Args[0]
		if strings.Contains(script, "creator") {
			t.Errorf("%s runs creator", c.Name)
		}
		for step, phase := range want {
			if got := strings.Contains(script, phase); got != (step == c.Name) {
				t.Errorf("%s runs %s: %t", c.Name, phase, got)
			}
		}
	}
	if pod.Containers[0].Name != exportContainer {
		t.Errorf("the last step is %s, want the export", pod.Containers[0].Name)
	}
}

// The lifecycle's records are on volumes the build phase can only read and
// the Dockerfile step cannot see. Were they in the layers directory, the
// build could rewrite analyzed.toml to name another team's image as the run
// image, and the export would pull it with the install's credential. Detect,
// which the repository can influence through its variables, writes its own
// decisions and nothing else.
func TestTheBuildCannotRewriteWhatTheExportReads(t *testing.T) {
	req := registryBuild()
	job := buildJob("b", req, buildRegistrySecret, "", "")
	pod := job.Spec.Template.Spec

	type access struct{ mounted, writable bool }
	want := map[string]map[string]access{
		phasesVolume: {
			analyzeContainer: {true, true}, restoreContainer: {true, true}, exportContainer: {true, true},
			detectContainer: {true, false}, buildContainer: {true, false},
		},
		detectedVolume: {
			detectContainer:  {true, true},
			analyzeContainer: {true, false}, restoreContainer: {true, false},
			buildContainer: {true, false}, exportContainer: {true, false},
		},
	}
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		for vol, steps := range want {
			var got access
			for _, m := range c.VolumeMounts {
				if m.Name == vol {
					got = access{true, !m.ReadOnly}
				}
			}
			if got != steps[c.Name] {
				t.Errorf("%s mounts %s %+v, want %+v", c.Name, vol, got, steps[c.Name])
			}
		}
	}

	export := exportStep(req).Args[0]
	for _, flag := range []string{
		"-analyzed=" + phasesPath + "/", "-group=" + detectedPath + "/", "-cache-dir=" + phasesPath + "/",
		"-report=" + phasesPath + "/", "-project-metadata=" + phasesPath + "/", "-extended=" + phasesPath + "/",
	} {
		if !strings.Contains(export, flag) {
			t.Errorf("the export does not read %s from a protected volume", flag)
		}
	}
	detect := detectStep(req, false).Args[0]
	for _, flag := range []string{
		"-order=/cnb/order.toml", "-group=" + detectedPath + "/", "-plan=" + detectedPath + "/",
		"-generated=" + detectedPath + "/",
	} {
		if !strings.Contains(detect, flag) {
			t.Errorf("detection does not have %s", flag)
		}
	}
}

// No step shares a process namespace with another, or with the node: a
// shared one would let the repository's code in one container read the
// credential through /proc in another.
func TestNoBuildStepSharesAProcessNamespace(t *testing.T) {
	pod := buildJob("b", registryBuild(), buildRegistrySecret, "b-repo", "b-args").Spec.Template.Spec
	if pod.ShareProcessNamespace != nil && *pod.ShareProcessNamespace {
		t.Error("the build's containers share a process namespace")
	}
	if pod.HostPID || pod.HostIPC || pod.HostNetwork {
		t.Errorf("the build shares the node's namespaces: pid %t ipc %t network %t",
			pod.HostPID, pod.HostIPC, pod.HostNetwork)
	}
}

// The Dockerfile step builds an OCI layout and pushes nothing. The push step
// runs nothing of the image's — no BuildKit, no Dockerfile frontend, so no
// ONBUILD trigger — and holds no credential: it waits, read-only, for the
// engine to read the layout out of it.
func TestADockerfileIsBuiltToALayoutThatTheEnginePushes(t *testing.T) {
	req := registryBuild()
	req.Insecure = true
	build := dockerfileStep(req, false).Args[0]
	if strings.Contains(build, "push=true") || !strings.Contains(build, "--output type=oci,dest="+imageLayout+",tar=false") {
		t.Errorf("the Dockerfile step does not build to a layout without pushing:\n%s", build)
	}
	// The digest is said before the uid probe runs anything of the image's.
	marker, probe := strings.Index(build, `echo "`+imageMarker+`$digest"`), strings.Index(build, "uidprobe")
	if marker < 0 || probe < 0 || marker > probe {
		t.Error("the image's digest is not printed before the uid probe runs the image")
	}
	if i := strings.Index(build, "touch "+dockerfileMarker); i < marker {
		t.Error("the marker is written before the digest is said")
	}

	push := pushStep()
	script := strings.Join(append(append([]string{}, push.Command...), push.Args...), " ")
	for _, never := range []string{
		"buildctl", "buildkit", "dockerfile.v0", "--frontend", "FROM", "oci-layout",
		"$DOCKERFILE", "$BUILD_ARGS", registryPath,
	} {
		if strings.Contains(script, never) {
			t.Errorf("the push step runs %q:\n%s", never, script)
		}
	}
	if push.Image != gitImage {
		t.Errorf("the push step runs %s, want the git image, which has tar and nothing of BuildKit's", push.Image)
	}
	if len(push.Env) != 0 || len(push.EnvFrom) != 0 {
		t.Errorf("the push step is given an environment: %+v", push.Env)
	}
	if len(push.VolumeMounts) != 1 || push.VolumeMounts[0].Name != buildVolume || !push.VolumeMounts[0].ReadOnly {
		t.Errorf("the push step mounts %+v, want the workspace read-only and nothing else", push.VolumeMounts)
	}
	if sc := push.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("the push step is not hardened: %+v", sc)
	}
}

// The push step ends at once when there is no Dockerfile image — every
// buildpacks build — and otherwise when the engine says it has pushed.
func TestThePushStepWaitsOnlyForADockerfileBuild(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	mkdirs(t, ws)
	script := pushStep().Args[0]
	script = strings.ReplaceAll(script, "/workspace", ws)
	script = strings.ReplaceAll(script, handedOver, filepath.Join(root, "handed-over"))

	if out, err := exec.Command("/bin/sh", "-euc", script).CombinedOutput(); err != nil {
		t.Fatalf("with no Dockerfile image the push step failed: %v\n%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(ws, ".dockerfile-built"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-euc", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("the push step ended before the image was collected: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(root, "handed-over"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the push step failed once handed over: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the push step did not end once handed over")
	}
}

// Variables that change how a program is loaded or a shell starts are not
// handed to buildpacks: detection runs the builder's buildpacks, and
// LD_PRELOAD or BASH_ENV would run the repository's code inside them. The
// rest reach the lifecycle as before, and the withheld names are said.
func TestDetectionIsNotGivenLoaderOrShellVariables(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	args := filepath.Join(root, "build-args")
	lifecycle := filepath.Join(root, "cnb", "lifecycle")
	mkdirs(t, filepath.Join(ws, "src"), filepath.Join(ws, "platform", "env"), args, lifecycle)
	if err := os.WriteFile(filepath.Join(lifecycle, "detector"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withheld := []string{
		"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "BASH_ENV", "ENV", "PATH", "CNB_PLATFORM_API",
		"CNB_APP_DIR", "GLIBC_TUNABLES", "SHELLOPTS", "BASHOPTS", "PS4", "GCONV_PATH", "BASH_FUNC_x",
	}
	given := []string{"DATABASE_URL", "NODE_OPTIONS", "BP_WEB_SERVER", "LDAP_URL", "PATHNAME", "ENVIRONMENT"}
	for _, name := range append(append([]string{}, withheld...), given...) {
		if err := os.WriteFile(filepath.Join(args, name), []byte("/workspace/src/evil.so"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	script := detectStep(orchestrator.BuildRequest{Image: "r/x:1"}, true).Args[0]
	script = strings.ReplaceAll(script, "/workspace", ws)
	script = strings.ReplaceAll(script, "/cnb/lifecycle", lifecycle)
	script = strings.ReplaceAll(script, detectedPath, filepath.Join(root, "detected"))
	cmd := exec.Command("/bin/sh", "-euc", script)
	cmd.Env = append(os.Environ(), "SUBDIR=", "BUILD_ARGS="+args)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("detect failed: %v\n%s", err, out)
	}
	for _, name := range withheld {
		if _, err := os.Stat(filepath.Join(ws, "platform", "env", name)); err == nil {
			t.Errorf("%s was handed to buildpacks", name)
		}
		if !strings.Contains(string(out), " "+name) {
			t.Errorf("the log does not say %s was withheld:\n%s", name, out)
		}
	}
	for _, name := range given {
		if _, err := os.Stat(filepath.Join(ws, "platform", "env", name)); err != nil {
			t.Errorf("%s was not handed to buildpacks: %v", name, err)
		}
	}
	if strings.Contains(string(out), "evil.so") {
		t.Errorf("a value reached the log:\n%s", out)
	}
}

// The image a Dockerfile build made is read from the Dockerfile step's log
// alone. The build phase runs the repository's scripts, which can print
// anything; a line there naming an image is a line.
func TestTheImageMarkerIsTakenFromTheDockerfileStepAlone(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		step, line, want string
		shown            bool
	}{
		{dockerfileContainer, imageMarker + digest, digest, false},
		{buildContainer, imageMarker + digest, "", true},
		{exportContainer, imageMarker + digest, "", true},
		{dockerfileContainer, imageMarker + "sha256:nothex", "", false},
	} {
		var (
			result orchestrator.BuildResult
			seen   markersSeen
			shown  string
		)
		scanStepLog(tc.step, strings.NewReader(tc.line+"\n"), func(s string) { shown += s }, &result, &seen)
		if seen.image != tc.want {
			t.Errorf("%s %q: image = %q, want %q", tc.step, tc.line, seen.image, tc.want)
		}
		if (shown != "") != tc.shown {
			t.Errorf("%s %q: shown %q", tc.step, tc.line, shown)
		}
	}
}

// layoutEntry is one entry of a tar the fake push step answers with.
type layoutEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
}

// fakePushStep answers execs in the push step: a tar holding entries for
// tar, and success for anything else. It records every command.
func fakePushStep(t *testing.T, o *Orchestrator, entries func(path string) []layoutEntry) *[][]string {
	t.Helper()
	var commands [][]string
	var mu sync.Mutex
	o.execStream = func(_ context.Context, ns, _ string, opts *corev1.PodExecOptions, streams remotecommand.StreamOptions) error {
		mu.Lock()
		commands = append(commands, append([]string{opts.Container}, opts.Command...))
		mu.Unlock()
		if ns != BuildNamespace || opts.Container != pushContainer {
			return errors.New("exec outside the push step")
		}
		if opts.Command[0] != "tar" {
			return nil
		}
		tw := tar.NewWriter(streams.Stdout)
		for _, e := range entries(opts.Command[len(opts.Command)-1]) {
			hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Linkname: e.linkname, Mode: 0o644}
			if e.typeflag == tar.TypeReg {
				hdr.Size = int64(len(e.body))
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := io.WriteString(tw, e.body); err != nil {
				return err
			}
		}
		return tw.Close()
	}
	return &commands
}

// The layout is read through tar, one named file at a time, and anything
// that is not a plain file of that name is refused rather than followed.
func TestTheLayoutIsReadAsPlainFilesOnly(t *testing.T) {
	blob := "blobs/sha256/" + strings.Repeat("cd", 32)
	for _, tc := range []struct {
		name    string
		path    string
		entries []layoutEntry
		limit   int64
		ok      bool
	}{
		{"a plain file", blob, []layoutEntry{{name: blob, typeflag: tar.TypeReg, body: "layer"}}, 100, true},
		{"the index", "index.json", []layoutEntry{{name: "index.json", typeflag: tar.TypeReg, body: "{}"}}, 100, true},
		{"a symlink to the credential", blob,
			[]layoutEntry{{name: blob, typeflag: tar.TypeSymlink, linkname: "/registry/config.json"}}, 100, false},
		{"a hard link", blob, []layoutEntry{{name: blob, typeflag: tar.TypeLink, linkname: "index.json"}}, 100, false},
		{"a directory", blob, []layoutEntry{{name: blob + "/", typeflag: tar.TypeDir}}, 100, false},
		{"a pipe", blob, []layoutEntry{{name: blob, typeflag: tar.TypeFifo}}, 100, false},
		{"another file", blob, []layoutEntry{{name: "index.json", typeflag: tar.TypeReg, body: "{}"}}, 100, false},
		{"too large", blob, []layoutEntry{{name: blob, typeflag: tar.TypeReg, body: "layer"}}, 3, false},
		{"nothing", blob, nil, 100, false},
		{"a path outside the layout's blobs", "../registry/config.json", nil, 100, false},
		{"another file of the layout", "oci-layout", nil, 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := testOrchestrator(t)
			commands := fakePushStep(t, o, func(string) []layoutEntry { return tc.entries })
			rc, size, err := podLayout{o: o, pod: "p"}.Open(context.Background(), tc.path, tc.limit)
			if !tc.ok {
				if err == nil {
					rc.Close()
					t.Fatal("opened")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			if string(body) != tc.entries[0].body || size != int64(len(body)) {
				t.Errorf("read %q (%d)", body, size)
			}
			want := []string{pushContainer, "tar", "-C", imageLayout, "-cf", "-", tc.path}
			if got := (*commands)[0]; strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("ran %q, want %q", got, want)
			}
		})
	}
}

// pushBuildPod makes the build's pod, with its push step in the given state.
func pushBuildPod(t *testing.T, o *Orchestrator, client *fake.Clientset, state corev1.ContainerState) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "build-pod", Namespace: BuildNamespace},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: pushContainer, State: state}},
		},
	}
	if _, err := client.CoreV1().Pods(BuildNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A Dockerfile build's image is pushed by the engine through PushLayout,
// reading the layout out of the push step; only then is the push step told it
// may end.
func TestTheEnginePushesTheDockerfileImageThenReleasesThePushStep(t *testing.T) {
	o, client := testOrchestrator(t)
	pushBuildPod(t, o, client, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
	commands := fakePushStep(t, o, func(path string) []layoutEntry {
		return []layoutEntry{{name: path, typeflag: tar.TypeReg, body: `{"schemaVersion":2}`}}
	})
	digest := "sha256:" + strings.Repeat("ab", 32)

	req := registryBuild()
	req.Log = func(string) {}
	var pushed []string
	req.PushLayout = func(ctx context.Context, image, d string, layout orchestrator.LayoutFiles, _ func(string)) error {
		rc, _, err := layout.Open(ctx, "index.json", 1<<20)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		pushed = append(pushed, image, d, string(body))
		if len(*commands) != 1 {
			t.Errorf("the push step was released before the push: %q", *commands)
		}
		return nil
	}
	if err := o.pushBuiltImage(context.Background(), "build-pod", req, digest); err != nil {
		t.Fatal(err)
	}
	if strings.Join(pushed, " ") != req.Image+" "+digest+` {"schemaVersion":2}` {
		t.Errorf("pushed %q", pushed)
	}
	last := (*commands)[len(*commands)-1]
	if strings.Join(last, " ") != pushContainer+" touch "+handedOver {
		t.Errorf("the push step was not released: %q", last)
	}

	// A failed push leaves the push step waiting, for the Job's deletion to
	// end.
	*commands = nil
	req.PushLayout = func(context.Context, string, string, orchestrator.LayoutFiles, func(string)) error {
		return errors.New("refused")
	}
	if err := o.pushBuiltImage(context.Background(), "build-pod", req, digest); err == nil {
		t.Fatal("a refused push succeeded")
	}
	for _, c := range *commands {
		if c[1] == "touch" {
			t.Error("the push step was released after a failed push")
		}
	}

	// Nothing to push with is a failure, not an image left unpushed.
	req.PushLayout = nil
	if err := o.pushBuiltImage(context.Background(), "build-pod", req, digest); err == nil {
		t.Fatal("a Dockerfile build with nothing to push it succeeded")
	}
}

// A push step that ended before the engine read from it is a failed build.
func TestAPushStepThatEndedFailsTheBuild(t *testing.T) {
	o, client := testOrchestrator(t)
	pushBuildPod(t, o, client, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})
	req := registryBuild()
	req.Log = func(string) {}
	req.PushLayout = func(context.Context, string, string, orchestrator.LayoutFiles, func(string)) error {
		t.Error("pushed from a push step that had ended")
		return nil
	}
	if err := o.pushBuiltImage(context.Background(), "build-pod", req, "sha256:"+strings.Repeat("ab", 32)); err == nil {
		t.Fatal("succeeded")
	}
}

// runGuard runs a guard script with /workspace moved to ws, and returns its
// output and error.
func runGuard(t *testing.T, script, ws, subdir string) (string, error) {
	t.Helper()
	script = strings.ReplaceAll(script, "/workspace", ws)
	cmd := exec.Command("/bin/sh", "-euc", script)
	cmd.Env = append(os.Environ(), "SUBDIR="+subdir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// The export follows the path to the app directory, and the first two levels
// of the layers directory, by name. A symlink there — planted by the build,
// which ran the repository's code — would have the exporter archive the
// credential into the team's own image. The export refuses it. A symlink
// deeper in a layer is archived as a symlink, and is left alone.
func TestTheExportRefusesToFollowALinkTheBuildLeft(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "registry")
	mkdirs(t, secret)

	for _, tc := range []struct {
		name, subdir string
		plant        func(ws string)
		refused      string
	}{
		{"a clean build", "", func(string) {}, ""},
		{"a clean build in a subdirectory", "apps/web", func(string) {}, ""},
		{"links inside a layer", "", func(ws string) {
			bin := filepath.Join(ws, "layers", "paketo_npm", "modules", "node_modules", ".bin")
			mkdirs(t, bin)
			symlink(t, "../next/dist/bin/next", filepath.Join(bin, "next"))
			symlink(t, "/usr/bin/python3", filepath.Join(ws, "layers", "paketo_npm", "modules", "python"))
		}, ""},
		{"the source replaced by a link", "", func(ws string) {
			_ = os.RemoveAll(filepath.Join(ws, "src"))
			symlink(t, secret, filepath.Join(ws, "src"))
		}, "/src"},
		{"a link on the way to the app directory", "apps/web", func(ws string) {
			_ = os.RemoveAll(filepath.Join(ws, "src", "apps"))
			symlink(t, filepath.Dir(secret), filepath.Join(ws, "src", "apps"))
		}, "/src/apps"},
		{"the app directory itself", "apps/web", func(ws string) {
			_ = os.RemoveAll(filepath.Join(ws, "src", "apps", "web"))
			symlink(t, secret, filepath.Join(ws, "src", "apps", "web"))
		}, "/src/apps/web"},
		{"the layers directory", "", func(ws string) {
			_ = os.RemoveAll(filepath.Join(ws, "layers"))
			symlink(t, secret, filepath.Join(ws, "layers"))
		}, "/layers"},
		{"a buildpack's directory", "", func(ws string) {
			symlink(t, secret, filepath.Join(ws, "layers", "paketo_node"))
		}, "/layers/paketo_node"},
		{"a layer's metadata", "", func(ws string) {
			symlink(t, filepath.Join(secret, "config.json"), filepath.Join(ws, "layers", "paketo_npm", "modules.toml"))
		}, "/layers/paketo_npm/modules.toml"},
		{"a layer", "", func(ws string) {
			_ = os.RemoveAll(filepath.Join(ws, "layers", "paketo_npm", "modules"))
			symlink(t, secret, filepath.Join(ws, "layers", "paketo_npm", "modules"))
		}, "/layers/paketo_npm/modules"},
		{"the launch config", "", func(ws string) {
			symlink(t, "/proc/self/environ", filepath.Join(ws, "layers", "config", "metadata.toml"))
		}, "/layers/config/metadata.toml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := filepath.Join(t.TempDir(), "workspace")
			app := filepath.Join(ws, "src", tc.subdir)
			mkdirs(t, app, filepath.Join(ws, "layers", "paketo_npm", "modules"), filepath.Join(ws, "layers", "config"))
			if err := os.WriteFile(filepath.Join(app, "package.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.plant(ws)

			out, err := runGuard(t, exportGuardScript, ws, tc.subdir)
			if tc.refused == "" {
				if err != nil {
					t.Fatalf("a clean export was refused: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("the export went ahead through a link at %s", tc.refused)
			}
			if !strings.Contains(out, ws+tc.refused) || !strings.Contains(out, "nothing is pushed") {
				t.Errorf("the refusal does not name %s:\n%s", tc.refused, out)
			}
		})
	}
}

// The registry credential is in the build's redactions, password, the
// base64 docker writes beside it, and any token, though no step that runs
// the repository's code can reach it. The username is left alone.
func TestTheRegistryCredentialIsRedactedFromTheLog(t *testing.T) {
	req := registryBuild()
	req.Redact = []string{testSecretValue}
	auth := base64.StdEncoding.EncodeToString([]byte("yacht:" + testRegistryPassword))

	redact := buildRedactions(req, testRepoToken)
	var got string
	logf := redactingLog(func(s string) { got += s }, redact...)
	logf(`{"auths":{"registry.test":{"username":"yacht","password":"` + testRegistryPassword +
		`","auth":"` + auth + `"}}} ` + testRepoToken + " " + testSecretValue + "\n")
	for _, secret := range []string{testRegistryPassword, auth, testRepoToken, testSecretValue} {
		if strings.Contains(got, secret) {
			t.Errorf("%q is in the log: %s", secret, got)
		}
	}
	if !strings.Contains(got, `"username":"yacht"`) {
		t.Errorf("the username was hidden: %s", got)
	}

	tokens := registrySecrets([]byte(`{"auths":{"a":{"identitytoken":"identity-TOKEN-1"},"b":{"registrytoken":"registry-TOKEN-2"}}}`))
	if strings.Join(tokens, " ") == "" ||
		!strings.Contains(strings.Join(tokens, " "), "identity-TOKEN-1") ||
		!strings.Contains(strings.Join(tokens, " "), "registry-TOKEN-2") {
		t.Errorf("tokens = %q", tokens)
	}
	if got := registrySecrets([]byte("not json")); got != nil {
		t.Errorf("an unreadable config gave %q", got)
	}
}

// The exporter writes the lifecycle's own SBOM three and four levels into
// the layers directory, below where the guard looks for links. What the build
// left at those two paths is removed before the export — links removed, not
// followed — for the exporter to make afresh.
func TestTheExportClearsTheLifecyclesSBOMPaths(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "registry")
	mkdirs(t, secret)
	if err := os.WriteFile(filepath.Join(secret, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "workspace")
	build := filepath.Join(ws, "layers", "sbom", "build")
	launch := filepath.Join(ws, "layers", "sbom", "launch", "buildpacksio_lifecycle")
	mkdirs(t, filepath.Join(ws, "src"), build, launch, filepath.Join(ws, "layers", "sbom", "launch", "paketo_node"))
	symlink(t, secret, filepath.Join(build, "buildpacksio_lifecycle"))
	symlink(t, secret, filepath.Join(launch, "launcher"))
	if err := os.WriteFile(filepath.Join(ws, "layers", "sbom", "launch", "paketo_node", "sbom.cdx.json"),
		[]byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := runGuard(t, exportGuardScript, ws, ""); err != nil {
		t.Fatalf("the guard refused: %v\n%s", err, out)
	}
	for _, p := range []string{filepath.Join(build, "buildpacksio_lifecycle"), launch} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(secret, "config.json")); err != nil {
		t.Errorf("removing a link removed what it pointed at: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "layers", "sbom", "launch", "paketo_node", "sbom.cdx.json")); err != nil {
		t.Errorf("a buildpack's SBOM was removed: %v", err)
	}
}

// The push step waits for the engine for buildTimeout, so the engine's push
// and the build's end after it must fit inside that, or the step gives up on
// an image that is still being pushed.
func TestThePushFitsInsideThePushStepsWait(t *testing.T) {
	if pushTimeout+finishTimeout >= buildTimeout {
		t.Errorf("push %v + finish %v is not inside the push step's wait of %v", pushTimeout, finishTimeout, buildTimeout)
	}
	if !strings.Contains(pushStep().Args[0], `"$waited" -ge 1800`) {
		t.Error("the push step does not wait for buildTimeout")
	}
}
