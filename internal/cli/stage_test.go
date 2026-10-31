package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// A product in a release stage says so after its version and atop its help;
// one with none says nothing extra.
func TestTheStageFollowsTheVersionAndTheHelp(t *testing.T) {
	run := func(stage string, args ...string) string {
		var out bytes.Buffer
		Run(context.Background(), Brand{Name: "datahuru", Version: "v1.2.3", Stage: stage}, args,
			Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }, ConfigDir: t.TempDir()})
		return out.String()
	}
	if got := run("beta", "version"); got != "datahuru v1.2.3 (beta)\n" {
		t.Errorf("version with a stage = %q", got)
	}
	if got := run("", "version"); got != "datahuru v1.2.3\n" {
		t.Errorf("version without one = %q", got)
	}
	if got := run("beta", "help"); !strings.HasPrefix(got, "datahuru (beta) — deploy to") {
		t.Errorf("help with a stage begins %q", strings.SplitN(got, "\n", 2)[0])
	}
	if got := run("", "help"); !strings.HasPrefix(got, "datahuru — deploy to") {
		t.Errorf("help without one begins %q", strings.SplitN(got, "\n", 2)[0])
	}
}
