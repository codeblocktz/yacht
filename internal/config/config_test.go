package config

import (
	"os"
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
	} {
		if !strings.Contains(string(src), name) {
			t.Fatalf("%s is not read by config.go", name)
		}
		if !strings.Contains(string(example), name) {
			t.Fatalf("%s is not documented in .env.example", name)
		}
	}
}
