package app

import (
	"testing"

	"github.com/codeblocktz/yacht/internal/dbrelease"
)

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

// pgTags is Docker Hub's postgres repository, in brief.
var pgTags = []string{
	"16", "16-alpine", "16.4", "16.4-alpine", "16.15", "16.15-alpine", "16.16", "16.16-alpine",
	"17", "17-alpine", "17.2", "17.2-alpine", "18beta1-alpine", "latest", "alpine",
}

func digests(m map[string]string) func(string) (string, bool) {
	return func(ref string) (string, bool) {
		d, ok := m[ref]
		return d, ok
	}
}

func n(s string) dbrelease.Numbers {
	v, _ := dbrelease.ParseNumbers(s)
	return v
}

func TestAnUpdateStaysWithinTheSeriesItRuns(t *testing.T) {
	pg := databaseEngines[SourcePostgres].line
	mysql := databaseEngines[SourceMySQL].line
	redis := databaseEngines[SourceRedis].line
	cases := []struct {
		name string
		in   updatePlan
		// image and version are the update; "" for none.
		image, version string
		rebuild        bool
		newest, next   string
	}{
		{
			name: "a pinned tag moves to the newest release's tag",
			in: updatePlan{line: pg, image: "postgres:16.4-alpine", running: n("16.4"), tags: pgTags,
				released: "sha256:old", digest: digests(map[string]string{"postgres:16.16-alpine": "sha256:new"})},
			image: "postgres:16.16-alpine", version: "16.16", newest: "16.16", next: "17",
		},
		{
			name: "a tag that follows the major is pulled again once it has moved",
			in: updatePlan{line: pg, image: "postgres:16-alpine", running: n("16.15"), tags: pgTags,
				released: "sha256:old", digest: digests(map[string]string{"postgres:16-alpine": "sha256:new"})},
			image: "postgres:16-alpine", version: "16.16", newest: "16.16", next: "17",
		},
		{
			name: "a following tag that has not moved yet offers nothing",
			in: updatePlan{line: pg, image: "postgres:16-alpine", running: n("16.15"), tags: pgTags,
				released: "sha256:old", digest: digests(map[string]string{"postgres:16-alpine": "sha256:old"})},
			newest: "16.16", next: "17",
		},
		{
			name: "the newest release offers only the next major",
			in: updatePlan{line: pg, image: "postgres:16-alpine", running: n("16.16"), tags: pgTags,
				released: "sha256:cur", digest: digests(map[string]string{"postgres:16-alpine": "sha256:cur"})},
			newest: "16.16", next: "17",
		},
		{
			name: "the same release rebuilt is pulled again",
			in: updatePlan{line: pg, image: "postgres:16-alpine", running: n("16.16"), tags: pgTags,
				released: "sha256:cur", digest: digests(map[string]string{"postgres:16-alpine": "sha256:rebuilt"})},
			image: "postgres:16-alpine", version: "16.16", rebuild: true, newest: "16.16", next: "17",
		},
		{
			name: "a variant with no newer image offers nothing",
			in: updatePlan{line: pg, image: "postgres:16.4-bookworm", running: n("16.4"), tags: pgTags,
				released: "sha256:old", digest: digests(nil)},
		},
		{
			name: "a release tag that cannot be found is not deployed",
			in: updatePlan{line: pg, image: "postgres:16.4-alpine", running: n("16.4"), tags: pgTags,
				released: "sha256:old", digest: digests(nil)},
			newest: "16.16", next: "17",
		},
		{
			name: "without tags PostgreSQL's own word names the release",
			in: updatePlan{line: pg, image: "postgres:16.4", running: n("16.4"), published: n("16.15"),
				released: "sha256:old", digest: digests(map[string]string{"postgres:16.15": "sha256:new"})},
			image: "postgres:16.15", version: "16.15", newest: "16.15",
		},
		{
			name: "the newest of 17 is never offered to 16",
			in: updatePlan{line: pg, image: "postgres:latest", running: n("16.16"), tags: pgTags,
				released: "sha256:16", digest: digests(map[string]string{"postgres:latest": "sha256:17"})},
			newest: "16.16", next: "17",
		},
		{
			name: "mysql:8 is pinned within 8.0, never pulled into 8.4",
			in: updatePlan{line: mysql, image: "mysql:8", running: n("8.0.40"),
				tags:     []string{"8", "8.0", "8.0.40", "8.0.44", "8.4", "8.4.8"},
				released: "sha256:old", digest: digests(map[string]string{"mysql:8": "sha256:84", "mysql:8.0.44": "sha256:44"})},
			image: "mysql:8.0.44", version: "8.0.44", newest: "8.0.44", next: "8.4",
		},
		{
			name: "a redis tag that has moved names the release it moved to",
			in: updatePlan{line: redis, image: "redis:7-alpine",
				tags:     []string{"7-alpine", "7.4.1-alpine", "7.4.2-alpine", "8.0.1-alpine"},
				released: "sha256:741", digest: digests(map[string]string{
					"redis:7-alpine": "sha256:742", "redis:7.4.2-alpine": "sha256:742"})},
			image: "redis:7-alpine", version: "7.4.2",
		},
		{
			name: "a redis running the newest of its tag says so",
			in: updatePlan{line: redis, image: "redis:7-alpine",
				tags:     []string{"7-alpine", "7.4.2-alpine", "8.0.1-alpine"},
				released: "sha256:742", digest: digests(map[string]string{
					"redis:7-alpine": "sha256:742", "redis:7.4.2-alpine": "sha256:742"})},
			newest: "7.4.2", next: "8",
		},
		{
			name: "an image named by digest is left alone",
			in: updatePlan{line: pg, image: "postgres@sha256:abc", running: n("16.4"), tags: pgTags,
				digest: digests(nil)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.plan()
			if got.image != c.image || (c.image != "" && got.version.String() != c.version) || got.rebuild != c.rebuild {
				t.Errorf("update = %q %s rebuild %v; want %q %s rebuild %v",
					got.image, got.version, got.rebuild, c.image, c.version, c.rebuild)
			}
			if got.newest.String() != c.newest || got.next.String() != c.next {
				t.Errorf("newest %s next %s; want %s %s", got.newest, got.next, c.newest, c.next)
			}
		})
	}
}
