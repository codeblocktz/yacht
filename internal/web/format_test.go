package web

import (
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func TestClampPercent(t *testing.T) {
	// A meter wider than its track is a rendering bug that looks like a
	// monitoring bug, so clamping is asserted rather than assumed.
	cases := map[int]int{-20: 0, 0: 0, 50: 50, 100: 100, 140: 100}
	for in, want := range cases {
		if got := clampPercent(in); got != want {
			t.Errorf("clampPercent(%d) = %d, want %d", in, got, want)
		}
	}
}

// Helpers return complete class names declared in input.css, not fragments
// composed in a template. A name built up from pieces in Go is invisible to
// Tailwind's scanner and gets stripped from the compiled stylesheet.
func TestMeterClassEscalates(t *testing.T) {
	cases := map[int]string{
		10:  "meter-ok",
		74:  "meter-ok",
		75:  "meter-warn",
		89:  "meter-warn",
		90:  "meter-err",
		100: "meter-err",
	}
	for in, want := range cases {
		if got := meterClass(in); got != want {
			t.Errorf("meterClass(%d) = %q, want %q", in, got, want)
		}
	}
}

// A Running pod whose containers are not all ready is not healthy. Colouring
// it green would hide exactly the state an operator is looking for.
func TestPodPhaseClassTreatsUnreadyRunningAsDegraded(t *testing.T) {
	healthy := orchestrator.PodInfo{Phase: "Running", Ready: 2, Total: 2}
	if got := podPhaseClass(healthy); got != "status-ok" {
		t.Errorf("fully ready = %q, want status-ok", got)
	}

	degraded := orchestrator.PodInfo{Phase: "Running", Ready: 1, Total: 2}
	if got := podPhaseClass(degraded); got != "status-warn" {
		t.Errorf("partially ready = %q, want status-warn", got)
	}

	failed := orchestrator.PodInfo{Phase: "Failed"}
	if got := podPhaseClass(failed); got != "status-err" {
		t.Errorf("failed = %q, want status-err", got)
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"seconds ago", now.Add(-20 * time.Second), "just now"},
		{"one minute", now.Add(-90 * time.Second), "1 minute ago"},
		{"minutes", now.Add(-12 * time.Minute), "12 minutes ago"},
		{"one hour", now.Add(-70 * time.Minute), "1 hour ago"},
		{"hours", now.Add(-5 * time.Hour), "5 hours ago"},
		{"one day", now.Add(-25 * time.Hour), "1 day ago"},
		{"days", now.Add(-6 * 24 * time.Hour), "6 days ago"},
		{"unset", time.Time{}, "unknown"},
		// Clock skew between a node and the control plane can put a timestamp
		// in the future. "in -3 minutes" would read as a bug in the product.
		{"future", now.Add(3 * time.Minute), "just now"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relativeTime(tc.at); got != tc.want {
				t.Errorf("relativeTime = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAppTabHref(t *testing.T) {
	if got := appTabHref("web", ""); got != "/apps/web" {
		t.Errorf("default tab href = %q", got)
	}
	if got := appTabHref("web", "metrics"); got != "/apps/web/metrics" {
		t.Errorf("metrics tab href = %q", got)
	}
}

func TestUsagePercentIsZeroWhenUnknown(t *testing.T) {
	// Without metrics-server there is no usage to report. Returning a
	// computed percentage from a zero numerator would draw an empty bar that
	// reads as "idle" rather than "unknown".
	n := orchestrator.NodeInfo{CPUCapacityMillis: 4000, MemCapacityBytes: 1 << 30}
	if got := n.CPUPercent(); got != 0 {
		t.Errorf("CPUPercent without metrics = %d, want 0", got)
	}

	n.UsageKnown = true
	n.CPUUsedMillis = 1000
	if got := n.CPUPercent(); got != 25 {
		t.Errorf("CPUPercent = %d, want 25", got)
	}
}

func TestPercentHandlesZeroCapacity(t *testing.T) {
	// A node reporting no capacity must not divide by zero.
	n := orchestrator.NodeInfo{UsageKnown: true, CPUUsedMillis: 500}
	if got := n.CPUPercent(); got != 0 {
		t.Errorf("CPUPercent with zero capacity = %d, want 0", got)
	}
	s := orchestrator.ClusterSummary{UsageKnown: true, MemUsedBytes: 100}
	if got := s.MemoryPercent(); got != 0 {
		t.Errorf("MemoryPercent with zero capacity = %d, want 0", got)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:               "0 B",
		512:             "512 B",
		1024:            "1.0 KiB",
		1536:            "1.5 KiB",
		1 << 20:         "1.0 MiB",
		4 * (1 << 30):   "4.0 GiB",
		512 * (1 << 20): "512 MiB",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatMillicores(t *testing.T) {
	cases := map[int64]string{0: "0", 500: "0.5", 1000: "1", 1500: "1.5", 16000: "16"}
	for in, want := range cases {
		if got := formatMillicores(in); got != want {
			t.Errorf("formatMillicores(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEnv(t *testing.T) {
	t.Run("parses pairs, skipping blanks and comments", func(t *testing.T) {
		got, err := parseEnv("A=1\n\n# a comment\nB = two \nC=")
		if err != nil {
			t.Fatalf("parseEnv: %v", err)
		}
		want := map[string]string{"A": "1", "B": "two", "C": ""}
		if len(got) != len(want) {
			t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
	})

	t.Run("rejects a line with no equals", func(t *testing.T) {
		if _, err := parseEnv("JUST_A_KEY"); err == nil {
			t.Error("expected an error for a malformed line")
		}
	})

	t.Run("rejects an empty key", func(t *testing.T) {
		if _, err := parseEnv("=value"); err == nil {
			t.Error("expected an error for an empty key")
		}
	})
}

func TestSortedKeysIsDeterministic(t *testing.T) {
	// Ranging a map directly reorders the page on every render.
	m := map[string]string{"z": "1", "a": "2", "m": "3"}
	got := sortedKeys(m)
	want := []string{"a", "m", "z"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedKeys = %v, want %v", got, want)
		}
	}
}
