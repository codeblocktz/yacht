package dbrelease

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseVersionReadsWhatTheServerSays(t *testing.T) {
	for in, want := range map[string]Version{
		"16.4":                           {Major: "16", Minor: 4},
		"16.4 (Debian 16.4-1.pgdg120+1)": {Major: "16", Minor: 4},
		"17.11":                          {Major: "17", Minor: 11},
		"9.6.24":                         {Major: "9.6", Minor: 24},
	} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "18beta1", "18rc1", "16", "9.6", "PostgreSQL"} {
		if v, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) = %v; want a refusal", in, v)
		}
	}
}

func TestFixedInPairsTheMajorWithItsRelease(t *testing.T) {
	got, ok := fixedIn("18, 17, 16, 15", "18.6, 17.11, 16.15, 15.19", "16")
	if !ok || got != (Version{Major: "16", Minor: 15}) {
		t.Fatalf("fixedIn = %v, %v; want 16.15", got, ok)
	}
	if _, ok := fixedIn("18", "18.6", "16"); ok {
		t.Fatal("an advisory that does not affect 16 was said to be fixed in it")
	}
}

func TestTheSecurityPageIsReadRowByRow(t *testing.T) {
	body, err := os.ReadFile("testdata/security-16.html")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseSecurity(body, "16", "https://pg.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d advisories, want the 3 in the table and none of the legend: %+v", len(got), got)
	}
	first := got[0]
	want := Advisory{
		ID: "CVE-2026-19385", URL: "https://pg.test/support/security/CVE-2026-19385/",
		Summary:   "PostgreSQL pg_dump heap buffer overflow executes arbitrary code",
		Component: "client", Score: 8.8, Fixed: Version{Major: "16", Minor: 15},
	}
	if first != want {
		t.Fatalf("first advisory\n got %+v\nwant %+v", first, want)
	}
	if got[2].Component != "core server" || got[2].Score != 4.3 {
		t.Fatalf("third advisory = %+v", got[2])
	}
}

// site serves versions.json and the security page for 16, counting asks.
func site(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	page, err := os.ReadFile("testdata/security-16.html")
	if err != nil {
		t.Fatal(err)
	}
	var asks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		switch r.URL.Path {
		case "/versions.json":
			_, _ = w.Write([]byte(`[
				{"major": "16", "latestMinor": "15", "eolDate": "2028-11-09", "supported": true},
				{"major": "12", "latestMinor": "22", "eolDate": "2024-11-21", "supported": false}]`))
		case "/support/security/16/":
			_, _ = w.Write(page)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asks
}

func TestAReportSaysWhatARunningVersionIsWithout(t *testing.T) {
	srv, asks := site(t)
	p := &Postgres{HTTP: srv.Client(), Base: srv.URL}
	ctx := context.Background()

	r, err := p.Report(ctx, Version{Major: "16", Minor: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Behind() || r.Latest.String() != "16.15" || !r.Supported {
		t.Fatalf("report = %+v; want behind 16.15, supported", r)
	}
	if len(r.Fixes) != 3 || r.Worst() != "high" || r.Fixes[0].Score != 8.8 {
		t.Fatalf("fixes = %+v; want the three, most severe first", r.Fixes)
	}

	// The newest release is without nothing, and the security page is not
	// asked for at all.
	before := asks.Load()
	r, err = p.Report(ctx, Version{Major: "16", Minor: 15})
	if err != nil || r.Behind() || len(r.Fixes) != 0 {
		t.Fatalf("latest release report = %+v, %v", r, err)
	}
	if asks.Load() != before {
		t.Fatal("the site was asked again inside the TTL")
	}

	// A major version past its end is said to be.
	r, err = p.Report(ctx, Version{Major: "12", Minor: 22})
	if err != nil || r.Supported || r.EOL.Year() != 2024 {
		t.Fatalf("end-of-life report = %+v, %v", r, err)
	}
}

func TestAnswersAreKeptForTheTTL(t *testing.T) {
	srv, asks := site(t)
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	p := &Postgres{HTTP: srv.Client(), Base: srv.URL, TTL: time.Hour, Now: func() time.Time { return now }}
	ctx := context.Background()
	running := Version{Major: "16", Minor: 4}

	if _, err := p.Report(ctx, running); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Report(ctx, running); err != nil {
		t.Fatal(err)
	}
	if got := asks.Load(); got != 2 {
		t.Fatalf("asked %d times inside the TTL, want 2 (versions and the security page)", got)
	}
	now = now.Add(2 * time.Hour)
	if _, err := p.Report(ctx, running); err != nil {
		t.Fatal(err)
	}
	if got := asks.Load(); got != 4 {
		t.Fatalf("asked %d times after the TTL, want 4", got)
	}
}

func TestASiteThatCannotBeReadIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	p := &Postgres{HTTP: srv.Client(), Base: srv.URL}
	if _, err := p.Report(context.Background(), Version{Major: "16", Minor: 4}); err == nil {
		t.Fatal("a site that answered 502 gave a report")
	}
}

func TestASiteThatCouldNotBeReadIsNotAskedAgainAtOnce(t *testing.T) {
	var asks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	p := &Postgres{HTTP: srv.Client(), Base: srv.URL, Now: func() time.Time { return now }}
	running := Version{Major: "16", Minor: 4}
	for range 3 {
		if _, err := p.Report(context.Background(), running); err == nil {
			t.Fatal("a site that answered 502 gave a report")
		}
	}
	if got := asks.Load(); got != 1 {
		t.Fatalf("asked %d times in a row, want once", got)
	}
	now = now.Add(retryAfter)
	_, _ = p.Report(context.Background(), running)
	if got := asks.Load(); got != 2 {
		t.Fatalf("asked %d times after waiting, want twice", got)
	}
}
