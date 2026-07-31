// Package config loads runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var appDomainRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

// DNS limits, and the app-name limit they have to accommodate.
//
// maxAppName mirrors the 40-character cap app.CreateInput.Validate enforces.
// It is duplicated rather than imported because config sits below app in the
// dependency order; if the two ever disagree, this one is the conservative
// side and fails at startup rather than per-app.
const (
	maxHostname = 253
	maxLabel    = 63
	maxAppName  = 40
)

// domainFault returns why a domain is unusable, or "" if it is fine.
func domainFault(d string) string {
	d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
	switch {
	case d == "":
		return "must not be empty"
	case !appDomainRE.MatchString(d):
		return "must be a valid dotted domain name"
	}
	for _, label := range strings.Split(d, ".") {
		if len(label) > maxLabel {
			return fmt.Sprintf("label %q exceeds %d characters", label, maxLabel)
		}
	}
	return ""
}

// Config is everything the engine needs to start.
type Config struct {
	// Addr is the listen address for the dashboard.
	Addr string

	// DatabaseURL is a Postgres connection string. Required.
	DatabaseURL string

	// Kubeconfig points at a cluster. Empty uses the default client-go
	// loading rules, or in-cluster config when KubeInCluster is set.
	Kubeconfig    string
	KubeInCluster bool

	// AuthToken, when set, requires a bearer token on every request. Empty
	// means the dashboard is unauthenticated and suitable only for a trusted
	// network — the engine warns loudly about this at startup.
	AuthToken string

	// OwnerID identifies the single owner every resource belongs to.
	OwnerID   string
	OwnerName string

	// AppDomain is the platform domain every app gets a hostname under, such
	// as apps.example.com. Empty switches per-app hostnames off entirely.
	AppDomain string

	// WildcardTLS serves platform hostnames over TLS using the ingress
	// controller's configured default certificate.
	//
	// Yacht cannot verify that default exists. An install without one serves
	// the wrong certificate rather than failing, so this is logged at startup
	// and shown on the settings page — a capability the engine cannot check is
	// one it should be noisy about.
	WildcardTLS bool

	// ReservedDomains are additional suffixes no tenant may claim. AppDomain
	// is always reserved whether or not it appears here.
	ReservedDomains []string

	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration

	// Debug enables verbose logging.
	Debug bool
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	c := Config{
		Addr:            env("YACHT_ADDR", ":8080"),
		DatabaseURL:     env("YACHT_DATABASE_URL", ""),
		Kubeconfig:      env("YACHT_KUBECONFIG", os.Getenv("KUBECONFIG")),
		KubeInCluster:   envBool("YACHT_KUBE_IN_CLUSTER", false),
		AuthToken:       env("YACHT_AUTH_TOKEN", ""),
		OwnerID:         env("YACHT_OWNER_ID", "owner-local"),
		OwnerName:       env("YACHT_OWNER_NAME", "Local"),
		AppDomain:       env("YACHT_APP_DOMAIN", ""),
		WildcardTLS:     envBool("YACHT_WILDCARD_TLS", false),
		ReservedDomains: envList("YACHT_RESERVED_DOMAINS"),
		ShutdownTimeout: envDuration("YACHT_SHUTDOWN_TIMEOUT", 15*time.Second),
		Debug:           envBool("YACHT_DEBUG", false),
	}

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	var errs []error

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("YACHT_DATABASE_URL is required"))
	}
	if c.Addr == "" {
		errs = append(errs, errors.New("YACHT_ADDR must not be empty"))
	}
	if c.OwnerID == "" {
		errs = append(errs, errors.New("YACHT_OWNER_ID must not be empty"))
	}
	// A short shared secret is worse than none, because it invites exposing
	// the dashboard while providing no real protection.
	if c.AuthToken != "" && len(c.AuthToken) < 16 {
		errs = append(errs, errors.New("YACHT_AUTH_TOKEN must be at least 16 characters"))
	}
	if c.AppDomain != "" {
		if fault := domainFault(c.AppDomain); fault != "" {
			errs = append(errs, fmt.Errorf("YACHT_APP_DOMAIN %s", fault))
		} else if n := len(c.AppDomain) + 1 + maxAppName; n > maxHostname {
			// An app domain long enough that the longest legal app name
			// overflows the DNS limit is a startup misconfiguration. Left
			// unchecked the operator meets it one failed create at a time,
			// with nothing connecting the failure to the setting.
			errs = append(errs, fmt.Errorf(
				"YACHT_APP_DOMAIN is too long: an app name of %d characters would "+
					"produce a %d-character hostname, over the %d-character limit",
				maxAppName, n, maxHostname))
		}
	}
	// A reserved list full of things that match nothing reads as protection
	// while providing none.
	for _, d := range c.ReservedDomains {
		if fault := domainFault(d); fault != "" {
			errs = append(errs, fmt.Errorf("YACHT_RESERVED_DOMAINS entry %q %s", d, fault))
		}
	}
	// TLS with nothing to apply it to leaves the operator believing apps are
	// served over TLS while nothing says otherwise.
	if c.WildcardTLS && c.AppDomain == "" {
		errs = append(errs, errors.New(
			"YACHT_WILDCARD_TLS requires YACHT_APP_DOMAIN — there would be no hostnames to serve"))
	}

	return errors.Join(errs...)
}

// Unauthenticated reports whether the dashboard will accept any caller.
func (c Config) Unauthenticated() bool { return c.AuthToken == "" }

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return b
}

// envList reads a comma-separated variable, trimming each entry and dropping
// empties, so a trailing comma or a stray space is not a silent extra entry.
func envList(key string) []string {
	raw := strings.Split(os.Getenv(key), ",")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return d
}

// String redacts secrets so a Config can be logged safely.
func (c Config) String() string {
	token := "unset"
	if c.AuthToken != "" {
		token = "set"
	}
	return fmt.Sprintf(
		"addr=%s db=%s kubeconfig=%s in_cluster=%t auth_token=%s owner=%s debug=%t",
		c.Addr, redactDSN(c.DatabaseURL), orNone(c.Kubeconfig),
		c.KubeInCluster, token, c.OwnerID, c.Debug,
	)
}

// redactDSN strips credentials from a connection string before logging.
func redactDSN(dsn string) string {
	if dsn == "" {
		return "unset"
	}
	// postgres://user:pass@host/db -> postgres://***@host/db
	if at := strings.LastIndex(dsn, "@"); at != -1 {
		if scheme := strings.Index(dsn, "://"); scheme != -1 && scheme+3 < at {
			return dsn[:scheme+3] + "***" + dsn[at:]
		}
	}
	return "set"
}

func orNone(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
