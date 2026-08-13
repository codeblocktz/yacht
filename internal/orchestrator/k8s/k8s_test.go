package k8s

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func testOrchestrator(t *testing.T) (*Orchestrator, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWithClient(client, log), client
}

func testSpec() orchestrator.AppSpec {
	return orchestrator.AppSpec{
		Ref: orchestrator.Ref{
			Owner:     "owner-1",
			Namespace: "yacht-demo",
			Name:      "web",
		},
		ReleaseID: "11111111-1111-1111-1111-111111111111", ConfigVersion: 7,
		Image:    "ghcr.io/example/web:v1",
		Replicas: 2,
		Port:     8080,
		Env:      map[string]string{"LOG_LEVEL": "info", "APP_ENV": "prod"},
	}
}

// TestEnsureNamespace is half of the smallest loop: a namespace that is safe by
// construction. The assertions here are the security posture — if any of them
// regress, workloads lose their floor.
func TestEnsureNamespace(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := orchestrator.NamespaceSpec{Owner: "owner-1", Name: "yacht-demo"}
	if err := o.EnsureNamespace(ctx, spec); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}

	ns, err := client.CoreV1().Namespaces().Get(ctx, "yacht-demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}

	// Pod Security Admission is what the cluster enforces, independent of what
	// the control plane chooses to send.
	for key, want := range map[string]string{
		psaEnforce:                  "restricted",
		psaAudit:                    "restricted",
		psaWarn:                     "restricted",
		orchestrator.LabelOwner:     "owner-1",
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
	} {
		if got := ns.Labels[key]; got != want {
			t.Errorf("namespace label %s = %q, want %q", key, got, want)
		}
	}

	// A LimitRange must always exist, so a workload that requests nothing
	// still cannot run unbounded.
	lr, err := client.CoreV1().LimitRanges("yacht-demo").
		Get(ctx, limitRangeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get limitrange: %v", err)
	}
	if len(lr.Spec.Limits) != 1 {
		t.Fatalf("limitrange has %d items, want 1", len(lr.Spec.Limits))
	}
	item := lr.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		t.Errorf("limitrange type = %q, want Container", item.Type)
	}
	if got := item.Default.Cpu().String(); got != "100m" {
		t.Errorf("default cpu = %q, want 100m", got)
	}
	if got := item.Max.Memory().String(); got != "4Gi" {
		t.Errorf("max memory = %q, want 4Gi", got)
	}
}

func TestEnsureNamespaceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := orchestrator.NamespaceSpec{Owner: "owner-1", Name: "yacht-demo"}

	for i := range 3 {
		if err := o.EnsureNamespace(ctx, spec); err != nil {
			t.Fatalf("EnsureNamespace call %d: %v", i+1, err)
		}
	}

	list, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("got %d namespaces after 3 applies, want 1", len(list.Items))
	}
}

// TestApplyApp is the other half of the smallest loop, and the most important
// test in the package: it asserts the hardening the engine promises is
// actually on the object that reaches the cluster.
func TestApplyApp(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	dep, err := client.AppsV1().Deployments("yacht-demo").
		Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %v, want 2", dep.Spec.Replicas)
	}
	if dep.Spec.ProgressDeadlineSeconds == nil ||
		*dep.Spec.ProgressDeadlineSeconds != deploymentProgressDeadline {
		t.Errorf("progress deadline = %v, want %d",
			dep.Spec.ProgressDeadlineSeconds, deploymentProgressDeadline)
	}
	for _, annotations := range []map[string]string{
		dep.Annotations, dep.Spec.Template.Annotations,
	} {
		if got := annotations[orchestrator.AnnotationReleaseID]; got != spec.ReleaseID {
			t.Errorf("release annotation = %q, want %q", got, spec.ReleaseID)
		}
		if got := annotations[orchestrator.AnnotationConfigVersion]; got != "7" {
			t.Errorf("config annotation = %q, want 7", got)
		}
	}
	status, err := o.AppStatus(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("AppStatus: %v", err)
	}
	if status.ReleaseID != spec.ReleaseID || status.ConfigVersion != spec.ConfigVersion {
		t.Fatalf("observed convergence key = %q/%d, want %q/%d",
			status.ReleaseID, status.ConfigVersion, spec.ReleaseID, spec.ConfigVersion)
	}

	podSpec := dep.Spec.Template.Spec
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be explicitly false")
	}
	if podSpec.SecurityContext == nil ||
		podSpec.SecurityContext.RunAsNonRoot == nil ||
		!*podSpec.SecurityContext.RunAsNonRoot {
		t.Error("pod securityContext.runAsNonRoot must be true")
	}

	if len(podSpec.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(podSpec.Containers))
	}
	c := podSpec.Containers[0]

	sc := c.SecurityContext
	switch {
	case sc == nil:
		t.Fatal("container securityContext must be set")
	case sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot:
		t.Error("runAsNonRoot must be true")
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Error("allowPrivilegeEscalation must be false")
	case sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem:
		t.Error("readOnlyRootFilesystem must default to true")
	case sc.Privileged == nil || *sc.Privileged:
		t.Error("privileged must be false")
	case sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 ||
		sc.Capabilities.Drop[0] != corev1.Capability("ALL"):
		t.Errorf("capabilities.drop = %v, want [ALL]", sc.Capabilities)
	case sc.SeccompProfile == nil ||
		sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
		t.Error("seccompProfile must be RuntimeDefault")
	}

	// A read-only root filesystem is only usable if /tmp is writable.
	var foundTmp bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/tmp" {
			foundTmp = true
		}
	}
	if !foundTmp {
		t.Error("expected a writable /tmp mount alongside the read-only root")
	}

	// Environment must be sorted, or every reconcile produces a different pod
	// template and restarts the workload.
	if len(c.Env) != 2 {
		t.Fatalf("got %d env vars, want 2", len(c.Env))
	}
	if c.Env[0].Name != "APP_ENV" || c.Env[1].Name != "LOG_LEVEL" {
		t.Errorf("env not sorted: got %s, %s", c.Env[0].Name, c.Env[1].Name)
	}

	// Selector must match the pod labels, or the Deployment adopts nothing.
	for k, want := range orchestrator.SelectorLabels("web") {
		if got := dep.Spec.Selector.MatchLabels[k]; got != want {
			t.Errorf("selector[%s] = %q, want %q", k, got, want)
		}
		if got := dep.Spec.Template.Labels[k]; got != want {
			t.Errorf("pod label[%s] = %q, want %q", k, got, want)
		}
	}

	// Owner is recorded but deliberately kept out of the immutable selector.
	if _, inSelector := dep.Spec.Selector.MatchLabels[orchestrator.LabelOwner]; inSelector {
		t.Error("owner must not be part of the immutable selector")
	}
	if got := dep.Labels[orchestrator.LabelOwner]; got != "owner-1" {
		t.Errorf("owner label = %q, want owner-1", got)
	}
}

