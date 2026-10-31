package k8s

import (
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The three steps of a build, named so their logs can be told apart.
const (
	cloneContainer      = "clone"
	dockerfileContainer = "dockerfile"
	buildpackContainer  = "buildpack"
)

// commitMarker prefixes the one line of the clone's output that is data rather
// than log. Reading it out of the stream avoids a second call into the cluster
// to ask what was checked out.
const commitMarker = "yacht-commit:"

// uidMarker prefixes the resolved numeric uid, the same way.
const uidMarker = "yacht-uid:"

// dockerfileMarker is written by the Dockerfile step when it has pushed an
// image, and read by the buildpack step as its instruction to do nothing.
//
// Kubernetes has no conditional step: every container in a pod runs. So the
// two strategies are both present and the second one checks whether the first
// already succeeded — which is also why they are init containers, since those
// run in order while ordinary containers run at once.
const dockerfileMarker = "/workspace/.dockerfile-built"

// buildVolume holds the checkout and the marker, shared by all three steps.
const buildVolume = "workspace"

// buildArgsMarker prefixes the names the Dockerfile declares with ARG, or
// orchestrator.BuildArgsAll from a buildpacks build. Names only, never
// values: they are how a deploy knows which variables the image was built
// with.
const buildArgsMarker = "yacht-build-args:"

// The app's variables, one file per variable from the build's own Secret,
// mounted by the two steps that build and not by the clone.
const (
	buildArgsVolume = "build-args"
	buildArgsPath   = "/build-args"
)

// The user the Cloud Native Buildpacks lifecycle runs as, and which it stamps
// on the layers it writes.
//
// Read out of the pinned builder image rather than assumed: it publishes
// CNB_USER_ID=1001 and CNB_GROUP_ID=1000, and the uid is not the gid. Guessing
// 1000 for both fails with "unknown userid 1000" after the clone has already
// run, which is a slow way to find out.
//
// They are constants because the builder image is pinned. Unpinning it means
// reading these from the image, which is what the lifecycle's own tooling does.
const (
	cnbUID = 1001
	cnbGID = 1000
)

// buildUID is the user the clone and BuildKit steps run as. It shares the
// buildpack step's group so all three can write the shared volume.
const buildUID = 1000

// shareSourceScript makes the checkout writable by the build's group, and is
// the last thing the clone and the fetch do.
//
// Sharing a group is not enough on its own. The source belongs to the user
// that wrote it, 1000, with the 0755 and 0644 the default umask leaves —
// readable by the lifecycle, which runs as 1001, and not writable by it. A
// buildpack that writes into the app directory then fails: npm-install puts
// node_modules there, and npm cannot even create its own scratch file
// ("open /workspace/src/executable_response…: permission denied"). fsGroup
// does not help, since it sets the group of what is created, not its mode.
//
// chmod rather than a umask: an uploaded archive brings its own modes, which
// tar keeps, so only setting them afterwards covers both kinds of source. X
// adds execute to directories and leaves files as they were.
const shareSourceScript = `chmod -R g+rwX /workspace/src`

// insecureOutput is the BuildKit output option for a plain-HTTP registry.
//
// Appended to the output rather than set as a daemon-wide flag, so it applies
// to this push and not to every registry the build happens to touch — a base
// image pulled from a public registry still gets TLS.
func insecureOutput(req orchestrator.BuildRequest) string {
	if !req.Insecure {
		return ""
	}
	return ",registry.insecure=true"
}

// insecureHost names the one registry the lifecycle may reach over HTTP.
func insecureHost(req orchestrator.BuildRequest) string {
	if !req.Insecure {
		return ""
	}
	host, _, _ := strings.Cut(req.Image, "/")
	return host
}

// resolveUIDScript asks the built image what user it actually runs as.
//
// Kubernetes refuses runAsNonRoot unless it can prove the user is not root,
// and it cannot prove that from a name — an image ending "USER node" builds,
// pushes and pulls perfectly and then fails to start. The name-to-uid mapping
// lives in the image's own /etc/passwd, so the only thing that can resolve it
// is the image.
//
// So it resolves itself: a throwaway build FROM the image runs "id -u", which
// runs as whatever USER the image set, and the answer is copied out to a
// scratch stage so only that one number crosses back. No layers are pushed and
// nothing is kept.
//
// Best effort by design. A distroless image has no shell and this finds
// nothing, which is not a failure — the deploy then behaves exactly as it did
// before, and the pod says why.
const resolveUIDScript = `
mkdir -p /tmp/uidprobe
# Written to /tmp, not to /. The whole reason this probe exists is that the
# image runs as a non-root user, and that user cannot write to the root
# directory — so the obvious path is the one guaranteed to fail.
printf 'FROM %s AS app\nRUN id -u > /tmp/uid\nFROM scratch\nCOPY --from=app /tmp/uid /uid\n' "$IMAGE" \
  > /tmp/uidprobe/Dockerfile
if buildctl-daemonless.sh build \
  --frontend dockerfile.v0 \
  --local context=/tmp/uidprobe \
  --local dockerfile=/tmp/uidprobe \
  --output type=local,dest=/tmp/uidout 2>/tmp/uidprobe.err; then
  echo "` + uidMarker + `$(cat /tmp/uidout/uid | tr -d "[:space:]")"
else
  # Printed rather than swallowed. Hiding this made a broken probe look like
  # an image that simply had no answer, which is the failure that cannot be
  # told apart from success without reading it.
  echo "Could not work out which user this image runs as:"
  tail -5 /tmp/uidprobe.err
fi`

// buildJob describes one build.
//
// Three init containers rather than one container doing everything. Each step
// gets its own image — clone, BuildKit, buildpacks — so none of them has to be
// a custom image this project builds and publishes, which would make running
// Yacht depend on Yacht having somewhere to push.
func buildJob(name string, req orchestrator.BuildRequest, pullSecret, repoSecret, argsSecret string) *batchv1.Job {
	// Never retried. A build that failed will fail the same way, and a second
	// attempt would double the log, double the wait, and end where it started.
	// Kubernetes' own default is six.
	var backoff int32
	// Cleaned up by the caller, but this is the backstop for a Yacht that
	// stopped between creating the Job and deleting it.
	ttl := int32(3600)

	labels := map[string]string{
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		orchestrator.LabelOwner:     sanitiseLabel(string(req.Owner)),
		"yacht/build":               "true",
	}

	volumes := []corev1.Volume{
		{
			Name: buildVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "registry",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: pullSecret,
					Items: []corev1.KeyToPath{{
						Key: corev1.DockerConfigJsonKey, Path: "config.json",
					}},
				},
			},
		},
	}
	if repoSecret != "" {
		// Readable by the build's group and nobody else, and mounted by the
		// clone alone: the steps after it run the repository's own
		// Dockerfile and buildpacks, which have no business reaching it.
		mode := int32(0o440)
		volumes = append(volumes, corev1.Volume{
			Name: repoCredentialVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  repoSecret,
					DefaultMode: &mode,
					Items:       []corev1.KeyToPath{{Key: repoTokenKey, Path: repoTokenKey}},
				},
			},
		})
	}

	if argsSecret != "" {
		// The same mode as the token's. The clone does not mount it: it runs
		// before anything of the app's own and needs none of this.
		mode := int32(0o440)
		volumes = append(volumes, corev1.Volume{
			Name: buildArgsVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: argsSecret, DefaultMode: &mode},
			},
		})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: BuildNamespace, Labels: labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,

					// A build runs whatever a repository's Dockerfile says to
					// run. Without this it would run it with a token for the
					// Kubernetes API mounted at a well-known path — which is
					// the standard finding against in-cluster builders, and
					// why Tekton and Knative both disable it and bind their
					// build pods to a service account holding no permissions.
					ServiceAccountName:           BuildServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),

					// The three steps share a volume and do not share a user:
					// the buildpack lifecycle runs as 1001 while the clone and
					// BuildKit run as 1000. An emptyDir belongs to root, so
					// without a common group the second step to touch it
					// cannot write. fsGroup is what Kubernetes provides for
					// exactly this, and all three are in group 1000.
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup: int64Ptr(cnbGID),
					},
					Volumes: volumes,
					InitContainers: []corev1.Container{
						sourceStep(req, repoSecret != ""),
						dockerfileStep(req, argsSecret != ""),
					},
					Containers: []corev1.Container{buildpackStep(req, argsSecret != "")},
				},
			},
		},
	}
}

