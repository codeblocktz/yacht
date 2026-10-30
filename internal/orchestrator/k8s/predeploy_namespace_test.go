package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// refuseMissingNamespaces makes the fake clientset answer as an API server
// does: a write into a namespace that does not exist is refused. The fake
// itself accepts one, which is how a pre-deploy run that came before its
// namespace passed every test here and failed on a cluster.
func refuseMissingNamespaces(client *fake.Clientset) {
	client.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ns := a.GetNamespace()
		switch a.GetVerb() {
		case "create", "update", "patch":
		default:
			return false, nil, nil
		}
		if ns == "" || a.GetResource().Resource == "namespaces" {
			return false, nil, nil
		}
		_, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("namespaces"), "", ns)
		if apierrors.IsNotFound(err) {
			return true, nil, apierrors.NewNotFound(corev1.Resource("namespaces"), ns)
		}
		return false, nil, nil
	})
}

// completePreDeploy plays the Job controller for a run that succeeds.
func completePreDeploy(ctx context.Context, t *testing.T, client *fake.Clientset, namespace, name string) {
	for {
		job, err := client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			})
			if _, err := client.BatchV1().Jobs(namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
				t.Errorf("finish job: %v", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A pre-deploy run writes into the app's namespace before anything of the app
// does, so on a first deploy it is refused unless the namespace was made
// first — the error this was seen as. Made first, with the same EnsureNamespace
// an apply uses, the run has everything it needs there and starts nothing of
// the app's own.
func TestAFirstPreDeployRunsInTheNamespaceItWasGiven(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o, client := testOrchestrator(t)
	refuseMissingNamespaces(client)
	spec := preDeploySpec()
	spec.RegistryAuth = []byte(`{"auths":{}}`)
	req := orchestrator.PreDeployRequest{App: spec, Command: "bin/migrate"}

	_, err := o.PreDeploy(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "store the pull credential") {
		t.Fatalf("PreDeploy with no namespace = %v, want the pull credential refused", err)
	}

	if err := o.EnsureNamespace(ctx, orchestrator.NamespaceSpec{
		Owner: spec.Owner, Name: spec.Namespace,
	}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	go completePreDeploy(ctx, t, client, spec.Namespace, preDeployJobName(spec))
	result, err := o.PreDeploy(ctx, req)
	if err != nil || !result.Succeeded {
		t.Fatalf("PreDeploy = %+v, %v; want it to run and succeed", result, err)
	}

	if _, err := client.CoreV1().Secrets(spec.Namespace).
		Get(ctx, orchestrator.PullSecretName, metav1.GetOptions{}); err != nil {
		t.Errorf("pull secret: %v", err)
	}
	if _, err := client.CoreV1().LimitRanges(spec.Namespace).
		Get(ctx, limitRangeName, metav1.GetOptions{}); err != nil {
		t.Errorf("limit range: %v", err)
	}
	policies, err := client.NetworkingV1().NetworkPolicies(spec.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(policies.Items) == 0 {
		t.Errorf("network policies = %v, %v; want the namespace isolated", policies, err)
	}
	deployments, err := client.AppsV1().Deployments(spec.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(deployments.Items) != 0 {
		t.Errorf("deployments = %d, %v; want none until the apply", len(deployments.Items), err)
	}
}