func TestAResourceFailureDoesNotPublishTheConvergenceCommitMarker(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	client.Fake.PrependReactor("patch", "ingresses",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("ingress API unavailable")
		})
	spec := testSpec()
	spec.Hosts = []string{"web.apps.test"}
	if err := o.ApplyApp(ctx, spec); err == nil {
		t.Fatal("ApplyApp succeeded despite the ingress failure")
	}
	dep, err := client.AppsV1().Deployments(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get partially applied deployment: %v", err)
	}
	if got := dep.Annotations[orchestrator.AnnotationReleaseID]; got != "" {
		t.Fatalf("partial apply published release %q", got)
	}
	if got := dep.Annotations[orchestrator.AnnotationConfigVersion]; got != "-1" {
		t.Fatalf("partial apply published config version %q", got)
	}
}

func TestApplyAppCreatesServiceOnlyWhenPortSet(t *testing.T) {
	ctx := context.Background()

	t.Run("with port", func(t *testing.T) {
		o, client := testOrchestrator(t)
		if err := o.ApplyApp(ctx, testSpec()); err != nil {
			t.Fatalf("ApplyApp: %v", err)
		}
		svc, err := client.CoreV1().Services("yacht-demo").
			Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get service: %v", err)
		}
		if svc.Spec.Ports[0].Port != servicePort {
			t.Errorf("service port = %d, want %d", svc.Spec.Ports[0].Port, servicePort)
		}
		if got := svc.Spec.Ports[0].TargetPort.IntValue(); got != 8080 {
			t.Errorf("target port = %d, want 8080", got)
		}
	})

	t.Run("without port", func(t *testing.T) {
		o, client := testOrchestrator(t)
		spec := testSpec()
		spec.Port = 0
		if err := o.ApplyApp(ctx, spec); err != nil {
			t.Fatalf("ApplyApp: %v", err)
		}
		list, err := client.CoreV1().Services("yacht-demo").List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list services: %v", err)
		}
		if len(list.Items) != 0 {
			t.Errorf("got %d services for a portless workload, want 0", len(list.Items))
		}
	})
}

// A repeated apply of an unchanged spec must not change the pod template.
// If it does, every reconcile restarts every workload.
func TestApplyAppIsStable(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	spec := testSpec()

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second, err := client.AppsV1().Deployments("yacht-demo").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	a := first.Spec.Template.Annotations[orchestrator.AnnotationRevision]
	b := second.Spec.Template.Annotations[orchestrator.AnnotationRevision]
	if a == "" {
		t.Fatal("revision annotation missing")
	}
	if a != b {
		t.Errorf("revision changed on unchanged spec: %q -> %q", a, b)
	}
}