// repoCredentialVolume is the clone's view of a private repository's token,
// mounted at repoCredentialPath.
const (
	repoCredentialVolume = "repo-credential"
	repoCredentialPath   = "/repo-credential"
)

// cloneStep fetches the repository.
//
// The URL and ref are passed as environment variables rather than interpolated
// into the script. A repository URL is somebody's input, and a value spliced
// into a shell command is a command they get to extend — validation upstream
// makes that unlikely, and this makes it impossible.
//
// A private repository's token is a file from a Secret, which git reads
// through a credential helper when the server asks for one. Not in the URL,
// where git would write it into the checkout's config and the Dockerfile
// step would copy it into the image; not an argument or a variable, which
// describing the pod would show. -c configures this one command, so the
// helper is not left in the checkout either.
func cloneStep(req orchestrator.BuildRequest, withToken bool) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "REPO_URL", Value: req.RepoURL},
		{Name: "REPO_REF", Value: req.Ref},
		// A credential refused is a failed clone, not a prompt that waits
		// for a terminal nobody is at.
		{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
	}
	mounts := []corev1.VolumeMount{{Name: buildVolume, MountPath: "/workspace"}}
	if withToken {
		env = append(env, corev1.EnvVar{Name: "REPO_TOKEN_FILE", Value: repoCredentialPath + "/" + repoTokenKey})
		mounts = append(mounts, corev1.VolumeMount{
			Name: repoCredentialVolume, MountPath: repoCredentialPath, ReadOnly: true,
		})
	}
	return corev1.Container{
		Name:    cloneContainer,
		Image:   gitImage,
		Env:     env,
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
echo "Cloning $REPO_URL at $REPO_REF"
if [ -n "${REPO_TOKEN_FILE:-}" ]; then
  git -c credential.helper='!f() { [ "$1" = get ] || exit 0; echo username=x-access-token; echo "password=$(cat "$REPO_TOKEN_FILE")"; }; f' \
    clone --depth 1 --branch "$REPO_REF" --single-branch "$REPO_URL" /workspace/src
else
  git clone --depth 1 --branch "$REPO_REF" --single-branch "$REPO_URL" /workspace/src
fi
echo "` + commitMarker + `$(git -C /workspace/src rev-parse HEAD)"
echo "Cloned $(git -C /workspace/src rev-parse --short HEAD)"
` + shareSourceScript + `
`},
		SecurityContext: hardened(buildUID),
		VolumeMounts:    mounts,
	}
}

// sourceStep puts the source in /workspace/src: a clone of the repository,
// or the archive the CLI uploaded.
func sourceStep(req orchestrator.BuildRequest, withCredential bool) corev1.Container {
	if req.SourceURL != "" {
		return fetchStep(req, withCredential)
	}
	return cloneStep(req, withCredential)
}

// fetchStep downloads an uploaded archive from the engine and unpacks it.
//
// The same container name and image as the clone. Everything after this step
// reads /workspace/src and nothing else, so an upload is a build like any
// other from there on; and the image is alpine, whose busybox has wget, TLS
// and a tar that reads gzip, so there is nothing new to pin.
//
// The URL is an environment variable, like a repository URL, and the token a
// file from the build's own Secret, like a repository token, sent as a header
// rather than in the URL so it is never in an access log. The engine checked
// the archive when it was uploaded — no path outside the directory, no
// devices — so unpacking it here is unpacking something already looked at.
func fetchStep(req orchestrator.BuildRequest, withToken bool) corev1.Container {
	env := []corev1.EnvVar{{Name: "SOURCE_URL", Value: req.SourceURL}}
	mounts := []corev1.VolumeMount{{Name: buildVolume, MountPath: "/workspace"}}
	if withToken {
		env = append(env, corev1.EnvVar{Name: "SOURCE_TOKEN_FILE", Value: repoCredentialPath + "/" + repoTokenKey})
		mounts = append(mounts, corev1.VolumeMount{
			Name: repoCredentialVolume, MountPath: repoCredentialPath, ReadOnly: true,
		})
	}
	return corev1.Container{
		Name:    cloneContainer,
		Image:   gitImage,
		Env:     env,
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
echo "Fetching the uploaded source"
mkdir -p /workspace/src
if ! wget -q -T 60 --header "Authorization: Bearer $(cat "$SOURCE_TOKEN_FILE")"     -O /workspace/source.tar.gz "$SOURCE_URL"; then
  echo "Could not fetch the uploaded source from the engine — a build reaches it at its base URL."
  exit 1
fi
tar -xzf /workspace/source.tar.gz -C /workspace/src
rm -f /workspace/source.tar.gz
` + shareSourceScript + `
echo "Fetched $(find /workspace/src -type f | wc -l | tr -d ' ') files"
`},
		SecurityContext: hardened(buildUID),
		VolumeMounts:    mounts,
	}
}

