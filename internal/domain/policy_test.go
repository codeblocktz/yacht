package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestIssue(t *testing.T) {
	cases := []struct {
		name      string
		appName   string
		appDomain string
		want      string
		wantErr   bool
	}{
		{"simple", "web", "apps.example.com", "web.apps.example.com", false},
		{"uppercase is normalised", "web", "Apps.Example.COM", "web.apps.example.com", false},
		{"trailing dot is trimmed", "web", "apps.example.com.", "web.apps.example.com", false},
		{"dashes are fine", "my-api", "apps.example.com", "my-api.apps.example.com", false},
		{"no app domain", "web", "", "", true},
		{"app domain must have a dot", "web", "localhost", "", true},
		{"app name with a dot is rejected", "a.b", "apps.example.com", "", true},
		{"app name with underscore is rejected", "a_b", "apps.example.com", "", true},
		{"empty app name", "", "apps.example.com", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Issue(tc.appName, tc.appDomain)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Issue(%q, %q) = %q, want error", tc.appName, tc.appDomain, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Issue(%q, %q): %v", tc.appName, tc.appDomain, err)
			}
			if got != tc.want {
				t.Fatalf("Issue(%q, %q) = %q, want %q", tc.appName, tc.appDomain, got, tc.want)
			}
		})
	}
}

func TestIssueWithoutAppDomainIsDistinguishable(t *testing.T) {
	// Callers treat "no app domain configured" as the feature being off, not
	// as a failure, so it must be tellable apart from a malformed domain.
	if _, err := Issue("web", ""); !errors.Is(err, ErrNoAppDomain) {
		t.Fatalf("want ErrNoAppDomain, got %v", err)
	}
	if _, err := Issue("web", "localhost"); errors.Is(err, ErrNoAppDomain) {
		t.Fatal("a malformed domain must not report as ErrNoAppDomain")
	}
}

// A wildcard reaches exactly one label. Getting this wrong in either direction
// is a certificate error in front of a customer: too narrow and the platform's
// own hostnames lose TLS, too wide and a name the certificate cannot match is
// offered one anyway.
func TestCoveredByWildcard(t *testing.T) {
	for host, want := range map[string]bool{
		"web.apps.example.com":  true,
		"a-b.apps.example.com":  true,
		"a.b.apps.example.com":  false, // two labels: no wildcard reaches it
		"apps.example.com":      false, // the apex is not covered by *.itself
		"shop.customer.test":    false, // a custom domain, never under ours
		"evilapps.example.com":  false, // suffix without a label boundary
		"WEB.APPS.EXAMPLE.COM":  true,  // DNS is case-insensitive
		"web.apps.example.com.": true,  // the root dot is optional
		"":                      false,
	} {
		if got := CoveredByWildcard(host, "apps.example.com"); got != want {
			t.Errorf("CoveredByWildcard(%q) = %v, want %v", host, got, want)
		}
	}

	// With no platform domain there is no wildcard, so nothing is covered.
	if CoveredByWildcard("web.apps.example.com", "") {
		t.Error("a host was called covered with no app domain configured")
	}
}

// The filter is what the Ingress's TLS block is built from.
func TestWildcardHostsKeepsOnlyWhatIsCovered(t *testing.T) {
	got := WildcardHosts([]string{
		"web.apps.example.com",
		"shop.customer.test",
		"a.b.apps.example.com",
	}, "apps.example.com")

	if len(got) != 1 || got[0] != "web.apps.example.com" {
		t.Fatalf("WildcardHosts = %v, want only the platform host", got)
	}
}

// TestReservedMatchesOnLabelBoundary is the most important test in this
// package. Suffix matching on raw strings says evilapps.example.com is inside
// apps.example.com, which would let a tenant claim a host under the platform
// domain — the exact hole the reserved rule exists to close.
func TestReservedMatchesOnLabelBoundary(t *testing.T) {
	const appDomain = "apps.example.com"

	cases := []struct {
		host string
		want bool
	}{
		{"apps.example.com", true},
		{"web.apps.example.com", true},
		{"a.b.apps.example.com", true},
		{"WEB.APPS.EXAMPLE.COM", true},
		{"web.apps.example.com.", true},

		{"evilapps.example.com", false},
		{"notapps.example.com", false},
		{"apps.example.com.evil.test", false},
		{"example.com", false},
		{"other.test", false},
	}

	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := Reserved(tc.host, appDomain, nil); got != tc.want {
				t.Fatalf("Reserved(%q, %q) = %v, want %v", tc.host, appDomain, got, tc.want)
			}
		})
	}
}

