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
			// Nothing ready and not getting there is failing, not pending.
			if want := map[bool]orchestrator.Phase{true: orchestrator.PhaseFailing, false: orchestrator.PhasePending}[tc.terminal]; st.Phase != want {
				t.Errorf("phase = %s, want %s", st.Phase, want)
			}
		})
	}
}

// A container that crashes on every start is ready for an instant each time,
// and a look in that instant found as many ready as wanted and skipped the
// crash check: the rollout read as running. Ready is not available, and the
// crash is looked for until every replica is available.
func TestACrashLoopCaughtReadyStillFails(t *testing.T) {
	o, client := testOrchestrator(t)
	spec := testSpec()
	one := int32(1)
	ctx := context.Background()
	if _, err := client.AppsV1().Deployments(spec.Namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace,
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r2"}},
		Spec:   appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs := waiting("CrashLoopBackOff", 4, &corev1.ContainerStateTerminated{ExitCode: 9, Reason: "Error"})
	if _, err := client.CoreV1().Pods(spec.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name + "-abc", Namespace: spec.Namespace,
			Labels:      orchestrator.ObjectLabels(spec.Ref),
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r2"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := o.AppStatus(ctx, spec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Terminal || st.Reason != "CrashLoopBackOff" {
		t.Fatalf("terminal = %v (%s: %s), want a failed rollout", st.Terminal, st.Reason, st.Message)
	}
	if st.Restarts != 4 || st.LastExit != "code 9 (Error)" {
		t.Errorf("restarts = %d, last exit %q; want 4, code 9 (Error)", st.Restarts, st.LastExit)
	}
}

// A redeploy whose new pod crash-loops while the old one still serves: the
// Deployment counts the old pod as ready and available, which hid the crash
// from a check that looked only while fewer were ready than wanted. Every pod
// is counted, so the rollout can be seen not to be through.
func TestACrashLoopBesideAnOldPodStillFails(t *testing.T) {
	o, client := testOrchestrator(t)
	spec := testSpec()
	one := int32(1)
	ctx := context.Background()
	if _, err := client.AppsV1().Deployments(spec.Namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace,
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r2"}},
		Spec: appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		name, release string
		cs            corev1.ContainerStatus
	}{
		{"old", "r1", corev1.ContainerStatus{Name: "app", Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		{"new", "r2", waiting("CrashLoopBackOff", 3, &corev1.ContainerStateTerminated{ExitCode: 127, Reason: "Error"})},
	} {
		if _, err := client.CoreV1().Pods(spec.Namespace).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: spec.Name + "-" + p.name, Namespace: spec.Namespace,
				Labels:      orchestrator.ObjectLabels(spec.Ref),
				Annotations: map[string]string{orchestrator.AnnotationReleaseID: p.release}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{p.cs}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := o.AppStatus(ctx, spec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pods != 2 || st.Updated != 1 {
		t.Errorf("pods = %d, updated %d; want 2, 1", st.Pods, st.Updated)
	}
	if !st.Terminal || st.Reason != "CrashLoopBackOff" {
		t.Fatalf("terminal = %v (%s: %s), want the new pod's crash loop", st.Terminal, st.Reason, st.Message)
	}
	if st.Restarts != 3 {
		t.Errorf("restarts = %d, want the new pod's 3 only", st.Restarts)
	}
}

// A release that only scales keeps the running pods and their stamp, the
// release that made them (see CONFIG-9): the Deployment says the scale's
// release, the template and the pods the one before. All its pods are the
// one ReplicaSet's, so none is an old revision's, and its pods are found by
// the template's stamp, for their restarts and for a crash loop among them.
func TestAScaleOnlyReleaseReadsTheRunningPods(t *testing.T) {
	for _, tc := range []struct {
		name     string
		crashing bool
	}{{"all up", false}, {"one crash-looping", true}} {
		t.Run(tc.name, func(t *testing.T) {
			o, client := testOrchestrator(t)
			spec := testSpec()
			three := int32(3)
			ctx := context.Background()
			ready := int32(3)
			if tc.crashing {
				ready = 2
			}
			if _, err := client.AppsV1().Deployments(spec.Namespace).Create(ctx, &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace,
					Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r3-scale"}},
				Spec: appsv1.DeploymentSpec{Replicas: &three, Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r2"}}}},
				Status: appsv1.DeploymentStatus{Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: ready, AvailableReplicas: ready,
					Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			for i, name := range []string{"a", "b", "c"} {
				cs := corev1.ContainerStatus{Name: "app", Ready: true, RestartCount: 1,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
				if tc.crashing && i == 2 {
					cs = waiting("CrashLoopBackOff", 3, &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"})
				}
				if _, err := client.CoreV1().Pods(spec.Namespace).Create(ctx, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: spec.Name + "-" + name, Namespace: spec.Namespace,
						Labels:      orchestrator.ObjectLabels(spec.Ref),
						Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r2"}},
					Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			st, err := o.AppStatus(ctx, spec.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if st.ReleaseID != "r3-scale" || st.Pods != 3 || st.Updated != 3 {
				t.Errorf("release %s, pods %d, updated %d; want r3-scale, 3, 3", st.ReleaseID, st.Pods, st.Updated)
			}
			if tc.crashing {
				if !st.Terminal || st.Reason != "CrashLoopBackOff" {
					t.Fatalf("terminal = %v (%s), want the running pods' crash loop found", st.Terminal, st.Reason)
				}
				return
			}
			if st.Terminal || st.Restarts != 3 {
				t.Fatalf("terminal %v, restarts %d; want a healthy scale with the running pods' 3 restarts", st.Terminal, st.Restarts)
			}
		})
	}
}

// A crash loop that outlasts a short healthcheck timeout, so the progress
// deadline is passed too: it is still a crash, not a version that was slow
// to listen.
func TestACrashLoopPastTheProgressDeadlineIsStillACrash(t *testing.T) {
	o, client := testOrchestrator(t)
	spec := testSpec()
	one := int32(1)
	ctx := context.Background()
	if _, err := client.AppsV1().Deployments(spec.Namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace,
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r2"}},
		Spec: appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
			Reason: orchestrator.ReasonProgressDeadlineExceeded, Message: "ReplicaSet timed out",
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs := waiting("CrashLoopBackOff", 4, &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"})
	if _, err := client.CoreV1().Pods(spec.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name + "-abc", Namespace: spec.Namespace,
			Labels:      orchestrator.ObjectLabels(spec.Ref),
			Annotations: map[string]string{orchestrator.AnnotationReleaseID: "r2"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := o.AppStatus(ctx, spec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Terminal || st.Reason != "CrashLoopBackOff" {
		t.Fatalf("status = %v (%s: %s), want the crash loop", st.Terminal, st.Reason, st.Message)
	}
}
