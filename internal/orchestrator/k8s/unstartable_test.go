package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// rolloutWith is a Deployment of release "r2", one replica wanted and none
// ready, and a pod of the given release in the given container state.
func rolloutWith(t *testing.T, podRelease string, cs corev1.ContainerStatus) orchestrator.AppStatus {
	t.Helper()
	o, client := testOrchestrator(t)
	spec := testSpec()
	one := int32(1)
	ctx := context.Background()
	if _, err := client.AppsV1().Deployments(spec.Namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace,
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r2"}},
		Spec: appsv1.DeploymentSpec{Replicas: &one},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods(spec.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name + "-abc", Namespace: spec.Namespace,
			Labels:      orchestrator.ObjectLabels(spec.Ref),
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: podRelease}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	status, err := o.AppStatus(ctx, spec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func waiting(reason string, restarts int32, last *corev1.ContainerStateTerminated) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: "app", RestartCount: restarts,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: reason + " happened"}},
		LastTerminationState: corev1.ContainerState{Terminated: last},
	}
}

// A container that cannot start fails its rollout at once, saying why.
func TestARolloutWhoseContainerCannotStartFailsAtOnce(t *testing.T) {
	startErr := &corev1.ContainerStateTerminated{ExitCode: 128, Reason: "StartError",
		Message: `exec: "/app/bin/server": permission denied`}
	for _, tc := range []struct {
		name     string
		release  string
		cs       corev1.ContainerStatus
		terminal bool
		says     string
	}{
		{"its start script not executable", "r2", waiting("CrashLoopBackOff", 1, startErr), true, "permission denied"},
		{"its config missing", "r2", waiting("CreateContainerConfigError", 0, nil), true, "CreateContainerConfigError"},
		{"crashing every start", "r2", waiting("CrashLoopBackOff", 3, &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}), true, "exited 3 times"},
		{"one crash, maybe a database still starting", "r2", waiting("CrashLoopBackOff", 1, &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}), false, ""},
		{"pulling its image", "r2", waiting("ContainerCreating", 0, nil), false, ""},
		{"an old release's trouble", "r1", waiting("CreateContainerConfigError", 0, nil), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := rolloutWith(t, tc.release, tc.cs)
			if st.Terminal != tc.terminal {
				t.Fatalf("terminal = %v, want %v (%s: %s)", st.Terminal, tc.terminal, st.Reason, st.Message)
			}
			if tc.says != "" && !strings.Contains(st.Reason+": "+st.Message, tc.says) {
				t.Errorf("says %q: %q, want %q", st.Reason, st.Message, tc.says)
			}
		})
	}
}
