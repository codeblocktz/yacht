package k8s

import (
	"context"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The markers are taken out of the log as data; every other line is shown.
func TestScanBuildLogTakesTheMarkers(t *testing.T) {
	log := "Cloning into '/workspace/src'...\n" +
		commitMarker + "b6edda3476eb\n" +
		"#12 exporting to image\n" +
		uidMarker + " 10001 \n" +
		uidMarker + "not-a-number\n"
	var (
		result orchestrator.BuildResult
		seen   markersSeen
		shown  []string
	)
	scanBuildLog(strings.NewReader(log), func(s string) { shown = append(shown, s) }, &result, &seen)
	if result.CommitSHA != "b6edda3476eb" || result.RunAsUser != 10001 || !seen.commit || !seen.uid {
		t.Errorf("result %+v, seen %+v", result, seen)
	}
	if got := strings.Join(shown, ""); got != "Cloning into '/workspace/src'...\n#12 exporting to image\n" {
		t.Errorf("shown %q: want everything but the markers", got)
	}
}

// A log cut short — the kubelet's own error where the rest should be — has
// no markers, and says so, so the finished build is read again.
func TestACutLogLeavesTheMarkersUnseen(t *testing.T) {
	log := "Cloning into '/workspace/src'...\nfailed to create fsnotify watcher: too many open files\n"
	var (
		result orchestrator.BuildResult
		seen   markersSeen
	)
	scanBuildLog(strings.NewReader(log), nil, &result, &seen)
	if seen.commit || seen.uid || result.RunAsUser != 0 {
		t.Errorf("a cut log: result %+v, seen %+v", result, seen)
	}
}

// Reading the finished build again finds what it finds, and says something
// only when that was new.
func TestRecoverBuildMarkersSaysNothingWhenThereIsNothingNew(t *testing.T) {
	o, _ := testOrchestrator(t)
	var (
		result orchestrator.BuildResult
		seen   markersSeen
		shown  []string
	)
	// The fake client's logs are a fixed "fake logs", with no markers.
	o.recoverBuildMarkers(context.Background(), "build-pod", &result, &seen, func(s string) { shown = append(shown, s) })
	if seen.commit || seen.uid || len(shown) != 0 {
		t.Errorf("seen %+v, shown %q", seen, shown)
	}
}
