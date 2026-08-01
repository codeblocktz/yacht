// Package config loads runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
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

	// OwnerEmail is the address that may sign in to a fresh install before
	// anybody has an account.
	//
	// Links go only to addresses that already exist, which is what stops the
	// sign-in form filling the user table — and leaves a new install with
	// accounts on unenterable. This names the one exception, and only until it
	// has an account of its own.
	OwnerEmail string

	// SecretKey seals secret environment variables, base64 of 32 bytes.
	//
	// Empty means secrets are refused rather than stored readable. Losing it
	// means losing every secret sealed with it — there is no recovery path,
	// because a recovery path is a second way in.
	SecretKey string

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

	// BaseURL is the public URL the dashboard is reached at, such as
	// https://yacht.example.com. It is what a sign-in link is built from, and
	// setting it is what switches accounts on: without it there is no address
	// to put in the link, so there is no way to sign in.
	BaseURL string

	// SMTP describes a relay to send sign-in links through. SMTPAddr empty
	// means no relay is configured.
	SMTPAddr     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string

	// ResendAPIKey and ResendFrom select Resend's HTTP API instead of a relay.
	ResendAPIKey string
	ResendFrom   string

	// SessionTTL is how long a signed-in browser stays signed in.
	SessionTTL time.Duration

	// MagicLinkTTL is how long a sign-in link remains redeemable. Short,
	// because the link is a bearer credential sitting in a mailbox.
	MagicLinkTTL time.Duration

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
		OwnerEmail:      strings.TrimSpace(env("YACHT_OWNER_EMAIL", "")),
		SecretKey:       strings.TrimSpace(env("YACHT_SECRET_KEY", "")),
		AppDomain:       env("YACHT_APP_DOMAIN", ""),
		WildcardTLS:     envBool("YACHT_WILDCARD_TLS", false),
		ReservedDomains: envList("YACHT_RESERVED_DOMAINS"),
		BaseURL:         strings.TrimRight(env("YACHT_BASE_URL", ""), "/"),
		SMTPAddr:        env("YACHT_SMTP_ADDR", ""),
		SMTPUsername:    env("YACHT_SMTP_USER", ""),
		SMTPPassword:    env("YACHT_SMTP_PASSWORD", ""),
		SMTPFrom:        env("YACHT_SMTP_FROM", ""),
		ResendAPIKey:    env("YACHT_RESEND_API_KEY", ""),
		ResendFrom:      env("YACHT_RESEND_FROM", ""),
		SessionTTL:      envDuration("YACHT_SESSION_TTL", 720*time.Hour),
		MagicLinkTTL:    envDuration("YACHT_MAGIC_LINK_TTL", 15*time.Minute),
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
	if c.OwnerEmail != "" {
		if _, err := mail.ParseAddress(c.OwnerEmail); err != nil {
			errs = append(errs, errors.New("YACHT_OWNER_EMAIL must be an email address"))
		}
	}
	errs = append(errs, c.accountFaults()...)

	return errors.Join(errs...)
}

// accountFaults validates the sign-in settings, and only once accounts are on.
//
// With no base URL nothing ever sends mail, so a half-finished relay block is
// inert; refusing to start over it would stop an install that had simply left
// the settings lying around. Once accounts are on the same settings decide
// whether anybody can get in, and each of these would be discovered at the
// first sign-in attempt — by which point the operator reads it as sign-in
// being broken rather than as a setting they got wrong.
func (c Config) accountFaults() []error {
	if !c.AccountsEnabled() {
		return nil
	}

	var errs []error
	if u, err := url.Parse(c.BaseURL); err != nil {
		errs = append(errs, fmt.Errorf("YACHT_BASE_URL is not a URL: %w", err))
	} else if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New(
			"YACHT_BASE_URL must be an absolute http or https URL, such as https://yacht.example.com"))
	}

	// Two transports is not a fallback chain, it is an unanswered question
	// about which one sends. Say so rather than picking.
	if c.SMTPAddr != "" && c.ResendAPIKey != "" {
		errs = append(errs, errors.New(
			"YACHT_SMTP_ADDR and YACHT_RESEND_API_KEY are both set — configure one mail transport"))
	}
	if c.SMTPAddr != "" && c.SMTPFrom == "" {
		errs = append(errs, errors.New("YACHT_SMTP_FROM is required with YACHT_SMTP_ADDR"))
	}
	if c.ResendAPIKey != "" && c.ResendFrom == "" {
		errs = append(errs, errors.New("YACHT_RESEND_FROM is required with YACHT_RESEND_API_KEY"))
	}
	// A zero or negative lifetime issues credentials that are already expired,
	// which presents as sign-in silently never working.
	if c.SessionTTL <= 0 {
		errs = append(errs, errors.New("YACHT_SESSION_TTL must be positive"))
	}
	if c.MagicLinkTTL <= 0 {
		errs = append(errs, errors.New("YACHT_MAGIC_LINK_TTL must be positive"))
	}
	return errs
}

// Unauthenticated reports whether the dashboard will accept any caller.
func (c Config) Unauthenticated() bool { return c.AuthToken == "" }

// AccountsEnabled reports whether the dashboard signs people in.
//
// A base URL and nothing else is enough: with no relay configured the sign-in
// link is written to the log, which is the documented way back in when mail
// breaks. What cannot be recovered from is the reverse — sign-in switched on
// with no address to put in the link, which locks the operator out of their own
// dashboard for good.
func (c Config) AccountsEnabled() bool { return c.BaseURL != "" }

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
		"addr=%s db=%s kubeconfig=%s in_cluster=%t auth_token=%s owner=%s "+
			"accounts=%t mail=%s debug=%t",
		c.Addr, redactDSN(c.DatabaseURL), orNone(c.Kubeconfig),
		c.KubeInCluster, token, c.OwnerID,
		c.AccountsEnabled(), c.MailTransport(), c.Debug,
	)
}

// MailTransport names the transport sign-in links will be delivered through.
//
// "log" is not an error state: it is the break-glass path an install with no
// relay runs on, and naming it at startup is what stops an operator believing
// mail is configured when it is not.
func (c Config) MailTransport() string {
	switch {
	case c.SMTPAddr != "":
		return "smtp"
	case c.ResendAPIKey != "":
		return "resend"
	default:
		return "log"
	}
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
