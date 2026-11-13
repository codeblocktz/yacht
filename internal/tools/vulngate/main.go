// Command vulngate is `make vulncheck`: govulncheck, with the release
// cooldown applied. A fix is adopted once it has been out 7 days (a new minor
// or major, 14; 3 with a cooldown-override), so failing CI the day a fix is
// out fails every branch for something nobody may act on yet. For each
// vulnerability the code reaches (as govulncheck, the rest never fail):
//
//   - fix younger than its cooldown: a warning, and a pass;
//   - fix past its cooldown: a failure, adopt it now;
//   - no fix at all: a warning that says so, and a failure, as govulncheck
//     has it: only the code no longer reaching it passes;
//   - fix's release date unknown: a failure. A gate that can't tell fails.
//
// The date is the Go module proxy's (GOPROXY's first URL, or
// proxy.golang.org): a module version's .info Time; for the standard library,
// golang.org/toolchain's. A toolchain's Time is when its source was cut, for
// a security release days before it was out (go1.26.11: 2026-11-02, out
// 2026-11-10), so the proxy's Last-Modified on its .mod is used when later.
// Without one the Time stands, which errs early: the gate fails sooner.
//
// -at (or VULNGATE_NOW) is now, as 2026-11-18 or RFC 3339. The same file is
// in Yacht and in the application that wraps it: keep the copies the same.
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const (
	patchDays    = 7
	minorDays    = 14
	overrideDays = 3
	stdlib       = "stdlib"
)

func main() {
	at := flag.String("at", os.Getenv("VULNGATE_NOW"), "pretend it is this date (2026-11-18 or RFC 3339)")
	goBin := flag.String("go", "go", "the go command that runs govulncheck")
	flag.Parse()
	now, err := parseNow(*at, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "vulngate:", err)
		os.Exit(2)
	}

	cmd := exec.Command(*goBin, append([]string{"tool", "govulncheck", "-format", "json"}, flag.Args()...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		fmt.Printf("::error::vulngate: govulncheck did not start: %v\n", err)
		os.Exit(1)
	}
	reached, other, perr := parse(out)
	io.Copy(io.Discard, out) // a parse error must not leave govulncheck blocked writing
	if err := cmd.Wait(); err != nil || perr != nil {
		fmt.Printf("::error::vulngate: govulncheck failed: %v\n", cmp.Or(err, perr))
		os.Exit(1)
	}

	p := proxy{base: proxyURL(os.Getenv("GOPROXY")), client: &http.Client{Timeout: 30 * time.Second}}
	results := make([]result, len(reached))
	for i, f := range reached {
		results[i] = judge(f, p.released, now)
	}
	if report(os.Stdout, results, other) {
		os.Exit(1)
	}
}

func parseNow(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return now, nil
	}
	for _, layout := range []string{time.DateOnly, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("-at %q: want 2026-11-18 or RFC 3339", s)
}

// finding is one vulnerability in one module, as govulncheck reports it.
type finding struct {
	OSV     string
	Module  string // "stdlib" for the standard library
	Package string
	Version string // what the repo is on
	Fixed   string // the earliest fixed version; empty when there is none
}

// parse reads govulncheck's JSON stream. reached is each vulnerability whose
// symbols the code calls, once per module and fix; other is the IDs of the
// rest, found only in a module or package the code imports.
func parse(r io.Reader) (reached []finding, other []string, err error) {
	type frame struct{ Module, Version, Package, Function string }
	dec := json.NewDecoder(r)
	seen := map[string]bool{}
	for {
		var m struct {
			Finding *struct {
				OSV   string  `json:"osv"`
				Fixed string  `json:"fixed_version"`
				Trace []frame `json:"trace"`
			} `json:"finding"`
		}
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, nil, err
		}
		f := m.Finding
		if f == nil || len(f.Trace) == 0 {
			continue
		}
		top := f.Trace[0]
		if top.Function == "" {
			other = append(other, f.OSV)
			continue
		}
		k := f.OSV + " " + top.Module + " " + f.Fixed
		if !seen[k] {
			seen[k] = true
			reached = append(reached, finding{f.OSV, top.Module, top.Package, top.Version, f.Fixed})
		}
	}
	slices.SortFunc(reached, func(a, b finding) int { return cmp.Compare(a.OSV, b.OSV) })
	slices.Sort(other)
	other = slices.Compact(other)
	return reached, slices.DeleteFunc(other, func(id string) bool {
		return slices.ContainsFunc(reached, func(f finding) bool { return f.OSV == id })
	}), nil
}

// cooldownDays is how long a fix must have been out: 7 days for a patch of
// the major.minor the code is on (v0.58.0 to the pseudo-version
// v0.58.1-0.20261009181718-2f1f834330b2 is one), 14 for anything that moves
// it.
func cooldownDays(current, fixed string) int {
	minor := func(v string) string {
		v, _, _ = strings.Cut(strings.TrimPrefix(v, "v"), "-")
		parts := strings.SplitN(v, ".", 3)
		return strings.Join(parts[:min(2, len(parts))], ".")
	}
	if minor(current) == minor(fixed) {
		return patchDays
	}
	return minorDays
}

