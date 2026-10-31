package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// releasePodsOrchestrator has the old version's pod serving and, until the
// rollback removes it, a new one for the release last applied, which has
// crashed: its current run has printed nothing, its last run why it died.
type releasePodsOrchestrator struct {
	*rolloutScriptOrchestrator
	oldRelease string

	mu     sync.Mutex
	newPod bool
	asked  []orchestrator.LogOptions
}

func (o *releasePodsOrchestrator) Pods(context.Context, orchestrator.PodListOptions) ([]orchestrator.PodInfo, error) {
	now := time.Now()
	pods := []orchestrator.PodInfo{{
		Name: "web-old", Namespace: "ns", App: "web", ReleaseID: o.oldRelease,
		Ready: 1, Total: 1, CreatedAt: now.Add(-time.Hour),
	}}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.newPod {
		pods = append(pods, orchestrator.PodInfo{
			Name: "web-new", Namespace: "ns", App: "web", ReleaseID: o.lastAppSpec().ReleaseID,
			Total: 1, Restarts: 3, CreatedAt: now,
		})
	}
	return pods, nil
}

func (o *releasePodsOrchestrator) Logs(_ context.Context, opts orchestrator.LogOptions) ([]orchestrator.LogLine, error) {
	o.mu.Lock()
	o.asked = append(o.asked, opts)
	o.mu.Unlock()
	switch {
	case opts.Pod == "web-old":
		return []orchestrator.LogLine{{Text: "GET / 200"}}, nil
	case opts.Pod == "web-new" && opts.Previous:
		return []orchestrator.LogLine{{Text: "QA-R1-CRASH"}, {Text: "exiting 1"}}, nil
	}
	return nil, nil
}

func (o *releasePodsOrchestrator) rollBack() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.newPod = false
}

func lineTexts(lines []orchestrator.LogLine) string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return strings.Join(out, "\n")
}

// A redeploy that crashes is rolled back, and its pod goes with the
// rollback. Its Deploy logs showed the old version's output, which is all the
// app had left; they show what the crashed container printed, read from its
// own pod while it is there and kept with the deployment once it is not.
func TestAFailedRedeployShowsItsOwnCrashedOutput(t *testing.T) {
	ctx := context.Background()
	events := &recordedEvents{}
	s, base, pool := testService(t, settleOptions(events))
	ownerID := owner(t, s, pool, "rollout-own-logs")
	prior, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	crash := crashedStatus(3, "code 1 (Error)")
	crash.Terminal, crash.Reason, crash.Message = true, "CrashLoopBackOff", "the container exited 3 times"
	orch := &releasePodsOrchestrator{
		rolloutScriptOrchestrator: &rolloutScriptOrchestrator{
			recordingOrchestrator: base, statuses: []orchestrator.AppStatus{crash},
		},
		oldRelease: prior.ActiveReleaseID.String(), newPod: true,
	}
	s.orch = orch
	if err := s.SetVariable(ctx, ownerID, "web", VariableInput{Key: "MODE", Value: "crash"}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if err := executeNamedOperation(s, ctx, ownerID, "web"); err == nil {
		t.Fatal("a redeploy that crash-loops succeeded")
	}
	d := deploymentOf(t, s, ownerID, prior.ID)
	if d.Status != DeployFailed {
		t.Fatalf("deployment = %s %q, want failed", d.Status, d.Message)
	}
	if got := events.last().Kind; got != EventDeployCrashed {
		t.Fatalf("event = %s, want %s", got, EventDeployCrashed)
	}

	// While its pod is there: its own, and the run that died, since the
	// one waiting to restart has printed nothing.
	got, err := s.DeploymentLogs(ctx, ownerID, "web", d.ID, LogRequest{})
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if text := lineTexts(got.Logs.Lines); !got.Live || got.Kept || got.Logs.Pod != "web-new" ||
		!got.Logs.Previous || !strings.Contains(text, "QA-R1-CRASH") || strings.Contains(text, "GET /") {
		t.Fatalf("logs = live %v kept %v pod %q previous %v %q, want web-new's last run",
			got.Live, got.Kept, got.Logs.Pod, got.Logs.Previous, text)
	}
	if strings.Join(got.Logs.Pods, ",") != "web-new" {
		t.Fatalf("pods offered = %v, want only the deployment's own", got.Logs.Pods)
	}

	// Once the rollback has removed it: what was kept as it failed.
	orch.rollBack()
	got, err = s.DeploymentLogs(ctx, ownerID, "web", d.ID, LogRequest{})
	if err != nil {
		t.Fatalf("DeploymentLogs after the rollback: %v", err)
	}
	if text := lineTexts(got.Logs.Lines); !got.Kept || got.Logs.Pod != "web-new" || !got.Logs.Previous ||
		text != "QA-R1-CRASH\nexiting 1" {
		t.Fatalf("logs after the rollback = kept %v pod %q previous %v %q, want what web-new printed",
			got.Kept, got.Logs.Pod, got.Logs.Previous, text)
	}

	// And the deployment serving still shows the old version's.
	deps, err := s.Deployments(ctx, ownerID, prior.ID, 10)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	for _, dep := range deps {
		if !dep.IsActive {
			continue
		}
		got, err = s.DeploymentLogs(ctx, ownerID, "web", dep.ID, LogRequest{})
		if err != nil {
			t.Fatalf("DeploymentLogs of the serving deployment: %v", err)
		}
		if got.Logs.Pod != "web-old" || lineTexts(got.Logs.Lines) != "GET / 200" {
			t.Fatalf("serving deployment's logs = %q %q, want web-old's", got.Logs.Pod, lineTexts(got.Logs.Lines))
		}
	}
}

// A deploy still deploying said it "has been replaced". It is the latest,
// on its way: its own pod's output once there is one, and that it has not
// started until then, never the old version's.
func TestADeployInFlightIsNotReplaced(t *testing.T) {
	ctx := context.Background()
	s, base, pool := testService(t, settleOptions(&recordedEvents{}))
	ownerID := owner(t, s, pool, "rollout-in-flight-logs")
	prior, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:1.27", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	orch := &releasePodsOrchestrator{
		rolloutScriptOrchestrator: &rolloutScriptOrchestrator{recordingOrchestrator: base},
		oldRelease:                prior.ActiveReleaseID.String(),
	}
	s.orch = orch
	if err := s.SetVariable(ctx, ownerID, "web", VariableInput{Key: "MODE", Value: "next"}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	d := deploymentOf(t, s, ownerID, prior.ID)
	if d.Status != DeployRunning {
		t.Fatalf("deployment = %s, want one still deploying", d.Status)
	}
	got, err := s.DeploymentLogs(ctx, ownerID, "web", d.ID, LogRequest{})
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if !got.Live || strings.Contains(got.Logs.Note, "replaced") || !strings.Contains(got.Logs.Note, "not started yet") {
		t.Fatalf("logs = live %v %q, want a deploy on its way", got.Live, got.Logs.Note)
	}
	if len(got.Logs.Lines) != 0 {
		t.Fatalf("a deploy with no pod yet showed %q", lineTexts(got.Logs.Lines))
	}
}
