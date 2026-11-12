package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

var netHTTP = finding{OSV: "GO-2026-6859", Module: stdlib, Package: "net/http", Version: "v1.26.10", Fixed: "v1.26.11"}

func releasedOn(date string) func(string, string) (time.Time, error) {
	return func(string, string) (time.Time, error) { return day(date).Add(17 * time.Hour), nil }
}

func TestJudge(t *testing.T) {
	unknown := func(string, string) (time.Time, error) { return time.Time{}, errors.New("404 Not Found") }
	noFixYet := netHTTP
	noFixYet.Fixed = ""
	minor := finding{OSV: "GO-2026-1", Module: "example.com/m", Package: "example.com/m/p", Version: "v1.2.3", Fixed: "v1.3.0"}

	for _, tc := range []struct {
		name     string
		f        finding
		released func(string, string) (time.Time, error)
		now      string
		want     verdict
		until    string
	}{
		{"patch, a day out", netHTTP, releasedOn("2026-11-10"), "2026-11-12", inCooldown, "2026-11-17"},
		{"patch, the last day", netHTTP, releasedOn("2026-11-10"), "2026-11-17", inCooldown, "2026-11-17"},
		{"patch, cleared", netHTTP, releasedOn("2026-11-10"), "2026-11-18", cleared, "2026-11-17"},
		{"minor, 8 days out", minor, releasedOn("2026-11-01"), "2026-11-09", inCooldown, "2026-11-15"},
		{"minor, cleared", minor, releasedOn("2026-11-01"), "2026-11-16", cleared, "2026-11-15"},
		{"no fix", noFixYet, nil, "2026-11-12", noFix, ""},
		{"unknown date", netHTTP, unknown, "2026-11-12", unknownDate, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := judge(tc.f, tc.released, day(tc.now))
			if r.verdict != tc.want {
				t.Fatalf("verdict %d, want %d", r.verdict, tc.want)
			}
			if tc.until != "" && r.until.Format(time.DateOnly) != tc.until {
				t.Errorf("until %s, want %s", r.until.Format(time.DateOnly), tc.until)
			}
		})
	}
}

func TestCooldownDays(t *testing.T) {
	for _, tc := range []struct {
		current, fixed string
		want           int
	}{
		{"v1.26.10", "v1.26.11", patchDays},
		{"v1.26.10", "v1.27.4", minorDays},
		{"v0.58.0", "v0.58.1-0.20261009181718-2f1f834330b2", patchDays},
		{"v0.58.0", "v0.59.0", minorDays},
		{"v1.9.9", "v2.0.0", minorDays},
		{"v2.3.0-rc.1", "v2.3.0", patchDays},
	} {
		if got := cooldownDays(tc.current, tc.fixed); got != tc.want {
			t.Errorf("cooldownDays(%s, %s) = %d, want %d", tc.current, tc.fixed, got, tc.want)
		}
	}
}

