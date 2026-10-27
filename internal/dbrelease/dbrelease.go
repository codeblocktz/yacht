// Package dbrelease is what a database's makers have released since the
// version a database runs: the newest release of its major version, whether
// that major version is still supported, and the security fixes in between.
//
// PostgreSQL publishes all three: versions.json for the releases, and a page
// per major version listing every security advisory with the release that
// fixed it. Both are read from postgresql.org, at most once per TTL, and only
// when somebody looks at a database.
package dbrelease

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

// Version is a PostgreSQL release: 16.4 is major "16", minor 4. Before 10 a
// major version had two parts, so 9.6.24 is major "9.6", minor 24.
type Version struct {
	Major string
	Minor int
}

func (v Version) String() string {
	if v.Major == "" {
		return ""
	}
	return v.Major + "." + strconv.Itoa(v.Minor)
}

// IsZero is a version that was not learned.
func (v Version) IsZero() bool { return v.Major == "" }

// leadingVersion is the numbers a server_version starts with. Debian's
// packages append their own build — "16.4 (Debian 16.4-1.pgdg120+1)" — and a
// beta has no minor at all: "18beta1".
var leadingVersion = regexp.MustCompile(`^(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// ParseVersion reads what server_version says. A pre-release — "18beta1",
// "18rc1" — is refused: nothing published says what is fixed in one.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	m := leadingVersion.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("dbrelease: %q is not a PostgreSQL version", s)
	}
	if rest := s[len(m[0]):]; rest != "" && !strings.HasPrefix(rest, " ") {
		return Version{}, fmt.Errorf("dbrelease: %q is a pre-release", s)
	}
	major, _ := strconv.Atoi(m[1])
	if major >= 10 {
		if m[2] == "" {
			return Version{}, fmt.Errorf("dbrelease: %q has no minor version", s)
		}
		minor, _ := strconv.Atoi(m[2])
		return Version{Major: m[1], Minor: minor}, nil
	}
	if m[2] == "" || m[3] == "" {
		return Version{}, fmt.Errorf("dbrelease: %q has no minor version", s)
	}
	minor, _ := strconv.Atoi(m[3])
	return Version{Major: m[1] + "." + m[2], Minor: minor}, nil
}

// Advisory is one security fix a database is without.
type Advisory struct {
	// ID is the CVE, and URL PostgreSQL's page about it.
	ID  string
	URL string
	// Summary is PostgreSQL's one line about it.
	Summary string
	// Component is what it is in: "core server", "client", "contrib module",
	// "packaging". Client advisories are in psql and pg_dump, which ship in
	// the same image.
	Component string
	// Score is the CVSS v3 base score, zero where none was published.
	Score float64
	// Fixed is the release of this major version that fixed it.
	Fixed Version
}

// Severity is the CVSS band the score falls in.
func (a Advisory) Severity() string {
	switch {
	case a.Score >= 9:
		return "critical"
	case a.Score >= 7:
		return "high"
	case a.Score >= 4:
		return "medium"
	case a.Score > 0:
		return "low"
	}
	return ""
}

// Report is where one running version stands.
type Report struct {
	Running Version
	// Latest is the newest release of Running's major version.
	Latest Version
	// Supported says the major version still gets fixes, and EOL is when it
	// stops or stopped.
	Supported bool
	EOL       time.Time
	// Fixes is the advisories fixed in a later release of the same major
	// version, the most severe first.
	Fixes []Advisory
}

// Behind says a newer release of the same major version is out.
func (r Report) Behind() bool {
	return !r.Latest.IsZero() && r.Latest.Minor > r.Running.Minor
}

// Worst is the most severe of the fixes it is without, "" when none.
func (r Report) Worst() string {
	if len(r.Fixes) == 0 {
		return ""
	}
	return cmp.Or(r.Fixes[0].Severity(), "unrated")
}

// Postgres reads postgresql.org.
type Postgres struct {
	// HTTP is the client to ask with; nil is one with a ten second timeout.
	HTTP *http.Client
	// Base is the site, "https://www.postgresql.org" when empty.
	Base string
	// TTL is how long an answer is kept, twelve hours when zero: PostgreSQL
	// releases quarterly, and out of cycle only for something serious.
	TTL time.Duration
	// Now is the clock, time.Now when nil.
	Now func() time.Time

	mu       sync.Mutex
	versions cached[map[string]major]
	security map[string]cached[[]Advisory]
	// failed is when the site last could not be read. It is not asked again
	// for retryAfter, so an install that cannot reach it waits once, not on
	// every page.
	failed time.Time
}

const retryAfter = 15 * time.Minute

type cached[T any] struct {
	value T
	at    time.Time
}

type major struct {
	latestMinor int
	supported   bool
	eol         time.Time
}

// Report says where a running version stands. An error is only returned
// when the releases themselves could not be read; a missing security page
// leaves Fixes empty rather than failing the rest.
func (p *Postgres) Report(ctx context.Context, running Version) (Report, error) {
	if running.IsZero() {
		return Report{}, errors.New("dbrelease: no running version")
	}
	majors, err := p.majors(ctx)
	if err != nil {
		return Report{}, err
	}
	r := Report{Running: running}
	m, ok := majors[running.Major]
	if !ok {
		return r, nil
	}
	r.Latest = Version{Major: running.Major, Minor: m.latestMinor}
	r.Supported, r.EOL = m.supported, m.eol
	if !r.Behind() {
		return r, nil
	}
	all, err := p.advisories(ctx, running.Major)
	if err != nil {
		return r, nil //nolint:nilerr // the release is still worth saying
	}
	for _, a := range all {
		if a.Fixed.Minor > running.Minor {
			r.Fixes = append(r.Fixes, a)
		}
	}
	slices.SortStableFunc(r.Fixes, func(a, b Advisory) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return cmp.Compare(b.Fixed.Minor, a.Fixed.Minor)
	})
	return r, nil
}

func (p *Postgres) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Postgres) fresh(at time.Time) bool {
	return !at.IsZero() && p.now().Sub(at) < cmp.Or(p.TTL, 12*time.Hour)
}

func (p *Postgres) majors(ctx context.Context) (map[string]major, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fresh(p.versions.at) {
		return p.versions.value, nil
	}
	if !p.failed.IsZero() && p.now().Sub(p.failed) < retryAfter {
		return nil, errors.New("dbrelease: postgresql.org could not be read recently")
	}
	body, err := p.get(ctx, "/versions.json")
	if err == nil {
		var majors map[string]major
		if majors, err = parseVersions(body); err == nil {
			p.versions = cached[map[string]major]{value: majors, at: p.now()}
			return majors, nil
		}
	}
	p.failed = p.now()
	return nil, err
}

func (p *Postgres) advisories(ctx context.Context, majorVersion string) ([]Advisory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.security[majorVersion]; ok && p.fresh(c.at) {
		return c.value, nil
	}
	body, err := p.get(ctx, "/support/security/"+majorVersion+"/")
	if err != nil {
		return nil, err
	}
	list, err := parseSecurity(body, majorVersion, p.base())
	if err != nil {
		return nil, err
	}
	if p.security == nil {
		p.security = map[string]cached[[]Advisory]{}
	}
	p.security[majorVersion] = cached[[]Advisory]{value: list, at: p.now()}
	return list, nil
}

func (p *Postgres) base() string {
	return strings.TrimRight(cmp.Or(p.Base, "https://www.postgresql.org"), "/")
}

// pageLimit bounds what is read from the site; the security page is about
// 60 KB.
const pageLimit = 4 << 20

func (p *Postgres) get(ctx context.Context, path string) ([]byte, error) {
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base()+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dbrelease: read %s: %w", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dbrelease: read %s: %s", path, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, pageLimit))
}

func parseVersions(body []byte) (map[string]major, error) {
	var rows []struct {
		Major       string `json:"major"`
		LatestMinor string `json:"latestMinor"`
		EOLDate     string `json:"eolDate"`
		Supported   bool   `json:"supported"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("dbrelease: read versions: %w", err)
	}
	out := make(map[string]major, len(rows))
	for _, r := range rows {
		minor, err := strconv.Atoi(r.LatestMinor)
		if err != nil || r.Major == "" {
			continue
		}
		eol, _ := time.Parse(time.DateOnly, r.EOLDate)
		out[r.Major] = major{latestMinor: minor, supported: r.Supported, eol: eol}
	}
	if len(out) == 0 {
		return nil, errors.New("dbrelease: versions.json lists no releases")
	}
	return out, nil
}

