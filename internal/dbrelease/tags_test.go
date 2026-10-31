package dbrelease

import (
	"slices"
	"testing"
)

func nums(t *testing.T, s string) Numbers {
	t.Helper()
	n, ok := ParseNumbers(s)
	if !ok {
		t.Fatalf("ParseNumbers(%q) failed", s)
	}
	return n
}

// Versions compare as numbers, not as text: 16.9 is before 16.10.
func TestVersionsCompareNumberByNumber(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"16.15", "16.16", -1},
		{"16.16", "16.15", 1},
		{"16.16", "17.0", -1},
		{"16.9", "16.10", -1},
		{"16", "16.4", -1},
		{"8.4.3", "8.4.3", 0},
		{"8.0.44", "8.4.1", -1},
	} {
		if got := nums(t, c.a).Compare(nums(t, c.b)); got != c.want {
			t.Errorf("%s vs %s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if v := nums(t, "8.4.3-log"); v.String() != "8.4.3" {
		t.Errorf("a server's suffix is kept: %s", v)
	}
	if _, ok := ParseNumbers("beta"); ok {
		t.Error("a version without numbers was read")
	}
}

func TestATagIsReadAsAVersionAndAVariant(t *testing.T) {
	pg := Line{Series: 1, Release: 2}
	mysql := Line{Series: 2, Release: 3}
	for _, c := range []struct {
		tag, version, variant string
		exact, follows        bool
		line                  Line
	}{
		{"16-alpine", "16", "alpine", false, true, pg},
		{"16.15-alpine", "16.15", "alpine", true, false, pg},
		{"16.4-alpine3.20", "16.4", "alpine3.20", true, false, pg},
		{"16", "16", "", false, true, pg},
		{"latest", "", "", false, false, pg},
		{"alpine", "", "alpine", false, false, pg},
		// mysql:8 follows more than one series, and is pinned rather than
		// pulled again.
		{"8", "8", "", false, false, mysql},
		{"8.4", "8.4", "", false, true, mysql},
		{"8.4.3-oraclelinux9", "8.4.3", "oraclelinux9", true, false, mysql},
	} {
		got, ok := ParseTag(c.tag)
		if !ok {
			t.Errorf("ParseTag(%q) refused", c.tag)
			continue
		}
		if got.Version.String() != c.version || got.Variant != c.variant {
			t.Errorf("ParseTag(%q) = %s %q, want %s %q", c.tag, got.Version, got.Variant, c.version, c.variant)
		}
		if got.Exact(c.line) != c.exact || got.Follows(c.line) != c.follows {
			t.Errorf("%q: exact %v follows %v, want %v %v", c.tag, got.Exact(c.line), got.Follows(c.line), c.exact, c.follows)
		}
	}
	for _, tag := range []string{"18beta1", "18rc1-alpine", "16.4beta"} {
		if _, ok := ParseTag(tag); ok {
			t.Errorf("pre-release %q was read as a release", tag)
		}
	}
	if got := TagFor(nums(t, "16.16"), "alpine"); got != "16.16-alpine" {
		t.Errorf("TagFor = %q", got)
	}
}

// What has been published is read per variant and per series: the newest
// alpine 16, and the newest major past it.
func TestTheNewestReleaseOfASeriesAndTheNextSeries(t *testing.T) {
	tags := []string{
		"16", "16-alpine", "16.9-alpine", "16.15-alpine", "16.16", "16.10-alpine",
		"17-alpine", "17.2-alpine", "18beta1-alpine", "18.6", "latest", "alpine",
	}
	pg := Line{Series: 1, Release: 2}
	alpine := NewReleases(tags, pg, "alpine")
	if v, ok := alpine.Newest(nums(t, "16")); !ok || v.String() != "16.15" {
		t.Errorf("newest alpine 16 = %s, %v; want 16.15 (16.16 has no alpine image)", v, ok)
	}
	if v, ok := alpine.NextSeries(nums(t, "16")); !ok || v.String() != "17" {
		t.Errorf("next alpine series = %s, %v; want 17 (18 is only a beta in alpine)", v, ok)
	}
	if _, ok := alpine.NextSeries(nums(t, "17")); ok {
		t.Error("17 is the newest alpine series")
	}
	plain := NewReleases(tags, pg, "")
	if v, _ := plain.NextSeries(nums(t, "16")); v.String() != "18" {
		t.Errorf("next plain series = %s, want 18", v)
	}

	mysql := NewReleases([]string{"8.0.40", "8.0.44", "8.4.1", "8.4.8", "8.4", "9.1.0"}, Line{Series: 2, Release: 3}, "")
	if v, _ := mysql.Newest(nums(t, "8.0")); v.String() != "8.0.44" {
		t.Errorf("newest 8.0 = %s", v)
	}
	if v, _ := mysql.NextSeries(nums(t, "8.0")); v.String() != "9.1" {
		t.Errorf("next after 8.0 = %s, want 9.1", v)
	}
	if !slices.Equal(Line{Series: 2}.SeriesOf(nums(t, "8.4.3")), Numbers{8, 4}) {
		t.Error("SeriesOf 8.4.3")
	}
}