type verdict int

const (
	inCooldown verdict = iota
	cleared
	noFix
	unknownDate
)

type result struct {
	finding
	verdict         verdict
	released, until time.Time
	err             error
}

// judge decides one finding. released says when a module version came out.
func judge(f finding, released func(module, version string) (time.Time, error), now time.Time) result {
	r := result{finding: f}
	if f.Fixed == "" {
		r.verdict = noFix
		return r
	}
	if r.released, r.err = released(f.Module, f.Fixed); r.err != nil {
		r.verdict = unknownDate
		return r
	}
	r.until = r.released.AddDate(0, 0, cooldownDays(f.Version, f.Fixed))
	r.verdict = cleared
	if now.Before(r.until) {
		r.verdict = inCooldown
	}
	return r
}

// report prints one GitHub Actions annotation per finding and a closing
// line, and says whether the gate fails.
func report(w io.Writer, results []result, other []string) (failed bool) {
	var waiting, failing int
	for _, r := range results {
		level := "error"
		if r.verdict == inCooldown || r.verdict == noFix {
			level = "warning"
		}
		fmt.Fprintf(w, "::%s::%s\n", level, r.line())
		if r.verdict == inCooldown {
			waiting++
		} else {
			failing++
		}
	}
	if len(other) > 0 {
		fmt.Fprintf(w, "vulngate: not failing on %s: in a module or package imported, but not called (make vulncheck-raw for detail)\n",
			strings.Join(other, ", "))
	}
	outcome := "pass"
	if failing > 0 {
		outcome = "FAIL"
	}
	fmt.Fprintf(w, "vulngate: %d reachable, %d in cooldown, %d failing: %s\n", len(results), waiting, failing, outcome)
	return failing > 0
}

func (r result) line() string {
	name := r.OSV + " (" + cmp.Or(r.Package, r.Module) + ")"
	fix, adopt := r.Module+"@"+r.Fixed, "go get "+r.Module+"@"+r.Fixed
	if r.Module == stdlib {
		fix, adopt = "go"+strings.TrimPrefix(r.Fixed, "v"), "raise go.mod's go line to "+strings.TrimPrefix(r.Fixed, "v")
	}
	day := func(t time.Time) string { return t.UTC().Format(time.DateOnly) }
	switch r.verdict {
	case inCooldown:
		return fmt.Sprintf("%s: fix %s released %s is in cooldown until %s (or %s with a cooldown-override); not failing yet.",
			name, fix, day(r.released), day(r.until), day(r.released.AddDate(0, 0, overrideDays)))
	case cleared:
		return fmt.Sprintf("%s: fix %s released %s cleared its cooldown on %s and the code is on %s; adopt it now: %s.",
			name, fix, day(r.released), day(r.until), r.Version, adopt)
	case noFix:
		return fmt.Sprintf("%s: no fixed version of %s exists, so there is nothing to adopt; failing until the code no longer reaches it.",
			name, r.Module)
	default:
		return fmt.Sprintf("%s: release date of fix %s unknown, so its cooldown can't be told; failing closed: %v",
			name, fix, r.err)
	}
}

// proxyURL is GOPROXY's first URL, or proxy.golang.org when it names none
// (direct, off, a file:// mirror).
func proxyURL(goproxy string) string {
	for _, p := range strings.FieldsFunc(goproxy, func(r rune) bool { return r == ',' || r == '|' }) {
		if p = strings.TrimSpace(p); strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "http://") {
			return strings.TrimSuffix(p, "/")
		}
	}
	return "https://proxy.golang.org"
}

type proxy struct {
	base   string
	client *http.Client
}

// released is when the proxy says module@version came out; see the package
// comment for the standard library.
func (p proxy) released(module, version string) (time.Time, error) {
	path := module
	if module == stdlib {
		v := strings.TrimPrefix(version, "v")
		if strings.Contains(v, "-") {
			return time.Time{}, fmt.Errorf("%s is not a Go release", version)
		}
		path, version = "golang.org/toolchain", "v0.0.1-go"+v+".linux-amd64"
	}
	at := p.base + "/" + escape(path) + "/@v/" + escape(version)
	resp, err := p.get(at + ".info")
	if err != nil {
		return time.Time{}, err
	}
	var info struct{ Time time.Time }
	err = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if err == nil && info.Time.IsZero() {
		err = errors.New("no Time")
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("%s.info: %w", at, err)
	}
	if module != stdlib {
		return info.Time, nil
	}
	resp, err = p.get(at + ".mod")
	if err != nil {
		return time.Time{}, err
	}
	resp.Body.Close()
	if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil && lm.After(info.Time) {
		return lm, nil
	}
	return info.Time, nil
}

func (p proxy) get(url string) (*http.Response, error) {
	resp, err := p.client.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp, nil
}

// escape is the proxy's case encoding: each capital letter becomes ! and
// the letter in lower case, as golang.org/x/mod/module.EscapePath has it.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if 'A' <= r && r <= 'Z' {
			b.WriteByte('!')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
