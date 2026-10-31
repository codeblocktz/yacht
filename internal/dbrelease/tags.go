package dbrelease

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Numbers is a version as its numbers: MySQL 8.4.3 is [8 4 3].
type Numbers []int

// ParseNumbers reads the numbers a version starts with, "8.4.3" from
// "8.4.3-log". False when it does not start with one.
func ParseNumbers(s string) (Numbers, bool) {
	m := leadingNumbers.FindString(strings.TrimSpace(s))
	if m == "" {
		return nil, false
	}
	var out Numbers
	for _, part := range strings.Split(m, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

var leadingNumbers = regexp.MustCompile(`^\d+(?:\.\d+)*`)

func (n Numbers) String() string {
	parts := make([]string, len(n))
	for i, v := range n {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ".")
}

// Compare orders two versions number by number; a version that runs out
// first is the older, so 16 is before 16.4.
func (n Numbers) Compare(o Numbers) int { return slices.Compare(n, o) }

// HasPrefix says n is within p: 16.4 is within 16, and 8.4.3 within 8.4.
func (n Numbers) HasPrefix(p Numbers) bool {
	return len(n) >= len(p) && slices.Equal(n[:len(p)], p)
}

// Line is how an engine numbers its releases, and so how far an update in
// place may go.
type Line struct {
	// Series is how many leading numbers an update in place stays within.
	// One for PostgreSQL, whose 16.4 and 16.16 share a data directory and 17
	// does not; two for MySQL, whose 8.0 and 8.4 are separate release series
	// that upgrade one way only.
	Series int
	// Release is how many numbers name one release: PostgreSQL 16.4 has two,
	// MySQL 8.4.3 three.
	Release int
}

// SeriesOf is the series a version is in: 16 for 16.4, 8.4 for 8.4.3.
func (l Line) SeriesOf(v Numbers) Numbers {
	if len(v) < l.Series {
		return nil
	}
	return v[:l.Series]
}

// Tag is an image tag read as a version and a variant: "16.15-alpine" is
// 16.15 of the alpine variant, "16-alpine" follows the newest 16 of it, and
// "latest" has no version at all.
type Tag struct {
	Version Numbers
	Variant string
}

var tagPattern = regexp.MustCompile(`^(\d+(?:\.\d+)*)(?:-([A-Za-z][A-Za-z0-9._-]*))?$`)

// ParseTag reads a tag. False for one that is neither a version nor a plain
// name, a pre-release among them: "18beta1", "18rc1-alpine". Nothing says
// what a pre-release fixes, and none is offered as an update.
func ParseTag(tag string) (Tag, bool) {
	if tag == "" || tag == "latest" {
		return Tag{}, true
	}
	if tag[0] < '0' || tag[0] > '9' {
		return Tag{Variant: tag}, true
	}
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return Tag{}, false
	}
	v, ok := ParseNumbers(m[1])
	if !ok {
		return Tag{}, false
	}
	return Tag{Version: v, Variant: m[2]}, true
}

// Exact says the tag names one release, so it never moves: 16.4-alpine
// rather than 16-alpine.
func (t Tag) Exact(l Line) bool { return len(t.Version) >= l.Release }

// Follows says the tag moves only within one series, so pulling it again
// cannot change the major version: 16-alpine follows 16, while mysql's 8
// moves from 8.0 to 8.4 and latest moves anywhere.
func (t Tag) Follows(l Line) bool { return !t.Exact(l) && len(t.Version) >= l.Series }

// TagFor is the tag of one release in a variant: 16.16 of alpine is
// "16.16-alpine".
func TagFor(v Numbers, variant string) string {
	if variant == "" {
		return v.String()
	}
	return v.String() + "-" + variant
}

// Releases is what a repository's tags say has been published in one
// variant: every exact release, and nothing that only follows one.
type Releases struct {
	line Line
	all  []Numbers
}

// NewReleases reads a repository's tags for one variant.
func NewReleases(tags []string, l Line, variant string) Releases {
	r := Releases{line: l}
	for _, s := range tags {
		t, ok := ParseTag(s)
		if !ok || t.Variant != variant || len(t.Version) != l.Release {
			continue
		}
		r.all = append(r.all, t.Version)
	}
	return r
}

// Known says any release was found.
func (r Releases) Known() bool { return len(r.all) > 0 }

// Newest is the newest release within a version: of a series, 16, or of
// what a tag follows. False when none is published.
func (r Releases) Newest(within Numbers) (Numbers, bool) {
	var best Numbers
	for _, v := range r.all {
		if v.HasPrefix(within) && (best == nil || v.Compare(best) > 0) {
			best = v
		}
	}
	return best, best != nil
}

// NextSeries is the newest series past one, 18 when 16 runs; false when the
// series is the newest published.
func (r Releases) NextSeries(series Numbers) (Numbers, bool) {
	var best Numbers
	for _, v := range r.all {
		s := r.line.SeriesOf(v)
		if s.Compare(series) > 0 && (best == nil || s.Compare(best) > 0) {
			best = s
		}
	}
	return best, best != nil
}
