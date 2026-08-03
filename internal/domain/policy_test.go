package domain

import (
	"errors"
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
