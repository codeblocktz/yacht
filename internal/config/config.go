// Package config loads runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

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
