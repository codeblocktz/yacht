package app

import (
	"testing"

	"github.com/codeblocktz/yacht/internal/dbrelease"
)

func TestAPinnedTagIsMovedToTheLatestRelease(t *testing.T) {
	running := dbrelease.Version{Major: "16", Minor: 4}
	latest := dbrelease.Version{Major: "16", Minor: 15}
	for image, want := range map[string]string{
		"postgres:16.4":                            "postgres:16.15",
		"postgres:16.4-alpine":                     "postgres:16.15-alpine",
		"docker.io/library/postgres:16.4-bookworm": "docker.io/library/postgres:16.15-bookworm",
		"registry.local:5000/pg:16.4":              "registry.local:5000/pg:16.15",
	} {
		got, ok := suggestedTag(image, running, latest)
		if !ok || got != want {
			t.Errorf("suggestedTag(%q) = %q, %v; want %q", image, got, ok, want)
		}
	}
	// Tags that follow a major version, or name an image by digest, are
	// left alone: a redeploy is what moves them, or nothing does.
	for _, image := range []string{
		"postgres:16-alpine", "postgres:16", "postgres", "postgres:16.40",
		"registry.local:5000/pg", "postgres@sha256:abc", "postgres:16.4@sha256:abc",
	} {
		if got, ok := suggestedTag(image, running, latest); ok {
			t.Errorf("suggestedTag(%q) = %q; want it left alone", image, got)
		}
	}
}

func TestADatabaseNeedsAttentionForFixesOrAnEndedVersion(t *testing.T) {
	fix := dbrelease.Advisory{ID: "CVE-2026-1", Score: 8.8}
	cases := []struct {
		name string
		r    DatabaseRelease
		want bool
	}{
		{"unknown", DatabaseRelease{Version: "16.4"}, false},
		{"current", DatabaseRelease{Known: true, Report: dbrelease.Report{Supported: true}}, false},
		{"missing a fix", DatabaseRelease{Known: true, Report: dbrelease.Report{
			Supported: true, Fixes: []dbrelease.Advisory{fix}}}, true},
		{"out of support", DatabaseRelease{Known: true}, true},
	}
	for _, c := range cases {
		if got := c.r.Attention(); got != c.want {
			t.Errorf("%s: Attention() = %v, want %v", c.name, got, c.want)
		}
	}
}