func TestReleased(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/golang.org/toolchain/@v/v0.0.1-go1.26.11.linux-amd64.info":
			w.Write([]byte(`{"Version":"v0.0.1-go1.26.11.linux-amd64","Time":"2026-11-02T20:36:21Z"}`))
		case "/golang.org/toolchain/@v/v0.0.1-go1.26.11.linux-amd64.mod":
			w.Header().Set("Last-Modified", "Tue, 10 Nov 2026 17:09:58 GMT")
			w.Write([]byte("module golang.org/toolchain\n"))
		case "/golang.org/toolchain/@v/v0.0.1-go1.26.10.linux-amd64.info":
			w.Write([]byte(`{"Time":"2026-10-19T22:20:36Z"}`))
		case "/golang.org/toolchain/@v/v0.0.1-go1.26.10.linux-amd64.mod":
			w.Write([]byte("module golang.org/toolchain\n")) // no Last-Modified
		case "/github.com/!burnt!sushi/toml/@v/v1.5.0.info":
			w.Write([]byte(`{"Version":"v1.5.0","Time":"2025-03-01T10:00:00Z"}`))
		case "/example.com/notime/@v/v1.0.0.info":
			w.Write([]byte(`{"Version":"v1.0.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := proxy{base: proxyURL("off," + srv.URL + "/,direct"), client: srv.Client()}

	for _, tc := range []struct {
		name, module, version string
		want                  string // RFC 3339; empty for an error
	}{
		{"stdlib, published after its Time", stdlib, "v1.26.11", "2026-11-10T17:09:58Z"},
		{"stdlib, no Last-Modified", stdlib, "v1.26.10", "2026-10-19T22:20:36Z"},
		{"module, escaped", "github.com/BurntSushi/toml", "v1.5.0", "2025-03-01T10:00:00Z"},
		{"unknown version", "example.com/gone", "v9.9.9", ""},
		{"no Time", "example.com/notime", "v1.0.0", ""},
		{"Go prerelease", stdlib, "v1.27.0-rc.1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.released(tc.module, tc.version)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("released = %s, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.UTC().Format(time.RFC3339) != tc.want {
				t.Errorf("released = %s, want %s", got.UTC().Format(time.RFC3339), tc.want)
			}
		})
	}
	// A module's date is its .info alone: its .mod is never asked for.
	if slices.Contains(asked, "/github.com/!burnt!sushi/toml/@v/v1.5.0.mod") {
		t.Error("asked for a module's .mod")
	}
}

func TestProxyURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "https://proxy.golang.org",
		"direct":                              "https://proxy.golang.org",
		"off":                                 "https://proxy.golang.org",
		"https://proxy.golang.org,direct":     "https://proxy.golang.org",
		"https://goproxy.example/|direct":     "https://goproxy.example",
		"file:///srv/mods,http://athens:3000": "http://athens:3000",
	} {
		if got := proxyURL(in); got != want {
			t.Errorf("proxyURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	stream := `{"config":{"scan_level":"symbol"}}
{"osv":{"id":"GO-2026-6859"}}
{"finding":{"osv":"GO-2026-6859","fixed_version":"v1.26.11","trace":[{"module":"stdlib","version":"v1.26.10"}]}}
{"finding":{"osv":"GO-2026-6859","fixed_version":"v1.26.11","trace":[{"module":"stdlib","version":"v1.26.10","package":"net/http"}]}}
{"finding":{"osv":"GO-2026-6859","fixed_version":"v1.26.11","trace":[{"module":"stdlib","version":"v1.26.10","package":"net/http","function":"Do","receiver":"*Client"},{"module":"example.com/app","function":"main"}]}}
{"finding":{"osv":"GO-2026-6859","fixed_version":"v1.26.11","trace":[{"module":"stdlib","version":"v1.26.10","package":"net/http","function":"Get","receiver":"Header"}]}}
{"finding":{"osv":"GO-2026-5932","fixed_version":"v0.58.1-0.20261009181718-2f1f834330b2","trace":[{"module":"golang.org/x/crypto","version":"v0.58.0"}]}}
{"finding":{"osv":"GO-2026-0001","trace":[{"module":"example.com/m","version":"v1.0.0","package":"example.com/m","function":"F"}]}}
`
	reached, other, err := parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	want := []finding{
		{OSV: "GO-2026-0001", Module: "example.com/m", Package: "example.com/m", Version: "v1.0.0"},
		netHTTP,
	}
	if !slices.Equal(reached, want) {
		t.Errorf("reached = %+v, want %+v", reached, want)
	}
	if !slices.Equal(other, []string{"GO-2026-5932"}) {
		t.Errorf("other = %v, want [GO-2026-5932]", other)
	}
	if _, _, err := parse(strings.NewReader(`{"finding":`)); err == nil {
		t.Error("parse of a cut-off stream: no error")
	}
}

func TestReport(t *testing.T) {
	released := day("2026-11-10").Add(17 * time.Hour)
	waiting := result{finding: netHTTP, verdict: inCooldown, released: released, until: released.AddDate(0, 0, 7)}
	due := waiting
	due.verdict = cleared
	crypto := finding{OSV: "GO-2026-2", Module: "golang.org/x/crypto", Package: "golang.org/x/crypto/ssh", Version: "v0.58.0", Fixed: "v0.59.0"}
	none := netHTTP
	none.Fixed = ""

	for _, tc := range []struct {
		name    string
		results []result
		other   []string
		want    string
		failed  bool
	}{
		{"in cooldown", []result{waiting}, []string{"GO-2026-5932"}, `::warning::GO-2026-6859 (net/http): fix go1.26.11 released 2026-11-10 is in cooldown until 2026-11-17 (or 2026-11-13 with a cooldown-override); not failing yet.
vulngate: not failing on GO-2026-5932: in a module or package imported, but not called (make vulncheck-raw for detail)
vulngate: 1 reachable, 1 in cooldown, 0 failing: pass
`, false},
		{"cleared", []result{due}, nil, `::error::GO-2026-6859 (net/http): fix go1.26.11 released 2026-11-10 cleared its cooldown on 2026-11-17 and the code is on v1.26.10; adopt it now: raise go.mod's go line to 1.26.11.
vulngate: 1 reachable, 0 in cooldown, 1 failing: FAIL
`, true},
		{"cleared module", []result{{finding: crypto, verdict: cleared, released: released, until: released.AddDate(0, 0, 14)}}, nil, `::error::GO-2026-2 (golang.org/x/crypto/ssh): fix golang.org/x/crypto@v0.59.0 released 2026-11-10 cleared its cooldown on 2026-11-24 and the code is on v0.58.0; adopt it now: go get golang.org/x/crypto@v0.59.0.
vulngate: 1 reachable, 0 in cooldown, 1 failing: FAIL
`, true},
		{"no fix", []result{{finding: none, verdict: noFix}}, nil, `::warning::GO-2026-6859 (net/http): no fixed version of stdlib exists, so there is nothing to adopt; failing until the code no longer reaches it.
vulngate: 1 reachable, 0 in cooldown, 1 failing: FAIL
`, true},
		{"unknown date", []result{{finding: netHTTP, verdict: unknownDate, err: errors.New("dial tcp: no route to host")}}, nil, `::error::GO-2026-6859 (net/http): release date of fix go1.26.11 unknown, so its cooldown can't be told; failing closed: dial tcp: no route to host
vulngate: 1 reachable, 0 in cooldown, 1 failing: FAIL
`, true},
		{"nothing", nil, nil, "vulngate: 0 reachable, 0 in cooldown, 0 failing: pass\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			if failed := report(&b, tc.results, tc.other); failed != tc.failed {
				t.Errorf("failed = %v, want %v", failed, tc.failed)
			}
			if b.String() != tc.want {
				t.Errorf("printed\n%s\nwant\n%s", b.String(), tc.want)
			}
		})
	}
}

func TestParseNow(t *testing.T) {
	today := day("2026-11-12")
	for in, want := range map[string]time.Time{
		"":                     today,
		"2026-11-18":           day("2026-11-18"),
		"2026-11-18T09:00:00Z": day("2026-11-18").Add(9 * time.Hour),
	} {
		if got, err := parseNow(in, today); err != nil || !got.Equal(want) {
			t.Errorf("parseNow(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := parseNow("next week", today); err == nil {
		t.Error("parseNow(next week): no error")
	}
}
