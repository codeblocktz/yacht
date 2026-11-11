package k8s

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"

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
// images. So no step that runs the repository's code may reach it — not the
// Dockerfile step, whose RUN lines are the repository's, and not the build
// phase, which runs its build scripts. Only the steps that push or read the
// registry, and run nothing of the repository's, hold it.
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

			holds := map[string]bool{
				pushContainer: true, analyzeContainer: true,
				restoreContainer: true, exportContainer: true,
			}
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
	for _, c := range []corev1.Container{dockerfileStep(req, true), lifecycleBuildStep(req)} {
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

// The lifecycle's records are on a volume the build phase can only read and
// the Dockerfile step cannot see. Were they in the layers directory, the
// build could rewrite analyzed.toml to name another team's image as the run
// image, and the export would pull it with the install's credential.
func TestTheBuildCannotRewriteWhatTheExportReads(t *testing.T) {
	req := registryBuild()
	job := buildJob("b", req, buildRegistrySecret, "", "")
	pod := job.Spec.Template.Spec
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		mounted, readOnly := false, false
		for _, m := range c.VolumeMounts {
			if m.Name == phasesVolume {
				mounted, readOnly = true, m.ReadOnly
			}
		}
		switch c.Name {
		case cloneContainer, dockerfileContainer, pushContainer:
			if mounted {
				t.Errorf("%s mounts the lifecycle's records", c.Name)
			}
		case buildContainer:
			if !mounted || !readOnly {
				t.Errorf("the build mounts the lifecycle's records: %t, read-only: %t", mounted, readOnly)
			}
		default:
			if !mounted || readOnly {
				t.Errorf("%s mounts the lifecycle's records: %t, read-only: %t", c.Name, mounted, readOnly)
			}
		}
	}

	export := exportStep(req).Args[0]
	for _, flag := range []string{
		"-analyzed=" + phasesPath + "/", "-group=" + phasesPath + "/", "-cache-dir=" + phasesPath + "/",
		"-report=" + phasesPath + "/", "-project-metadata=" + phasesPath + "/", "-extended=" + phasesPath + "/",
	} {
		if !strings.Contains(export, flag) {
			t.Errorf("the export does not read %s from the protected volume", flag)
		}
	}
	if !strings.Contains(detectStep(req, false).Args[0], "-order=/cnb/order.toml") {
		t.Error("detection may take its order from the layers directory")
	}
}

// The Dockerfile step builds an OCI layout and pushes nothing; the push step
// pushes that layout, and the only Dockerfile it builds is its own one line.
func TestADockerfileIsBuiltInOneStepAndPushedInTheNext(t *testing.T) {
	req := registryBuild()
	req.Insecure = true
	build := dockerfileStep(req, false).Args[0]
	if strings.Contains(build, "push=true") || !strings.Contains(build, "--output type=oci,dest="+imageLayout+",tar=false") {
		t.Errorf("the Dockerfile step does not build to a layout without pushing:\n%s", build)
	}
	if strings.Contains(build, "$INSECURE\n") || strings.Contains(build, "registry.insecure") {
		t.Error("the Dockerfile step carries a push option")
	}
	// The uid probe runs the image's own "id", so it reads the layout here
	// rather than pulling the pushed image somewhere with a credential.
	if !strings.Contains(build, "oci-layout://built@") || strings.Contains(build, `"$IMAGE" \`) {
		t.Errorf("the uid probe does not read the built layout:\n%s", build)
	}
	if i, j := strings.Index(build, "touch "+dockerfileMarker), strings.Index(build, "printf %s \"$digest\" > "+imageDigest); i < 0 || j < 0 || j > i {
		t.Error("the marker is written before the digest the push needs")
	}

	push := pushStep(req)
	script := push.Args[0]
	for _, want := range []string{
		"printf 'FROM built\\n' > /tmp/push/Dockerfile",
		"--local context=/tmp/push",
		"--local dockerfile=/tmp/push",
		"--output type=image,name=$IMAGE,push=true$INSECURE",
		pushGuardScript,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the push step does not have %q", want)
		}
	}
	for _, never := range []string{"/workspace/src", "$DOCKERFILE", "$BUILD_ARGS"} {
		if strings.Contains(script, never) {
			t.Errorf("the push step reads %s", never)
		}
	}
	if strings.Index(script, pushGuardScript) > strings.Index(script, "buildctl-daemonless.sh") {
		t.Error("the push step pushes before it checks the layout")
	}
	insecure := ""
	for _, e := range push.Env {
		if e.Name == "INSECURE" {
			insecure = e.Value
		}
	}
	if insecure != ",registry.insecure=true" {
		t.Errorf("the push to a plain-HTTP registry is not told so: %q", insecure)
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

// The layout the push sends came out of the step that ran the repository's
// RUN lines. Anything in it but a plain file or directory is refused before
// the push reads a byte of it, and the digest is checked for shape without
// being printed.
func TestThePushRefusesALayoutThatIsNotPlainFiles(t *testing.T) {
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "config.json")
	if err := os.WriteFile(secret, []byte(`{"auths":{"r":{"password":"`+testRegistryPassword+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("ab", 32)

	for _, tc := range []struct {
		name  string
		plant func(ws string)
		ok    bool
	}{
		{"a clean layout", func(string) {}, true},
		{"a blob that links to the credential", func(ws string) {
			symlink(t, secret, filepath.Join(ws, "image", "blobs", "sha256", strings.Repeat("cd", 32)))
		}, false},
		{"an index that links to the credential", func(ws string) {
			_ = os.Remove(filepath.Join(ws, "image", "index.json"))
			symlink(t, secret, filepath.Join(ws, "image", "index.json"))
		}, false},
		{"the layout replaced by a link", func(ws string) {
			_ = os.RemoveAll(filepath.Join(ws, "image"))
			symlink(t, secretDir, filepath.Join(ws, "image"))
		}, false},
		{"the digest replaced by a link", func(ws string) {
			_ = os.Remove(filepath.Join(ws, "image.digest"))
			symlink(t, secret, filepath.Join(ws, "image.digest"))
		}, false},
		{"a pipe that would hang the push", func(ws string) {
			if err := syscall.Mkfifo(filepath.Join(ws, "image", "blobs", "fifo"), 0o644); err != nil {
				t.Skipf("no fifo here: %v", err)
			}
		}, false},
		{"a digest that is not one", func(ws string) {
			if err := os.WriteFile(filepath.Join(ws, "image.digest"), []byte("sha256:abc; rm -rf /"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := filepath.Join(t.TempDir(), "workspace")
			mkdirs(t, filepath.Join(ws, "image", "blobs", "sha256"))
			for name, body := range map[string]string{
				"image/index.json": `{"schemaVersion":2}`, "image/oci-layout": `{}`, "image.digest": digest,
			} {
				if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			tc.plant(ws)

			out, err := runGuard(t, pushGuardScript+`echo "digest=$digest"`, ws, "")
			if strings.Contains(out, testRegistryPassword) {
				t.Fatalf("the guard printed the credential:\n%s", out)
			}
			if tc.ok {
				if err != nil || !strings.Contains(out, "digest="+digest) {
					t.Fatalf("a clean layout was refused: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("the push went ahead:\n%s", out)
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
