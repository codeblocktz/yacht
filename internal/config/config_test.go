package config

import (
	"maps"
	"os"
	"regexp"
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// A database URL is required by validate(); supply one so these tests are
	// about the new variables and nothing else.
	t.Setenv("YACHT_DATABASE_URL", "postgres://localhost/yacht?sslmode=disable")
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestAppDomainDefaultsToOff(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AppDomain != "" {
		t.Fatalf("AppDomain = %q, want empty by default", c.AppDomain)
	}
	if c.WildcardTLS {
		t.Fatal("WildcardTLS should default to false")
	}
}

func TestBuildAdmissionCeilingLoadsAndIsBounded(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_MAX_CONCURRENT_BUILDS": "3"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxConcurrentBuilds != 3 {
		t.Fatalf("MaxConcurrentBuilds = %d, want 3", c.MaxConcurrentBuilds)
	}

	setEnv(t, map[string]string{"YACHT_MAX_CONCURRENT_BUILDS": "0"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YACHT_MAX_CONCURRENT_BUILDS") {
		t.Fatalf("Load accepted an unbounded-zero build ceiling: %v", err)
	}
}

func TestAppDomainAndReservedDomainsLoad(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_APP_DOMAIN":       "apps.example.com",
		"YACHT_WILDCARD_TLS":     "true",
		"YACHT_RESERVED_DOMAINS": "internal.example.com, admin.example.com",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AppDomain != "apps.example.com" {
		t.Fatalf("AppDomain = %q", c.AppDomain)
	}
	if !c.WildcardTLS {
		t.Fatal("WildcardTLS = false, want true")
	}
	if len(c.ReservedDomains) != 2 ||
		c.ReservedDomains[0] != "internal.example.com" ||
		c.ReservedDomains[1] != "admin.example.com" {
		t.Fatalf("ReservedDomains = %v, want both entries trimmed", c.ReservedDomains)
	}
}

// Wildcard TLS with nothing to apply it to is incoherent rather than merely
// useless: the operator believes apps are served over TLS and nothing says
// otherwise. Fail at startup instead.
func TestWildcardTLSWithoutAnAppDomainFails(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_WILDCARD_TLS": "true"})
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted YACHT_WILDCARD_TLS with no YACHT_APP_DOMAIN")
	}
	if !strings.Contains(err.Error(), "YACHT_APP_DOMAIN") {
		t.Fatalf("error should name the missing variable, got: %v", err)
	}
}

func TestCertIssuerLoadsAndIsValidated(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_CERT_ISSUER": "yacht-acme"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CertIssuer != "yacht-acme" {
		t.Fatalf("CertIssuer = %q, want yacht-acme", c.CertIssuer)
	}

	for _, bad := range []string{"Yacht_ACME", "has space", "-leading"} {
		setEnv(t, map[string]string{"YACHT_CERT_ISSUER": bad})
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YACHT_CERT_ISSUER") {
			t.Errorf("%q: err = %v, want it refused by name", bad, err)
		}
	}
}

func TestMalformedAppDomainFails(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_APP_DOMAIN": "not a domain"})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a malformed YACHT_APP_DOMAIN")
	}
}

// Every variable config.go reads must be documented, or an operator finds out
// a setting exists by reading the source.
func TestEveryVariableIsDocumented(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	for _, name := range []string{
		"YACHT_APP_DOMAIN", "YACHT_WILDCARD_TLS", "YACHT_RESERVED_DOMAINS",
		"YACHT_CERT_ISSUER",
	} {
		if !strings.Contains(string(src), name) {
			t.Fatalf("%s is not read by config.go", name)
		}
		if !strings.Contains(string(example), name) {
			t.Fatalf("%s is not documented in .env.example", name)
		}
	}
}

// An app domain that cannot produce a valid hostname is a startup
// misconfiguration, not a per-app runtime failure. Without this check the
// operator learns about it only when a create fails, one app at a time.
func TestOverlongAppDomainFailsAtStartup(t *testing.T) {
	cases := map[string]string{
		"whole name too long for any app": strings.Repeat("a", 120) + "." +
			strings.Repeat("b", 120) + ".example.com",
		"single label over 63": strings.Repeat("a", 70) + ".example.com",
	}

	for name, domain := range cases {
		t.Run(name, func(t *testing.T) {
			setEnv(t, map[string]string{"YACHT_APP_DOMAIN": domain})
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted an app domain that can never yield a hostname (len=%d)", len(domain))
			}
		})
	}
}