var cveID = regexp.MustCompile(`CVE-\d{4}-\d+`)

// parseSecurity reads the table on a major version's security page: one row
// per advisory, its affected major versions and the releases that fixed them
// listed in the same order.
func parseSecurity(body []byte, majorVersion, base string) ([]Advisory, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("dbrelease: read security page: %w", err)
	}
	var out []Advisory
	for tr := range doc.Descendants() {
		if tr.Type != html.ElementNode || tr.Data != "tr" {
			continue
		}
		var cells [][]string
		for td := range tr.ChildNodes() {
			if td.Type == html.ElementNode && td.Data == "td" {
				cells = append(cells, lines(td))
			}
		}
		if len(cells) != 5 {
			continue
		}
		id := cveID.FindString(strings.Join(cells[0], " "))
		if id == "" {
			continue
		}
		fixed, ok := fixedIn(strings.Join(cells[1], " "), strings.Join(cells[2], " "), majorVersion)
		if !ok {
			continue
		}
		a := Advisory{ID: id, URL: base + "/support/security/" + id + "/", Fixed: fixed}
		if len(cells[3]) > 0 {
			a.Component = cells[3][0]
		}
		if len(cells[3]) > 1 {
			a.Score, _ = strconv.ParseFloat(cells[3][1], 64)
		}
		if len(cells[4]) > 0 {
			a.Summary = cells[4][0]
		}
		out = append(out, a)
	}
	return out, nil
}

// fixedIn is the release of one major version an advisory was fixed in:
// "18, 17, 16" and "18.6, 17.11, 16.15" pair up by position.
func fixedIn(affected, fixed, majorVersion string) (Version, bool) {
	majors := strings.Split(affected, ",")
	fixes := strings.Split(fixed, ",")
	for i, m := range majors {
		if strings.TrimSpace(m) != majorVersion || i >= len(fixes) {
			continue
		}
		v, err := ParseVersion(strings.TrimSpace(fixes[i]))
		if err != nil || v.Major != majorVersion {
			return Version{}, false
		}
		return v, true
	}
	return Version{}, false
}

// lines is a cell's text, split where it breaks: the component, its score
// and its vector are three lines of one cell, as the summary and "more
// details" are.
func lines(n *html.Node) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.Join(strings.Fields(cur.String()), " "); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for d := range n.Descendants() {
		switch {
		case d.Type == html.TextNode:
			cur.WriteString(d.Data)
		case d.Type == html.ElementNode && d.Data == "br":
			flush()
		}
	}
	flush()
	return out
}