func TestReservedHonoursExtraDomains(t *testing.T) {
	extra := []string{"internal.example.com", "admin.example.com"}

	if !Reserved("thing.internal.example.com", "apps.example.com", extra) {
		t.Fatal("a host under an extra reserved domain must be reserved")
	}
	if Reserved("evilinternal.example.com", "apps.example.com", extra) {
		t.Fatal("extra domains must also match on the label boundary")
	}
	if Reserved("customer.test", "apps.example.com", extra) {
		t.Fatal("an unrelated host must not be reserved")
	}
}

func TestReservedWithNoAppDomainStillHonoursExtras(t *testing.T) {
	// The feature can be off while an operator still reserves names.
	if !Reserved("thing.internal.example.com", "", []string{"internal.example.com"}) {
		t.Fatal("extras must apply even with no app domain")
	}
	if Reserved("anything.test", "", nil) {
		t.Fatal("nothing is reserved when neither is configured")
	}
}

// Generated names are <app>-<environment>, one label, and keep what tells
// them apart when they have to be cut to fit.
func TestGeneratedLabels(t *testing.T) {
	if got := SuggestLabel("Web", "production"); got != "web-production" {
		t.Errorf("SuggestLabel = %q, want web-production", got)
	}
	long := strings.Repeat("a", 60)
	if got := SuggestLabel(long, "production"); len(got) != MaxLabel || !strings.HasSuffix(got, "-production") {
		t.Errorf("SuggestLabel(60 a's) = %q (%d), want 63 characters ending -production", got, len(got))
	}
	// A cut that lands on a dash must not leave two in a row, or end in one.
	dashed := strings.Repeat("ab-", 20)
	if got := WithSuffix(dashed, "x1y2"); strings.Contains(got, "--") || len(got) > MaxLabel || ValidateLabel(got) != nil {
		t.Errorf("WithSuffix = %q, want a valid label", got)
	}
	if got := WithSuffix("web-production", "a1b2"); got != "web-production-a1b2" {
		t.Errorf("WithSuffix = %q", got)
	}

	for label, ok := range map[string]bool{
		"web-production":        true,
		strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false,
		"":                      false,
		"-web":                  false,
		"web-":                  false,
		"we_b":                  false,
		"a.b":                   false,
	} {
		if err := ValidateLabel(label); (err == nil) != ok {
			t.Errorf("ValidateLabel(%q) = %v, want ok=%t", label, err, ok)
		}
	}
}

// The platform wildcard carries *.<domain> and *.up.<domain>: a generated
// name is covered, and still nothing deeper is.
func TestThePlatformWildcardCoversGeneratedNames(t *testing.T) {
	const d = "apps.example.com"
	for host, want := range map[string]bool{
		"web.apps.example.com":               true,
		"web-production.up.apps.example.com": true,
		"up.apps.example.com":                true,
		"a.b.up.apps.example.com":            false,
		"a.b.apps.example.com":               false,
		"shop.customer.test":                 false,
	} {
		if got := CoveredByPlatformWildcard(host, d); got != want {
			t.Errorf("CoveredByPlatformWildcard(%q) = %t, want %t", host, got, want)
		}
	}
	if GeneratedDomain("") != "" || GeneratedDomain("Apps.Example.com.") != "up.apps.example.com" {
		t.Error("GeneratedDomain does not normalise, or invents one with no app domain")
	}
}

// A custom domain has to be a name that can be pointed and certified. An IP
// address and an over-long label both matched the dotted-name pattern and were
// stored, then sat unverifiable forever (CONFIG-8).
func TestCheckCustomHost(t *testing.T) {
	label63 := strings.Repeat("x", 63)
	ok := []string{
		"shop.example.com",
		"example.com",
		label63 + ".example.com",
		"1password.com",
		"a.b.c.example.co.tz",
	}
	for _, h := range ok {
		if err := checkCustomHost(h); err != nil {
			t.Errorf("checkCustomHost(%q) = %v, want nil", h, err)
		}
	}

	long := strings.Repeat(label63+".", 4) + "com" // 4*64+3 = 259 > 253
	bad := []string{
		"",
		"10.0.0.1",
		"192.168.1.256",
		"example.123",
		label63 + "x.example.com",
		"shop." + label63 + "x",
		long,
		"localhost",
		"not a domain",
		"*.example.com",
		"::1",
	}
	for _, h := range bad {
		if err := checkCustomHost(h); err == nil {
			t.Errorf("checkCustomHost(%q) = nil, want an error", h)
		}
	}
}