func TestUsableAppDomainStillLoads(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_APP_DOMAIN": "apps.example.com"})
	if _, err := Load(); err != nil {
		t.Fatalf("Load rejected a perfectly ordinary app domain: %v", err)
	}
}

// Garbage in a reserved list is worse than an empty one: it reads as though
// names are protected while matching nothing.
func TestReservedDomainsAreValidated(t *testing.T) {
	for _, bad := range []string{"not a domain", "https://evil.test", "*", "evil.test/path"} {
		t.Run(bad, func(t *testing.T) {
			setEnv(t, map[string]string{"YACHT_RESERVED_DOMAINS": bad})
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %q as a reserved domain", bad)
			}
		})
	}
}

func TestValidReservedDomainsLoad(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_RESERVED_DOMAINS": "internal.example.com, admin.example.com",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.ReservedDomains) != 2 {
		t.Fatalf("ReservedDomains = %v, want 2", c.ReservedDomains)
	}
}

// Retired keys with no active key is the shape of a rotation somebody got
// half-way through — the new key not pasted in, or the old one moved out of
// YACHT_SECRET_KEY before the new one arrived.
//
// Started rather than refused, it looks like the safest possible state and is
// the opposite: no key seals, no key opens, and every secret variable fails at
// deploy time pointing at the app rather than at the setting.
func TestRetiredKeysWithoutAnActiveKeyFail(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_SECRET_KEY":          "",
		"YACHT_SECRET_KEY_PREVIOUS": "aGVsbG8td29ybGQtdGhpcnR5LXR3by1ieXRlcyE=",
	})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted retired keys with nothing sealing")
	} else if !strings.Contains(err.Error(), "YACHT_SECRET_KEY_PREVIOUS") {
		t.Errorf("the failure does not name the setting at fault: %v", err)
	}
}

// The two together are the state an install is in mid-rotation, and it has to
// start — this is the whole point of holding a retired key.
func TestAnActiveKeyWithRetiredKeysLoads(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_SECRET_KEY":          "YS1rZXktdGhhdC1pcy10aGlydHktdHdvLWJ5dGVzIQ==",
		"YACHT_SECRET_KEY_PREVIOUS": "b25lLXJldGlyZWQta2V5LXRoaXJ0eS10d28tYnl0ZQ==, dHdvLXJldGlyZWQta2V5LXRoaXJ0eS10d28tYnl0ZQ==",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.SecretKeyPrevious) != 2 {
		t.Fatalf("SecretKeyPrevious = %v, want both entries", c.SecretKeyPrevious)
	}
	// Trimmed, because a list an operator edits by hand grows spaces after the
	// commas and a key with a leading space is not base64.
	if strings.HasPrefix(c.SecretKeyPrevious[1], " ") {
		t.Errorf("entry %q kept its leading space", c.SecretKeyPrevious[1])
	}
}

// The install that has never rotated, which is nearly all of them. The setting
// is present and empty — the installer writes the line either way — and that
// must yield no retired keys rather than one empty one, which the keeper would
// refuse to decode and stop startup over.
func TestAnEmptyRetiredListHoldsNoKeys(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_SECRET_KEY":          "YS1rZXktdGhhdC1pcy10aGlydHktdHdvLWJ5dGVzIQ==",
		"YACHT_SECRET_KEY_PREVIOUS": "",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.SecretKeyPrevious) != 0 {
		t.Fatalf("SecretKeyPrevious = %v, want none", c.SecretKeyPrevious)
	}
}

// Turning sign-in on with no way to deliver a link locks the operator out of
// their own dashboard permanently. Accounts stay off unless a link can be
// delivered and a URL exists to put in it.
func TestAccountsRequireABaseURL(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_SMTP_ADDR": "smtp.example.test:587"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AccountsEnabled() {
		t.Fatal("accounts enabled with no YACHT_BASE_URL — the link would point nowhere")
	}
}

func TestAccountsEnabledWithBaseURL(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_BASE_URL": "https://yacht.example.test"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.AccountsEnabled() {
		t.Fatal("accounts should be enabled once a base URL is set")
	}
}

