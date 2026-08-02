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
func buildJob(name string, req orchestrator.BuildRequest, pullSecret string) *batchv1.Job {
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
					Volumes: []corev1.Volume{
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
					},
					InitContainers: []corev1.Container{
						cloneStep(req),
						dockerfileStep(req),
					},
					Containers: []corev1.Container{buildpackStep(req)},
				},
			},
		},
	}
}

// cloneStep fetches the repository.
//
// The URL and ref are passed as environment variables rather than interpolated
// into the script. A repository URL is somebody's input, and a value spliced
// into a shell command is a command they get to extend — validation upstream
// makes that unlikely, and this makes it impossible.
func cloneStep(req orchestrator.BuildRequest) corev1.Container {
	return corev1.Container{
		Name:  cloneContainer,
		Image: gitImage,
		Env: []corev1.EnvVar{
			{Name: "REPO_URL", Value: req.RepoURL},
			{Name: "REPO_REF", Value: req.Ref},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
echo "Cloning $REPO_URL at $REPO_REF"
git clone --depth 1 --branch "$REPO_REF" --single-branch "$REPO_URL" /workspace/src
echo "` + commitMarker + `$(git -C /workspace/src rev-parse HEAD)"
echo "Cloned $(git -C /workspace/src rev-parse --short HEAD)"
`},
		SecurityContext: hardened(buildUID),
		VolumeMounts:    []corev1.VolumeMount{{Name: buildVolume, MountPath: "/workspace"}},
	}
}

// dockerfileStep builds a Dockerfile with rootless BuildKit, if there is one.
//
// --oci-worker-no-process-sandbox is what lets BuildKit run without the
// privileges it would otherwise need to set up its own sandbox. The trade is
// stated rather than hidden: builds are less isolated from each other than
// they would be under a privileged daemon, which is why this namespace runs
// nothing but these Jobs.
func dockerfileStep(req orchestrator.BuildRequest) corev1.Container {
	unconfined := corev1.SeccompProfileTypeUnconfined
	user := int64(buildUID)
	group := int64(cnbGID)

	return corev1.Container{
		Name:  dockerfileContainer,
		Image: buildkitImage,
		Env: []corev1.EnvVar{
			{Name: "IMAGE", Value: req.Image},
			{Name: "SUBDIR", Value: req.Subdir},
			{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
			{Name: "DOCKER_CONFIG", Value: "/registry"},
			// An empty value leaves the output option off entirely, so the
			// secure path carries no trace of the insecure one.
			{Name: "INSECURE", Value: insecureOutput(req)},
			// The host, separately, because a plain-HTTP registry has to be
			// declared to the daemon as well: the output option covers the
			// push, and pulling from one is daemon configuration.
			{Name: "INSECURE_HOST", Value: insecureHost(req)},
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
if [ ! -f Dockerfile ]; then
  echo "No Dockerfile here — falling back to buildpacks."
  exit 0
fi
echo "Building the Dockerfile and pushing $IMAGE"
buildctl-daemonless.sh build \
  --frontend dockerfile.v0 \
  --local context=. \
  --local dockerfile=. \
  --output type=image,name=$IMAGE,push=true$INSECURE \
  --progress plain
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
		VolumeMounts: []corev1.VolumeMount{
			{Name: buildVolume, MountPath: "/workspace"},
			{Name: "registry", MountPath: "/registry", ReadOnly: true},
		},
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

// buildpackStep builds a repository that has no Dockerfile.
//
// The lifecycle runs inside the builder image rather than on its own, because
// the buildpacks that detect what the code is live in that image — a lifecycle
// with no buildpacks detects nothing.
//
// It exits immediately when the Dockerfile step already pushed. Every
// container in a pod runs, so "did the previous step handle this" is a
// question this one has to ask rather than something the pod can express.
func buildpackStep(req orchestrator.BuildRequest) corev1.Container {

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
/cnb/lifecycle/creator \
  -app="/workspace/src/$SUBDIR" \
  -layers=/workspace/layers \
  -cache-dir=/workspace/cache \
  -platform=/workspace/platform \
  -uid=` + itoa(cnbUID) + ` -gid=` + itoa(cnbGID) + ` \
  -log-level=info \
  "$IMAGE"
echo "Pushed $IMAGE"
` + resolveUIDScript + `
`},
		SecurityContext: hardenedAs(cnbUID, cnbGID),
		VolumeMounts: []corev1.VolumeMount{
			{Name: buildVolume, MountPath: "/workspace"},
			{Name: "registry", MountPath: "/registry", ReadOnly: true},
		},
	}
}
