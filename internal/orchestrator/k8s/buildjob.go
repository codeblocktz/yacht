package k8s

import (
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
		VolumeMounts: []corev1.VolumeMount{{Name: buildVolume, MountPath: "/workspace"}},
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
	user := int64(1000)

	return corev1.Container{
		Name:  dockerfileContainer,
		Image: buildkitImage,
		Env: []corev1.EnvVar{
			{Name: "IMAGE", Value: req.Image},
			{Name: "SUBDIR", Value: req.Subdir},
			{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
			{Name: "DOCKER_CONFIG", Value: "/registry"},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
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
  --output type=image,name=$IMAGE,push=true \
  --progress plain
touch ` + dockerfileMarker + `
echo "Pushed $IMAGE"
`},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:      &user,
			RunAsGroup:     &user,
			SeccompProfile: &corev1.SeccompProfile{Type: unconfined},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: buildVolume, MountPath: "/workspace"},
			{Name: "registry", MountPath: "/registry", ReadOnly: true},
		},
	}
}

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
	user := int64(1000)

	return corev1.Container{
		Name:  buildpackContainer,
		Image: packImage,
		Env: []corev1.EnvVar{
			{Name: "IMAGE", Value: req.Image},
			{Name: "SUBDIR", Value: req.Subdir},
			{Name: "DOCKER_CONFIG", Value: "/registry"},
			{Name: "CNB_PLATFORM_API", Value: "0.13"},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
if [ -f ` + dockerfileMarker + ` ]; then
  echo "Already built from the Dockerfile."
  exit 0
fi
echo "No Dockerfile — detecting what this is with buildpacks."
/cnb/lifecycle/creator \
  -app=/workspace/src/$SUBDIR \
  -log-level=info \
  "$IMAGE"
echo "Pushed $IMAGE"
`},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  &user,
			RunAsGroup: &user,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: buildVolume, MountPath: "/workspace"},
			{Name: "registry", MountPath: "/registry", ReadOnly: true},
		},
	}
}