// The install's own names, refused as custom domains: a zone with every name
// under it, a name alone. A name is not a suffix — the registry may be
// ghcr.io, and nothing under it is anybody's to refuse.
func TestReservationsRefuseTheInstallsOwnNames(t *testing.T) {
	r := Reservations{
		AppDomain: "apps.example.com",
		Zones:     []string{"yacht.example.com"},
		Names:     []string{"registry.example.com", "edge.example.com"},
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"web.apps.example.com", true},
		{"yacht.example.com", true},
		{"YACHT.example.com.", true},
		{"evil.yacht.example.com", true},
		{"notyacht.example.com", false},
		{"registry.example.com", true},
		{"edge.example.com", true},
		{"x.registry.example.com", false},
		{"shop.customer.test", false},
		{"", false},
	} {
		if got := r.Refuses(tc.host); got != tc.want {
			t.Errorf("Refuses(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// Every shape an install's own address is configured in comes down to the
// hostname somebody would type as a custom domain.
func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://Yacht.Example.com":        "yacht.example.com",
		"https://yacht.example.com:8443/x": "yacht.example.com",
		"http://garage.internal:3900":      "garage.internal",
		"registry.example.com:5000":        "registry.example.com",
		"registry.example.com/team/app":    "registry.example.com",
		"tcp.example.com.":                 "tcp.example.com",
		" edge.example.com ":               "edge.example.com",
		"10.0.0.5:5000":                    "",
		"https://[2001:db8::1]:443":        "",
		"203.0.113.7":                      "",
		"":                                 "",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A registrable domain configured as a zone is refused alone: a dashboard at
// example.com must not take every name the company has from every team. The
// allow list frees the install's own names and nothing the operator chose.
func TestReservationsSpareAnApexAndWhatIsAllowed(t *testing.T) {
	r := Reservations{
		AppDomain: "apps.example.com",
		Reserved:  []string{"internal.example.com"},
		Zones:     []string{"example.com", "yacht.example.co.uk", "s3.example.org"},
		Names:     []string{"registry.example.net"},
		Allowed:   []string{"shop.s3.example.org", "internal.example.com", "registry.example.net"},
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"www.example.com", false},
		{"yacht.example.co.uk", true},
		{"x.yacht.example.co.uk", true},
		{"bucket.s3.example.org", true},
		{"shop.s3.example.org", false},
		{"a.shop.s3.example.org", false},
		{"registry.example.net", false},
		{"x.internal.example.com", true},
		{"web.apps.example.com", true},
	} {
		if got := r.Refuses(tc.host); got != tc.want {
			t.Errorf("Refuses(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// What the platform issues is checked against the install's own names as
// names alone, never against the app domain: a dashboard at the app domain's
// apex must not refuse every hostname under it.
func TestIssuableRefusesOnlyTheInstallsOwnNames(t *testing.T) {
	r := Reservations{
		AppDomain: "apps.example.com",
		Reserved:  []string{"admin.apps.example.com"},
		Zones:     []string{"apps.example.com"},
		Names:     []string{"tcp.up.apps.example.com"},
	}
	for host, want := range map[string]bool{
		"web.up.apps.example.com":  true,
		"tcp.up.apps.example.com":  false,
		"apps.example.com":         false,
		"admin.apps.example.com":   false,
		"x.admin.apps.example.com": false,
		"other.apps.example.com":   true,
	} {
		if got := r.Issuable(host); got != want {
			t.Errorf("Issuable(%q) = %v, want %v", host, got, want)
		}
	}
}

// The install's addresses as configured, read as the ASCII hostname a custom
// domain is spelled as; a *. prefix means everything under the name; a value
// that is not a host is an error, not nothing.
func TestPlatformHost(t *testing.T) {
	for _, tc := range []struct {
		in       string
		host     string
		wildcard bool
		bad      bool
	}{
		{in: "https://Bücher.example", host: "xn--bcher-kva.example"},
		{in: "*.example.com", host: "example.com", wildcard: true},
		{in: "https://*.example.com", host: "example.com", wildcard: true},
		{in: "10.0.0.5:5000"},
		{in: ""},
		{in: "https://", bad: true},
		{in: "http://exa mple.com", bad: true},
		{in: "not a host", bad: true},
		{in: "-bad-.example.com", bad: true},
	} {
		host, wildcard, err := PlatformHost(tc.in)
		if (err != nil) != tc.bad || host != tc.host || wildcard != tc.wildcard {
			t.Errorf("PlatformHost(%q) = %q, %v, %v; want %q, %v, error %v",
				tc.in, host, wildcard, err, tc.host, tc.wildcard, tc.bad)
		}
	}
}