func TestSMTPAndResendTogetherFail(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_BASE_URL":       "https://yacht.example.test",
		"YACHT_SMTP_ADDR":      "smtp.example.test:587",
		"YACHT_SMTP_FROM":      "yacht@example.test",
		"YACHT_RESEND_API_KEY": "re_123",
	})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted two mail transports at once")
	}
}

func TestCloudflareEmailIsATransport(t *testing.T) {
	cf := map[string]string{
		"YACHT_BASE_URL":                    "https://yacht.example.test",
		"YACHT_CLOUDFLARE_EMAIL_TOKEN":      "tok",
		"YACHT_CLOUDFLARE_EMAIL_ACCOUNT_ID": "acct",
		"YACHT_CLOUDFLARE_EMAIL_FROM":       "Acme <no-reply@example.test>",
	}
	setEnv(t, cf)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.MailTransport(); got != "cloudflare" {
		t.Errorf("MailTransport = %q, want cloudflare", got)
	}

	for _, missing := range []string{"YACHT_CLOUDFLARE_EMAIL_ACCOUNT_ID", "YACHT_CLOUDFLARE_EMAIL_FROM"} {
		env := map[string]string{}
		for k, v := range cf {
			if k != missing {
				env[k] = v
			}
		}
		setEnv(t, env)
		t.Setenv(missing, "")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("without %s: Load = %v, want an error naming it", missing, err)
		}
	}

	setEnv(t, cf)
	t.Setenv("YACHT_RESEND_API_KEY", "re_123")
	t.Setenv("YACHT_RESEND_FROM", "yacht@example.test")
	if _, err := Load(); err == nil {
		t.Error("Load accepted Resend and Cloudflare at once")
	}
}

func TestSMTPWithoutFromFails(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_BASE_URL":  "https://yacht.example.test",
		"YACHT_SMTP_ADDR": "smtp.example.test:587",
	})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted SMTP with no from address")
	}
}

func TestMalformedBaseURLFails(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_BASE_URL": "not a url"})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a malformed base URL")
	}
}

// Open sign-up loads only on an install that keeps strangers out of the Admin
// area, keeps its own team for its owner, and can mail them their link.
func TestOpenSignupNeedsOperatorsAnOwnerAndMail(t *testing.T) {
	complete := map[string]string{
		"YACHT_BASE_URL":       "https://yacht.example.test",
		"YACHT_OPEN_SIGNUP":    "true",
		"YACHT_OPERATORS":      "ops@example.test",
		"YACHT_OWNER_EMAIL":    "owner@example.test",
		"YACHT_RESEND_API_KEY": "re_123",
		"YACHT_RESEND_FROM":    "yacht@example.test",
	}
	setEnv(t, complete)
	c, err := Load()
	if err != nil || !c.OpenSignup {
		t.Fatalf("Load = %v, OpenSignup %v; want a complete open install to load", err, c.OpenSignup)
	}

	for _, missing := range []string{
		"YACHT_OPERATORS", "YACHT_OWNER_EMAIL", "YACHT_RESEND_API_KEY", "YACHT_BASE_URL",
	} {
		t.Run(missing, func(t *testing.T) {
			env := maps.Clone(complete)
			env[missing] = ""
			if missing == "YACHT_RESEND_API_KEY" {
				env["YACHT_RESEND_FROM"] = ""
			}
			setEnv(t, env)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YACHT_OPEN_SIGNUP") {
				t.Fatalf("Load with open sign-up and no %s = %v, want it refused", missing, err)
			}
		})
	}
}