func TestSpecHash(t *testing.T) {
	base := testSpec()

	t.Run("stable across env map ordering", func(t *testing.T) {
		other := testSpec()
		other.Env = map[string]string{"APP_ENV": "prod", "LOG_LEVEL": "info"}
		if specHash(base) != specHash(other) {
			t.Error("hash must not depend on map iteration order")
		}
	})

	t.Run("changes with image", func(t *testing.T) {
		other := testSpec()
		other.Image = "ghcr.io/example/web:v2"
		if specHash(base) == specHash(other) {
			t.Error("hash must change when the image changes")
		}
	})

	t.Run("changes with env value", func(t *testing.T) {
		other := testSpec()
		other.Env = map[string]string{"LOG_LEVEL": "debug", "APP_ENV": "prod"}
		if specHash(base) == specHash(other) {
			t.Error("hash must change when an env value changes")
		}
	})
}

func TestAppStatus(t *testing.T) {
	ctx := context.Background()
	ref := orchestrator.Ref{Owner: "owner-1", Namespace: "yacht-demo", Name: "web"}

	t.Run("missing workload", func(t *testing.T) {
		o, _ := testOrchestrator(t)
		_, err := o.AppStatus(ctx, ref)
		if err != orchestrator.ErrNotFound {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	cases := []struct {
		name      string
		desired   int32
		ready     int32
		wantPhase orchestrator.Phase
	}{
		{"all ready", 2, 2, orchestrator.PhaseRunning},
		{"none ready", 2, 0, orchestrator.PhasePending},
		{"partially ready", 3, 1, orchestrator.PhaseDegraded},
		{"scaled to zero", 0, 0, orchestrator.PhaseStopped},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, client := testOrchestrator(t)
			replicas := tc.desired
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: "web", Namespace: "yacht-demo", Generation: 3,
				},
				Spec: appsv1.DeploymentSpec{Replicas: &replicas},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 3,
					UpdatedReplicas:    tc.ready,
					ReadyReplicas:      tc.ready,
					AvailableReplicas:  tc.ready,
					Conditions: []appsv1.DeploymentCondition{{
						Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
					}},
				},
			}
			if _, err := client.AppsV1().Deployments("yacht-demo").
				Create(ctx, dep, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed deployment: %v", err)
			}

			got, err := o.AppStatus(ctx, ref)
			if err != nil {
				t.Fatalf("AppStatus: %v", err)
			}
			if got.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", got.Phase, tc.wantPhase)
			}
			if got.Desired != tc.desired {
				t.Errorf("desired = %d, want %d", got.Desired, tc.desired)
			}
			if got.Ready != tc.ready {
				t.Errorf("ready = %d, want %d", got.Ready, tc.ready)
			}
			if got.Generation != 3 || got.ObservedGeneration != 3 {
				t.Errorf("generation = %d/%d, want 3/3",
					got.Generation, got.ObservedGeneration)
			}
		})
	}
}

func TestAppStatusCarriesTheTerminalKubernetesReason(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "yacht-demo", Generation: 4,
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 4,
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "ProgressDeadlineExceeded", Message: "ReplicaSet timed out",
			}},
		},
	}
	if _, err := client.AppsV1().Deployments("yacht-demo").
		Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	got, err := o.AppStatus(ctx, orchestrator.Ref{
		Owner: "owner-1", Namespace: "yacht-demo", Name: "web",
	})
	if err != nil {
		t.Fatalf("AppStatus: %v", err)
	}
	if !got.Terminal || got.Reason != "ProgressDeadlineExceeded" ||
		got.Message != "ReplicaSet timed out" {
		t.Fatalf("terminal status = %#v", got)
	}
}

func TestDeleteAppIsIdempotent(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)
	spec := testSpec()

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	if err := o.DeleteApp(ctx, spec.Ref); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Deleting something already gone is a no-op, not an error. Callers
	// retry, and a retry must converge rather than fail.
	if err := o.DeleteApp(ctx, spec.Ref); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// Bad names must be rejected before they reach a cluster. A name that escapes
// its namespace is the failure mode that matters most once more than one owner
// shares a cluster.
func TestApplyAppRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)

	cases := []struct {
		name   string
		mutate func(*orchestrator.AppSpec)
	}{
		{"empty owner", func(s *orchestrator.AppSpec) { s.Owner = "" }},
		{"empty name", func(s *orchestrator.AppSpec) { s.Name = "" }},
		{"uppercase name", func(s *orchestrator.AppSpec) { s.Name = "Web" }},
		{"path traversal in namespace", func(s *orchestrator.AppSpec) { s.Namespace = "../kube-system" }},
		{"slash in name", func(s *orchestrator.AppSpec) { s.Name = "web/admin" }},
		{"empty image", func(s *orchestrator.AppSpec) { s.Image = "" }},
		{"negative replicas", func(s *orchestrator.AppSpec) { s.Replicas = -1 }},
		{"port out of range", func(s *orchestrator.AppSpec) { s.Port = 70000 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			tc.mutate(&spec)
			if err := o.ApplyApp(ctx, spec); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}