// dockerfileStep builds a Dockerfile with rootless BuildKit, if there is one.
//
// --oci-worker-no-process-sandbox is what lets BuildKit run without the
// privileges it would otherwise need to set up its own sandbox. The trade is
// stated rather than hidden: builds are less isolated from each other than
// they would be under a privileged daemon, which is why this namespace runs
// nothing but these Jobs.
func dockerfileStep(req orchestrator.BuildRequest, withArgs bool) corev1.Container {
	unconfined := corev1.SeccompProfileTypeUnconfined
	user := int64(buildUID)
	group := int64(cnbGID)
	mounts := []corev1.VolumeMount{
		{Name: buildVolume, MountPath: "/workspace"},
		{Name: "registry", MountPath: "/registry", ReadOnly: true},
	}
	if withArgs {
		mounts = append(mounts, corev1.VolumeMount{
			Name: buildArgsVolume, MountPath: buildArgsPath, ReadOnly: true,
		})
	}

	return corev1.Container{
		Name:  dockerfileContainer,
		Image: buildkitImage,
		Env: []corev1.EnvVar{
			{Name: "IMAGE", Value: req.Image},
			{Name: "SUBDIR", Value: req.Subdir},
			// An environment variable rather than spliced into the script,
			// like the repository URL: it is somebody's input.
			{Name: "DOCKERFILE", Value: req.Dockerfile},
			{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
			{Name: "DOCKER_CONFIG", Value: "/registry"},
			// An empty value leaves the output option off entirely, so the
			// secure path carries no trace of the insecure one.
			{Name: "INSECURE", Value: insecureOutput(req)},
			// The host, separately, because a plain-HTTP registry has to be
			// declared to the daemon as well: the output option covers the
			// push, and pulling from one is daemon configuration.
			{Name: "INSECURE_HOST", Value: insecureHost(req)},
			// Where the variables are, not what they are. Set whether or not
			// there are any, so the script reads one way.
			{Name: "BUILD_ARGS", Value: buildArgsPath},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
# A plain-HTTP registry has to be declared to buildkitd, not just to the
# output. Without this the push works — it carries its own option — and every
# pull from that registry fails, which is a confusing pair of behaviours to
# debug from the outside.
if [ -n "$INSECURE_HOST" ]; then
  printf '[registry."%s"]\n  http = true\n' "$INSECURE_HOST" > /tmp/buildkitd.toml
  export BUILDKITD_FLAGS="$BUILDKITD_FLAGS --config /tmp/buildkitd.toml"
fi

cd /workspace/src/$SUBDIR
# A Dockerfile somebody named is built or the build fails; only the default
# falls back to buildpacks, because only there is "no Dockerfile" a choice.
if [ -n "$DOCKERFILE" ]; then
  if [ ! -f "$DOCKERFILE" ]; then
    echo "There is no $DOCKERFILE in this repository — check the Dockerfile path in Settings."
    exit 1
  fi
  dockerfile="$DOCKERFILE"
elif [ -f Dockerfile ]; then
  dockerfile=Dockerfile
else
  echo "No Dockerfile here — falling back to buildpacks."
  exit 0
fi
echo "Building $dockerfile and pushing $IMAGE"
` + buildArgsScript + `
# The context stays the directory being built; only where the Dockerfile is
# read from moves, which is what a Dockerfile path means everywhere else.
buildctl-daemonless.sh build \
  --frontend dockerfile.v0 \
  --local context=. \
  --local dockerfile="$(dirname "$dockerfile")" \
  --opt filename="$(basename "$dockerfile")" \
  --output type=image,name=$IMAGE,push=true$INSECURE \
  --progress plain \
  "$@"
touch ` + dockerfileMarker + `
echo "Pushed $IMAGE"
` + resolveUIDScript + `
`},
		// This is BuildKit's own documented Kubernetes configuration for the
		// rootless image, and the deviations from it were tried and reverted
		// rather than assumed to be safe improvements.
		//
		// Unconfined seccomp is required: rootlesskit cannot fork under
		// RuntimeDefault, which fails with "fork/exec /proc/self/exe:
		// operation not permitted". That single requirement is what forces
		// this namespace to "privileged", since baseline forbids Unconfined.
		//
		// allowPrivilegeEscalation stays true and capabilities are not
		// dropped, which looks like an omission and is not. Rootless BuildKit
		// sets up its user namespace with newuidmap, a setuid helper; denying
		// privilege escalation sets no_new_privs and the build dies with
		// "newuidmap: operation not permitted". Dropping ALL takes CAP_SETUID
		// out of the bounding set and breaks the same call a second way. The
		// clone and buildpack steps take both, because they need neither.
		//
		// AppArmor is unconfined for the same class of reason: on a node that
		// enforces it, the default profile blocks the mounts BuildKit makes.
		// It is a no-op where AppArmor is not enforced.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:       &user,
			RunAsGroup:      &group,
			SeccompProfile:  &corev1.SeccompProfile{Type: unconfined},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		},
		VolumeMounts: mounts,
	}
}

// hardened is the security context for the steps that need nothing special.
//
// Only the BuildKit step needs the seccomp exception; the clone and the
// buildpack lifecycle are ordinary programs. Giving them the same relaxation
// would widen it for no reason.
func hardened(user int64) *corev1.SecurityContext {
	return hardenedAs(user, cnbGID)
}

func hardenedAs(uid, gid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                &uid,
		RunAsGroup:               &gid,
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }

// itoa keeps the lifecycle flags built from the same constants the security
// context uses, so the two cannot drift apart.
func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// The lifecycle's exit codes for a detection that nothing passed: 20 when
// every buildpack declined, 21 when one of them also failed while looking.
const (
	cnbFailedDetect           = 20
	cnbFailedDetectWithErrors = 21
)

// staticSiteScript serves a repository nothing recognised, with Apache httpd,
// when it is a website: an index.html at the top, or in public/.
//
// The builder has Paketo's web-servers buildpack, and it is the one that
// declines here: it serves a directory only when asked to, with
// BP_WEB_SERVER, because a static site has no file that says it is one the
// way package.json says Node. Asking for it up front is not an option either.
// web-servers comes before Node.js in the builder's order, so setting it for
// every build would turn a Node app with a public/index.html into a web
// server that never runs the app. So it is asked for only once detection has
// failed without it — a static site is what is left, which is how Railpack
// treats one too.
//
// httpd rather than nginx, which is the buildpack's other server and the
// obvious first choice, because of the read-only root filesystem every app
// runs with. Paketo's nginx writes its configuration into the app directory
// when the container starts, to put $PORT in it, and fails there with "open
// /workspace/src/nginx.conf: read-only file system". Its httpd writes the
// configuration at build time and Apache reads ${PORT} itself, so nothing is
// written at launch. The same variables tune either server
// (BP_WEB_SERVER_ENABLE_PUSH_STATE for a single-page app, BP_WEB_SERVER_ROOT),
// and they are the app's own variables, so a team sets them like any other.
//
// A variable the app already sets is left as it is. And when nothing is
// recognised even then, the build says what would be, in one line: the
// lifecycle's own error is a page of every group it tried, ending in none.
var staticSiteScript = `
if [ "$code" -eq ` + itoa(cnbFailedDetect) + ` ] || [ "$code" -eq ` + itoa(cnbFailedDetectWithErrors) + ` ]; then
  root=""
  if [ -f "/workspace/src/$SUBDIR/index.html" ]; then
    root=.
  elif [ -f "/workspace/src/$SUBDIR/public/index.html" ]; then
    root=public
  fi
  if [ -n "$root" ] && [ ! -f /workspace/platform/env/BP_WEB_SERVER ]; then
    echo "No language was recognised, but there is an index.html: serving it as a static site with Apache httpd."
    printf httpd > /workspace/platform/env/BP_WEB_SERVER
    if [ ! -f /workspace/platform/env/BP_WEB_SERVER_ROOT ]; then
      printf %s "$root" > /workspace/platform/env/BP_WEB_SERVER_ROOT
    fi
    # The first try left its analysis behind; this one starts clean.
    rm -rf /workspace/layers
    mkdir -p /workspace/layers
    code=0
    create || code=$?
  fi
fi
if [ "$code" -eq ` + itoa(cnbFailedDetect) + ` ] || [ "$code" -eq ` + itoa(cnbFailedDetectWithErrors) + ` ]; then
  echo "Nothing in this repository was recognised. Add a Dockerfile, or a Procfile naming the command that starts the app; a static site needs an index.html at the top or in public/."
fi`

// nodeEnvScript builds with NODE_ENV=production unless the app sets
// NODE_ENV itself.
//
// Paketo's npm-install sets NODE_ENV=development in the build environment of
// every buildpack after it, so "npm run build" runs in development. Next.js
// then fails its prerender with "<Html> should not be imported outside of
// pages/_document", and a repository that builds everywhere else, Railway
// included, fails here unless somebody thinks to add a variable. A platform
// variable outranks a buildpack's, so setting it here is what turns that
// around.
//
// It does not cost the build its devDependencies, which is what NODE_ENV=
// production would mean to a plain "npm ci": npm-install and yarn-install
// both install the build's modules with development dependencies whatever
// NODE_ENV says, and the app's runtime gets production from the Node.js
// buildpacks anyway. Other languages never read it.
const nodeEnvScript = `if [ ! -f /workspace/platform/env/NODE_ENV ]; then
  printf production > /workspace/platform/env/NODE_ENV
fi`

// buildpackStep builds a repository that has no Dockerfile.
//
// No uid probe at the end, unlike the Dockerfile step. The probe is a
// BuildKit build, and the builder image has no BuildKit, so all it ever did
// here was end a good build with "Could not work out which user this image
// runs as". Nor is it needed: the lifecycle gives the image the run image's
// user, which Paketo's sets as numbers (1002:1000), and Kubernetes can tell
// those are not root without asking the image.
//
// The lifecycle runs inside the builder image rather than on its own, because
// the buildpacks that detect what the code is live in that image — a lifecycle
// with no buildpacks detects nothing.
//
// It exits immediately when the Dockerfile step already pushed. Every
// container in a pod runs, so "did the previous step handle this" is a
// question this one has to ask rather than something the pod can express.
func buildpackStep(req orchestrator.BuildRequest, withArgs bool) corev1.Container {
	mounts := []corev1.VolumeMount{
		{Name: buildVolume, MountPath: "/workspace"},
		{Name: "registry", MountPath: "/registry", ReadOnly: true},
	}
	if withArgs {
		mounts = append(mounts, corev1.VolumeMount{
			Name: buildArgsVolume, MountPath: buildArgsPath, ReadOnly: true,
		})
	}
	return corev1.Container{
		Name:  buildpackContainer,
		Image: packImage,
		Env: []corev1.EnvVar{
			{Name: "IMAGE", Value: req.Image},
			{Name: "SUBDIR", Value: req.Subdir},
			// The lifecycle reads registry credentials from this, the same
			// place BuildKit does, so one secret serves both.
			{Name: "DOCKER_CONFIG", Value: "/registry"},
			// The platform API the lifecycle is asked to speak. Pinned rather
			// than left to default, because the builder image and the
			// lifecycle inside it move independently of this code.
			{Name: "CNB_PLATFORM_API", Value: "0.13"},
			// The lifecycle takes its insecure registries as a list rather
			// than a flag on the output, so the same fact is spelled twice.
			{Name: "CNB_INSECURE_REGISTRIES", Value: insecureHost(req)},
			{Name: "BUILD_ARGS", Value: buildArgsPath},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
if [ -f ` + dockerfileMarker + ` ]; then
  echo "Already built from the Dockerfile."
  exit 0
fi
echo "No Dockerfile — detecting what this is with buildpacks."
# The lifecycle writes layers, a cache and a platform directory, and refuses
# to guess where. They go on the shared volume rather than the image's own
# filesystem, which is read-only to a non-root user.
mkdir -p /workspace/layers /workspace/cache /workspace/platform/env
# Every variable, as the platform environment: one file a variable, which is
# how the buildpacks spec gives a build its environment. Which of them a
# buildpack reads is up to the buildpack.
for f in "$BUILD_ARGS"/*; do
  if [ -f "$f" ]; then
    cat "$f" > "/workspace/platform/env/${f##*/}"
  fi
done
` + nodeEnvScript + `
echo "` + buildArgsMarker + orchestrator.BuildArgsAll + `"
create() {
  /cnb/lifecycle/creator \
    -app="/workspace/src/$SUBDIR" \
    -layers=/workspace/layers \
    -cache-dir=/workspace/cache \
    -platform=/workspace/platform \
    -uid=` + itoa(cnbUID) + ` -gid=` + itoa(cnbGID) + ` \
    -log-level=info \
    "$IMAGE"
}
code=0
create || code=$?
` + staticSiteScript + `
if [ "$code" -ne 0 ]; then
  exit "$code"
fi
echo "Pushed $IMAGE"
`},
		SecurityContext: hardenedAs(cnbUID, cnbGID),
		VolumeMounts:    mounts,
	}
}
