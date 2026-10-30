package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

const testRepoToken = "ghs_0123456789abcdefTOKEN"

func tokenBuild() orchestrator.BuildRequest {
	return orchestrator.BuildRequest{
		Owner: "owner-1", App: "web", Image: "registry.test/owner-1/web:d1",
		RepoURL: "https://github.com/acme/web.git", Ref: "main",
		RegistryAuth: []byte(`{"auths":{}}`), RepoToken: testRepoToken,
	}
}

// A private repository's token reaches the clone as a file from a Secret of
// the build's own — not in the Job, its arguments or its environment, which
// anyone who can describe a pod can read — and only the clone mounts it.
func TestTheRepositoryTokenReachesOnlyTheCloneThroughASecret(t *testing.T) {
	o, client := testOrchestrator(t)
	var job *batchv1.Job
	var secret *corev1.Secret
	client.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		job = a.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		return false, nil, nil
	})
	client.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret); strings.HasSuffix(s.Name, "-repo") {
			secret = s.DeepCopy()
		}
		return false, nil, nil
	})

	// No pod ever starts on a fake cluster, so the build waits until this
	// gives up, and then cleans up after itself as it would after a real one.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := o.Build(ctx, tokenBuild())
	if err == nil {
		t.Fatal("a build with no pod succeeded")
	}
	if strings.Contains(err.Error(), testRepoToken) {
		t.Fatalf("the token is in the build's error: %v", err)
	}

	if job == nil || secret == nil {
		t.Fatalf("job %v, secret %v — want both created", job != nil, secret != nil)
	}
	if string(secret.Data[repoTokenKey]) != testRepoToken {
		t.Fatalf("the Secret holds %q", secret.Data)
	}
	spec, _ := json.Marshal(job)
	if strings.Contains(string(spec), testRepoToken) {
		t.Fatal("the token is in the Job")
	}

	pod := job.Spec.Template.Spec
	var vol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == repoCredentialVolume {
			vol = &pod.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != secret.Name ||
		vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o440 {
		t.Fatalf("credential volume = %+v", vol)
	}
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == repoCredentialVolume {
				mounted = m.ReadOnly
			}
		}
		if (c.Name == cloneContainer) != mounted {
			t.Errorf("%s mounts the credential: %t — only the clone may, read-only", c.Name, mounted)
		}
	}
	clone := pod.InitContainers[0]
	var tokenFile string
	for _, e := range clone.Env {
		if e.Name == "REPO_TOKEN_FILE" {
			tokenFile = e.Value
		}
	}
	if tokenFile != repoCredentialPath+"/"+repoTokenKey ||
		!strings.Contains(clone.Args[0], "credential.helper") ||
		strings.Contains(clone.Args[0], "x-access-token:") {
		t.Fatalf("the clone does not read the token through a credential helper:\n%s", clone.Args[0])
	}

	// Gone with the build.
	if _, err := client.CoreV1().Secrets(BuildNamespace).
		Get(context.Background(), secret.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the token's Secret outlived the build: %v", err)
	}
}

// A public repository is cloned as before: no Secret, no mount.
func TestAPublicRepositoryClonesWithNoCredential(t *testing.T) {
	req := tokenBuild()
	req.RepoToken = ""
	job := buildJob("build-x", req, buildRegistrySecret, "", "")
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == repoCredentialVolume {
			t.Fatal("a public repository's build mounts a credential")
		}
	}
	for _, e := range job.Spec.Template.Spec.InitContainers[0].Env {
		if e.Name == "REPO_TOKEN_FILE" {
			t.Fatal("a public repository's clone is told of a token")
		}
	}
}

func TestTheTokenIsRedactedFromLogAndError(t *testing.T) {
	var got string
	logf := redactingLog(func(s string) { got += s }, testRepoToken)
	logf("fatal: https://x-access-token:" + testRepoToken + "@github.com/acme/web.git\n")
	if strings.Contains(got, testRepoToken) || !strings.Contains(got, "***") {
		t.Fatalf("log = %q", got)
	}
	err := redactError(errors.New("clone failed for "+testRepoToken), testRepoToken)
	if strings.Contains(err.Error(), testRepoToken) {
		t.Fatalf("error = %v", err)
	}
}