// TestEveryVariableReadIsDocumented scans for every YACHT_* string config.go
// reads, rather than checking a list someone has to remember to extend. The
// hardcoded version passed while YACHT_SHUTDOWN_TIMEOUT went undocumented.
func TestEveryVariableReadIsDocumented(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	found := regexp.MustCompile(`"(YACHT_[A-Z_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatal("no YACHT_* variables found in config.go — the scan is broken")
	}

	seen := map[string]bool{}
	for _, m := range found {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(string(example), name) {
			t.Errorf("%s is read by config.go but not documented in .env.example", name)
		}
	}
}

// The default region becomes a node affinity on every app placed in it, so
// it is held to the label rule a region is, at startup rather than at the
// first deploy.
func TestDefaultRegionLoadsAndIsValidated(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_DEFAULT_REGION": " fsn "})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DefaultRegion != "fsn" {
		t.Fatalf("DefaultRegion = %q, want fsn", c.DefaultRegion)
	}

	for _, bad := range []string{"FSN", "fsn;id", "-fsn", strings.Repeat("a", 64)} {
		setEnv(t, map[string]string{"YACHT_DEFAULT_REGION": bad})
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YACHT_DEFAULT_REGION") {
			t.Errorf("Load accepted YACHT_DEFAULT_REGION=%q: %v", bad, err)
		}
	}
}

// TCP proxies are off until the operator names where they are reached, and
// take the NodePort range's low band unless told otherwise.
func TestTCPProxyLoads(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.TCPProxyHost != "" {
		t.Errorf("TCPProxyHost = %q, want empty by default", c.TCPProxyHost)
	}
	if first, last, err := c.TCPProxyRange(); err != nil || first != 30000 || last != 30085 {
		t.Errorf("TCPProxyRange = %d-%d, %v; want the default 30000-30085", first, last, err)
	}

	setEnv(t, map[string]string{
		"YACHT_TCP_PROXY_HOST":  "Proxy.Example.com.",
		"YACHT_TCP_PROXY_PORTS": "31000-31009",
	})
	if c, err = Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.TCPProxyHost != "proxy.example.com" {
		t.Errorf("TCPProxyHost = %q, want it normalised", c.TCPProxyHost)
	}
	if first, last, _ := c.TCPProxyRange(); first != 31000 || last != 31009 {
		t.Errorf("TCPProxyRange = %d-%d, want 31000-31009", first, last)
	}

	for _, bad := range []string{"30085-30000", "0-10", "30000-70000", "thirty"} {
		setEnv(t, map[string]string{"YACHT_TCP_PROXY_PORTS": bad})
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YACHT_TCP_PROXY_PORTS") {
			t.Errorf("Load accepted YACHT_TCP_PROXY_PORTS=%q: %v", bad, err)
		}
	}
}

// Half a bucket is refused at startup, because it would leave every backup on
// a volume while the operator believed they were off-site.
func TestBackupBucketIsAllOrNothing(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_BACKUP_S3_ENDPOINT": "https://acct.r2.cloudflarestorage.com",
		"YACHT_BACKUP_S3_BUCKET":   "datahuru-backups",
	})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "YACHT_BACKUP_S3_ACCESS_KEY") {
		t.Fatalf("Load = %v, want the missing keys named", err)
	}

	t.Setenv("YACHT_BACKUP_S3_ACCESS_KEY", "AKID")
	t.Setenv("YACHT_BACKUP_S3_SECRET_KEY", "secret")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s3, err := c.BackupStore()
	if err != nil || s3 == nil || s3.Prefix != "databases" || s3.Region != "us-east-1" {
		t.Fatalf("store = %+v, %v", s3, err)
	}
	if strings.Contains(c.String(), "secret") {
		t.Error("the secret key is in what startup logs")
	}

	t.Setenv("YACHT_BACKUP_S3_ENDPOINT", "r2.example")
	if _, err := Load(); err == nil {
		t.Error("an endpoint that is not a URL was accepted")
	}
}

func TestNoBucketKeepsBackupsOnAVolume(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s3, err := c.BackupStore(); s3 != nil || err != nil {
		t.Errorf("store = %+v, %v; want none", s3, err)
	}
}

// A Turnstile site is both keys or neither: half of one either draws a
// widget nothing checks or refuses every sign-in.
func TestTurnstileKeysGoTogether(t *testing.T) {
	base := map[string]string{"YACHT_BASE_URL": "https://yacht.example.test"}
	for _, tc := range []struct {
		site, secret string
		ok           bool
	}{
		{"", "", true},
		{"1x00000000000000000000AA", "1x0000000000000000000000000000000AA", true},
		{"1x00000000000000000000AA", "", false},
		{"", "1x0000000000000000000000000000000AA", false},
	} {
		env := maps.Clone(base)
		env["YACHT_TURNSTILE_SITE_KEY"], env["YACHT_TURNSTILE_SECRET"] = tc.site, tc.secret
		setEnv(t, env)
		c, err := Load()
		if (err == nil) != tc.ok {
			t.Errorf("site %q secret %q: Load = %v, want ok %v", tc.site, tc.secret, err, tc.ok)
		}
		if err == nil && c.TurnstileSiteKey != tc.site {
			t.Errorf("site key = %q, want %q", c.TurnstileSiteKey, tc.site)
		}
	}
}
