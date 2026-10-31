// Package config loads runtime configuration from the environment.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/backup"
	"github.com/codeblocktz/yacht/internal/bucket"
	"github.com/codeblocktz/yacht/internal/github"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

var appDomainRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

// issuerNameRE is a Kubernetes object name: a DNS subdomain.
var issuerNameRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

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

	// OpenSignup lets anybody make an account: the sign-in form mails an
	// address nobody holds a link that creates it, with a team of its own.
	// Off by default, which keeps an install to its owner and the people
	// they invite.
	OpenSignup bool

	// RequestAccessURL is where somebody without an account asks for one,
	// when sign-up is not open: the sign-in page links to it.
	RequestAccessURL string

	// TurnstileSiteKey and TurnstileSecret are a Cloudflare Turnstile site's
	// keys. With both, the sign-in form — which is also open sign-up's —
	// draws the widget and is refused without a token Cloudflare accepts.
	// Without them the form is not checked, and the log says so when sign-up
	// is open.
	TurnstileSiteKey string
	TurnstileSecret  string

	// Operators are the people who run the install itself, as opposed to a
	// team on it: nodes, the registry, DNS, every team's quota. Email
	// addresses, matched case-insensitively against the signed-in person.
	//
	// Empty keeps the single-team reading — the owner of the team a request
	// acts as is the operator — which is right for an install with one team
	// and wrong for one hosting several: there, every team's owner would be
	// able to drain the nodes every other team runs on.
	Operators []string

	// SecretKey seals secret environment variables, base64 of 32 bytes.
	//
	// Empty means secrets are refused rather than stored readable. Losing it
	// means losing every secret sealed with it — there is no recovery path,
	// because a recovery path is a second way in.
	SecretKey string

	// SecretKeyPrevious are keys kept only to open values sealed before the
	// current one. Comma-separated, each the same shape as SecretKey.
	//
	// Nothing is ever sealed with these. They exist so that replacing a key is
	// not a flag day: the new key becomes YACHT_SECRET_KEY, the old one moves
	// here, and every stored value keeps opening until it has been written
	// again under the new key. An install that has never rotated sets only
	// YACHT_SECRET_KEY and this stays empty.
	SecretKeyPrevious []string

	// AppDomain is the platform domain apps get hostnames under, such as
	// apps.example.com: <app>-<environment>.up.<this> once somebody presses
	// Generate Domain, and <name>.<this> for apps given one before that
	// existed. Empty switches per-app hostnames off entirely.
	AppDomain string

	// WildcardTLS serves platform hostnames over TLS using the ingress
	// controller's configured default certificate.
	//
	// Yacht cannot verify that default exists. An install without one serves
	// the wrong certificate rather than failing, so this is logged at startup
	// and shown on the settings page — a capability the engine cannot check is
	// one it should be noisy about.
	WildcardTLS bool

	// CertIssuer names the cert-manager ClusterIssuer that gives each custom
	// domain a certificate of its own. Empty means there is none, and a custom
	// domain is served over plain HTTP — the wildcard above can never cover a
	// name outside the platform domain.
	//
	// Yacht only names it. Whether the issuer exists and can reach Let's
	// Encrypt is the installer's to check, and what became of each request is
	// read back and shown on the domain.
	CertIssuer string

	// ReservedDomains are additional suffixes no tenant may claim. AppDomain
	// is always reserved whether or not it appears here.
	ReservedDomains []string

	// DNSResolver pins custom-domain lookups to one nameserver, as host or
	// host:port — "1.1.1.1" or "10.0.0.53:53".
	//
	// Empty is the right answer almost always. By default Yacht asks the
	// domain's own nameservers, which hold no cache: a record created two
	// seconds ago verifies two seconds later. Naming a recursive resolver here
	// gives that up — every recursive resolver caches negative answers, and the
	// check made just before somebody creates a record is what puts one there.
	//
	// Worth setting for a split-horizon install, where the answer that matters
	// is the one a particular resolver gives rather than the one the public
	// internet would.
	DNSResolver string

	// TCPProxyHost is the name an app's TCP proxy is reached at, the
	// <host> in <host>:<port>. Empty switches TCP proxies off.
	//
	// Any name that resolves to the machines does: every machine answers on
	// every proxy's port. Yacht does not publish it; it is what the dashboard
	// tells somebody to connect to.
	TCPProxyHost string

	// TCPProxyPorts are the ports proxies are given, as YACHT_TCP_PROXY_PORTS
	// says them: a range, "30000-30085", both ends included. Read through
	// TCPProxyRange.
	//
	// They have to be NodePorts the cluster accepts — 30000-32767 unless its
	// API server was told otherwise — and ports the machines' firewalls let in
	// from the internet. The default is the low band of the NodePort range,
	// which Kubernetes keeps for ports chosen by hand and allocates to nobody
	// else unless the rest is full, so a port picked here is not one some other
	// Service already took.
	TCPProxyPorts string

	// NetworkIsolation is how far teams are kept apart on the cluster's
	// network: "team" (the default) refuses every connection into a team's
	// workloads from another team's or from a build; "strict" also limits
	// what workloads and builds may connect out to — DNS, their own team and
	// public addresses — so the nodes' network and anything else private is
	// out of reach; "off" writes no policy at all.
	NetworkIsolation string

	// EgressAllow are private ranges, as CIDRs, that strict isolation still
	// lets workloads and builds reach: an image registry on the LAN, say.
	EgressAllow string

	// BaseURL is the public URL the dashboard is reached at, such as
	// https://yacht.example.com. It is what a sign-in link is built from, and
	// setting it is what switches accounts on: without it there is no address
	// to put in the link, so there is no way to sign in.
	BaseURL string

	// BuildSourceURL is where a build fetches source the CLI uploaded, as
	// a build pod reaches the engine. Empty is BaseURL, which a build reaches
	// through the cluster's own ingress as it reaches anything public; set it
	// where that is not true — an engine behind a proxy the cluster cannot
	// loop back through, say.
	BuildSourceURL string

	// SMTP describes a relay to send sign-in links through. SMTPAddr empty
	// means no relay is configured.
	SMTPAddr     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string

	// ResendAPIKey and ResendFrom select Resend's HTTP API instead of a relay.
	ResendAPIKey string
	ResendFrom   string

	// CloudflareEmail* select Cloudflare Email Sending's HTTP API: for a host
	// that blocks port 465, the only one Cloudflare takes SMTP on.
	CloudflareEmailToken     string
	CloudflareEmailAccountID string
	CloudflareEmailFrom      string
	CloudflareEmailReplyTo   string

	// MailCompany, MailWebsite and MailReplyTo sign every mail: who runs the
	// install and where ("hosting by Example Ltd, Nairobi, Kenya"), its public
	// site, and an address a person reads replies at. A mail that says who it
	// is from and why reads less like phishing, to filters and to people.
	// Empty leaves each out; MailReplyTo falls back to the Cloudflare one.
	MailCompany string
	MailWebsite string
	MailReplyTo string

	// SessionTTL is how long a signed-in browser stays signed in.
	SessionTTL time.Duration

	// MagicLinkTTL is how long a sign-in link remains redeemable. Short,
	// because the link is a bearer credential sitting in a mailbox.
	MagicLinkTTL time.Duration

	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration

	// WakerAddr is where the cluster's ingress controller reaches the engine's
	// waker, as ip:port: the address a sleeping app's hostnames are routed to.
	// Empty means apps cannot sleep, because nothing could wake them.
	//
	// An IP, because it becomes a Service endpoint. Where the engine runs as a
	// process on a cluster node — the installer's layout — it is that node's
	// address; where it runs in the cluster, the cluster IP of a Service in
	// front of it. Either way the port must be open to the cluster's pods.
	WakerAddr string

	// WakerListen is where the engine serves the waker, by default every
	// interface on WakerAddr's port. Its own listener rather than a route on
	// the dashboard's: the waker answers any hostname the cluster sends it,
	// and none of those requests should be able to reach a dashboard page.
	WakerListen string

	// WakeTimeout bounds one wake, from asking for a sleeping app's pods to
	// one of them being ready. A request is held that long at most.
	WakeTimeout time.Duration

	// DefaultRegion is the region a new app is placed in when nobody chooses
	// one, and the one every app made before regions existed is recorded in.
	// A value of the topology.kubernetes.io/region label on the cluster's
	// nodes. Empty leaves it to the regions that are open: the only one, or
	// the first. Ignored on a cluster whose nodes carry no region.
	DefaultRegion string

	// StageChanges holds a person's edits to an app's variables and to the
	// settings that roll out until they press Deploy, as Railway does, rather
	// than deploying each one as it is saved. On unless set false.
	StageChanges bool

	// MaxConcurrentBuilds is the install-wide admission ceiling. It is paired
	// with per-container bounds in the build namespace so the count represents
	// a bounded amount of node capacity rather than an arbitrary pod count.
	MaxConcurrentBuilds int32

	// GitHubApp* are the install's GitHub App, which teams connect to build
	// private repositories and deploy on push without a webhook of their own.
	// All unset, there is no GitHub App and a repository is a pasted URL,
	// cloned anonymously. Any set, all are required, with BaseURL: GitHub
	// sends people back to an address under it.
	//
	// The private key is the PEM GitHub generates, given inline — newlines
	// may be written as \n, for an env file that holds one line per variable
	// — or as the path of a file holding it, never both.
	GitHubAppID             int64
	GitHubAppSlug           string
	GitHubAppPrivateKey     string
	GitHubAppPrivateKeyFile string
	GitHubAppWebhookSecret  string
	GitHubAppClientID       string
	GitHubAppClientSecret   string

	// BackupS3* are where database backups are written: an S3-compatible
	// bucket — R2, MinIO, AWS — under BackupS3Prefix. Endpoint, bucket,
	// access key and secret key together, or none of them; with none, each
	// database's backups are kept on a volume beside it, which survives a bad
	// migration and not the loss of the cluster.
	//
	// The keys stay in the engine. What a backup job in a customer's
	// namespace is given is a URL signed for its one object, so the bucket
	// may be shared with other things the install keeps, under another
	// prefix, without a customer's job being able to reach them.
	BackupS3Endpoint  string
	BackupS3Region    string
	BackupS3Bucket    string
	BackupS3Prefix    string
	BackupS3AccessKey string
	BackupS3SecretKey string

	// Buckets* are the install's Garage, which Add › Bucket makes customers'
	// buckets on: its admin API and the token it was started with, its S3 API
	// as the engine reaches it, and as apps and browsers do, and the region
	// requests are signed for. The admin URL, token and endpoint together, or
	// none of them; with none, Add › Bucket says buckets are not set up.
	BucketsAdminURL   string
	BucketsAdminToken string
	BucketsS3URL      string
	BucketsEndpoint   string
	BucketsRegion     string

	// BackupToolImage uploads and downloads dumps in a backup job; curl, run
	// as the database's own non-root user. Empty is the engine's pinned
	// default. An install that pulls from a mirror names its copy here.
	BackupToolImage string

	// LogsURL is a VictoriaLogs the cluster's container output is collected
	// into, and MetricsURL a VictoriaMetrics scraping the kubelets and the
	// ingress controller. Each is optional: without the first, a project's
	// Logs page reads the live pods, as an app's always has, and history ends
	// with the pod; without the second, Observability shows what
	// metrics-server says now, and no requests, latency or history. Neither
	// is ever exposed: the engine reads them and scopes every query to the
	// signed-in team's own apps (package observe).
	LogsURL    string
	MetricsURL string

	// LogsRetention is how long the log store keeps lines, as it was told
	// to: the engine cannot ask it. Only offered as the longest range the Logs
	// page lets somebody pick, and said beside it.
	LogsRetention time.Duration

	// Debug enables verbose logging.
	Debug bool
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	c := Config{
		Addr:                     env("YACHT_ADDR", ":8080"),
		DatabaseURL:              env("YACHT_DATABASE_URL", ""),
		Kubeconfig:               env("YACHT_KUBECONFIG", os.Getenv("KUBECONFIG")),
		KubeInCluster:            envBool("YACHT_KUBE_IN_CLUSTER", false),
		AuthToken:                env("YACHT_AUTH_TOKEN", ""),
		OwnerID:                  env("YACHT_OWNER_ID", "owner-local"),
		OwnerName:                env("YACHT_OWNER_NAME", "Local"),
		OwnerEmail:               strings.TrimSpace(env("YACHT_OWNER_EMAIL", "")),
		OpenSignup:               envBool("YACHT_OPEN_SIGNUP", false),
		RequestAccessURL:         strings.TrimSpace(env("YACHT_REQUEST_ACCESS_URL", "")),
		TurnstileSiteKey:         strings.TrimSpace(env("YACHT_TURNSTILE_SITE_KEY", "")),
		TurnstileSecret:          strings.TrimSpace(env("YACHT_TURNSTILE_SECRET", "")),
		SecretKey:                strings.TrimSpace(env("YACHT_SECRET_KEY", "")),
		SecretKeyPrevious:        envList("YACHT_SECRET_KEY_PREVIOUS"),
		AppDomain:                env("YACHT_APP_DOMAIN", ""),
		WildcardTLS:              envBool("YACHT_WILDCARD_TLS", false),
		CertIssuer:               strings.TrimSpace(env("YACHT_CERT_ISSUER", "")),
		DNSResolver:              env("YACHT_DNS_RESOLVER", ""),
		TCPProxyHost:             strings.ToLower(strings.TrimSuffix(strings.TrimSpace(env("YACHT_TCP_PROXY_HOST", "")), ".")),
		TCPProxyPorts:            strings.TrimSpace(env("YACHT_TCP_PROXY_PORTS", defaultTCPProxyPorts)),
		NetworkIsolation:         env("YACHT_NETWORK_ISOLATION", ""),
		EgressAllow:              env("YACHT_EGRESS_ALLOW", ""),
		ReservedDomains:          envList("YACHT_RESERVED_DOMAINS"),
		Operators:                envList("YACHT_OPERATORS"),
		BaseURL:                  strings.TrimRight(env("YACHT_BASE_URL", ""), "/"),
		BuildSourceURL:           strings.TrimRight(env("YACHT_BUILD_SOURCE_URL", ""), "/"),
		SMTPAddr:                 env("YACHT_SMTP_ADDR", ""),
		SMTPUsername:             env("YACHT_SMTP_USER", ""),
		SMTPPassword:             env("YACHT_SMTP_PASSWORD", ""),
		SMTPFrom:                 env("YACHT_SMTP_FROM", ""),
		ResendAPIKey:             env("YACHT_RESEND_API_KEY", ""),
		ResendFrom:               env("YACHT_RESEND_FROM", ""),
		CloudflareEmailToken:     env("YACHT_CLOUDFLARE_EMAIL_TOKEN", ""),
		CloudflareEmailAccountID: env("YACHT_CLOUDFLARE_EMAIL_ACCOUNT_ID", ""),
		CloudflareEmailFrom:      env("YACHT_CLOUDFLARE_EMAIL_FROM", ""),
		CloudflareEmailReplyTo:   env("YACHT_CLOUDFLARE_EMAIL_REPLY_TO", ""),
		MailCompany:              strings.TrimSpace(env("YACHT_MAIL_COMPANY", "")),
		MailWebsite:              strings.TrimSpace(env("YACHT_MAIL_WEBSITE", "")),
		MailReplyTo:              strings.TrimSpace(env("YACHT_MAIL_REPLY_TO", "")),
		SessionTTL:               envDuration("YACHT_SESSION_TTL", 720*time.Hour),
		MagicLinkTTL:             envDuration("YACHT_MAGIC_LINK_TTL", 15*time.Minute),
		ShutdownTimeout:          envDuration("YACHT_SHUTDOWN_TIMEOUT", 15*time.Second),
		MaxConcurrentBuilds:      envInt32("YACHT_MAX_CONCURRENT_BUILDS", 2),
		WakerAddr:                strings.TrimSpace(env("YACHT_WAKER_ADDR", "")),
		WakerListen:              strings.TrimSpace(env("YACHT_WAKER_LISTEN", "")),
		WakeTimeout:              envDuration("YACHT_WAKE_TIMEOUT", 60*time.Second),
		DefaultRegion:            strings.TrimSpace(env("YACHT_DEFAULT_REGION", "")),
		StageChanges:             envBool("YACHT_STAGE_CHANGES", true),
		GitHubAppID:              envInt64("YACHT_GITHUB_APP_ID"),
		GitHubAppSlug:            strings.TrimSpace(env("YACHT_GITHUB_APP_SLUG", "")),
		GitHubAppPrivateKey:      env("YACHT_GITHUB_APP_PRIVATE_KEY", ""),
		GitHubAppPrivateKeyFile:  strings.TrimSpace(env("YACHT_GITHUB_APP_PRIVATE_KEY_FILE", "")),
		GitHubAppWebhookSecret:   env("YACHT_GITHUB_APP_WEBHOOK_SECRET", ""),
		GitHubAppClientID:        strings.TrimSpace(env("YACHT_GITHUB_APP_CLIENT_ID", "")),
		GitHubAppClientSecret:    env("YACHT_GITHUB_APP_CLIENT_SECRET", ""),
		Debug:                    envBool("YACHT_DEBUG", false),
		BackupS3Endpoint:         strings.TrimSpace(env("YACHT_BACKUP_S3_ENDPOINT", "")),
		BackupS3Region:           strings.TrimSpace(env("YACHT_BACKUP_S3_REGION", "")),
		BackupS3Bucket:           strings.TrimSpace(env("YACHT_BACKUP_S3_BUCKET", "")),
		BackupS3Prefix:           strings.TrimSpace(env("YACHT_BACKUP_S3_PREFIX", "databases")),
		BackupS3AccessKey:        strings.TrimSpace(env("YACHT_BACKUP_S3_ACCESS_KEY", "")),
		BackupS3SecretKey:        strings.TrimSpace(env("YACHT_BACKUP_S3_SECRET_KEY", "")),
		BackupToolImage:          strings.TrimSpace(env("YACHT_BACKUP_TOOL_IMAGE", "")),
		BucketsAdminURL:          strings.TrimSpace(env("YACHT_BUCKETS_ADMIN_URL", "")),
		BucketsAdminToken:        strings.TrimSpace(env("YACHT_BUCKETS_ADMIN_TOKEN", "")),
		BucketsS3URL:             strings.TrimSpace(env("YACHT_BUCKETS_S3_URL", "")),
		BucketsEndpoint:          strings.TrimSpace(env("YACHT_BUCKETS_ENDPOINT", "")),
		BucketsRegion:            strings.TrimSpace(env("YACHT_BUCKETS_REGION", "us-east-1")),
		LogsURL:                  strings.TrimSpace(env("YACHT_LOGS_URL", "")),
		MetricsURL:               strings.TrimSpace(env("YACHT_METRICS_URL", "")),
		LogsRetention:            envDuration("YACHT_LOGS_RETENTION", 7*24*time.Hour),
	}

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	var errs []error

	if _, err := c.Isolation(); err != nil {
		errs = append(errs, fmt.Errorf("YACHT_NETWORK_ISOLATION or YACHT_EGRESS_ALLOW: %w", err))
	}

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("YACHT_DATABASE_URL is required"))
	}
	if err := orchestrator.ValidateRegion(c.DefaultRegion); err != nil {
		errs = append(errs, fmt.Errorf("YACHT_DEFAULT_REGION: %w", err))
	}
	if c.Addr == "" {
		errs = append(errs, errors.New("YACHT_ADDR must not be empty"))
	}
	if c.OwnerID == "" {
		errs = append(errs, errors.New("YACHT_OWNER_ID must not be empty"))
	}
	if c.MaxConcurrentBuilds < 1 || c.MaxConcurrentBuilds > 16 {
		errs = append(errs, errors.New(
			"YACHT_MAX_CONCURRENT_BUILDS must be between 1 and 16"))
	}
	errs = append(errs, c.tcpProxyFaults()...)
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
	// Written into an annotation cert-manager resolves by name. A value that
	// could never be an object name fails here rather than as an Ingress event
	// nobody reads.
	if c.CertIssuer != "" && (len(c.CertIssuer) > maxHostname || !issuerNameRE.MatchString(c.CertIssuer)) {
		errs = append(errs, fmt.Errorf(
			"YACHT_CERT_ISSUER %q is not a valid ClusterIssuer name", c.CertIssuer))
	}
	// TLS with nothing to apply it to leaves the operator believing apps are
	// served over TLS while nothing says otherwise.
	if c.WildcardTLS && c.AppDomain == "" {
		errs = append(errs, errors.New(
			"YACHT_WILDCARD_TLS requires YACHT_APP_DOMAIN — there would be no hostnames to serve"))
	}
	// Retired keys with nothing to be retired from. Nothing seals and nothing
	// opens, while the setting that exists to keep values readable sits there
	// looking as though it is doing that.
	if len(c.SecretKeyPrevious) > 0 && c.SecretKey == "" {
		errs = append(errs, errors.New(
			"YACHT_SECRET_KEY_PREVIOUS is set without YACHT_SECRET_KEY — a retired key opens "+
				"values but never seals them, so there would be no key in use"))
	}
	errs = append(errs, c.wakerFaults()...)
	if c.OwnerEmail != "" {
		if _, err := mail.ParseAddress(c.OwnerEmail); err != nil {
			errs = append(errs, errors.New("YACHT_OWNER_EMAIL must be an email address"))
		}
	}
	errs = append(errs, c.accountFaults()...)
	errs = append(errs, c.gitHubFaults()...)
	if _, err := c.BackupStore(); err != nil {
		errs = append(errs, err)
	}
	if _, err := c.BucketStore(); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, c.observeFaults()...)

	return errors.Join(errs...)
}

// defaultTCPProxyPorts is the low band of Kubernetes' default NodePort range,
// 30000-32767: the 86 ports it sets aside for Services that name their own,
// and hands out itself only once every other port is taken.
const defaultTCPProxyPorts = "30000-30085"

// TCPProxyRange is the first and last port a TCP proxy may be given.
//
// Empty is the default range, so a Config built by hand rather than loaded —
// an embedder's, or a test's — gets what an unset variable gets.
func (c Config) TCPProxyRange() (first, last int32, err error) {
	raw := cmp.Or(strings.TrimSpace(c.TCPProxyPorts), defaultTCPProxyPorts)
	lo, hi, ok := strings.Cut(raw, "-")
	if !ok {
		hi = lo
	}
	f, err1 := strconv.ParseInt(strings.TrimSpace(lo), 10, 32)
	l, err2 := strconv.ParseInt(strings.TrimSpace(hi), 10, 32)
	switch {
	case err1 != nil || err2 != nil:
		return 0, 0, fmt.Errorf("YACHT_TCP_PROXY_PORTS %q must be a port or a range such as %s", raw, defaultTCPProxyPorts)
	case f < 1 || l > 65535 || f > l:
		return 0, 0, fmt.Errorf("YACHT_TCP_PROXY_PORTS %q must be ports between 1 and 65535, lowest first", raw)
	}
	return int32(f), int32(l), nil
}

// tcpProxyFaults validates the TCP proxy's settings.
//
// Checked at startup, because a range the cluster refuses would otherwise be
// met one proxy at a time, as an apply that fails for an app somebody has just
// tried to reach their database through.
func (c Config) tcpProxyFaults() []error {
	var errs []error
	if _, _, err := c.TCPProxyRange(); err != nil {
		errs = append(errs, err)
	}
	if c.TCPProxyHost != "" {
		if fault := domainFault(c.TCPProxyHost); fault != "" {
			errs = append(errs, fmt.Errorf("YACHT_TCP_PROXY_HOST %s", fault))
		}
	}
	return errs
}

// wakerFaults validates the waker's settings. Each of these would otherwise be
// found by the first app to fall asleep and never wake.
func (c Config) wakerFaults() []error {
	var errs []error
	if c.WakerAddr != "" {
		if _, _, err := c.Waker(); err != nil {
			errs = append(errs, err)
		}
	} else if c.WakerListen != "" {
		errs = append(errs, errors.New(
			"YACHT_WAKER_LISTEN is set without YACHT_WAKER_ADDR — the cluster would not know where the waker is"))
	}
	if c.WakeTimeout < 5*time.Second || c.WakeTimeout > 10*time.Minute {
		errs = append(errs, errors.New("YACHT_WAKE_TIMEOUT must be between 5s and 10m"))
	}
	return errs
}

// Waker is WakerAddr's address and port. The address must be one the cluster
// can reach: loopback is the ingress controller's own pod to the ingress
// controller.
func (c Config) Waker() (string, int32, error) {
	host, portText, err := net.SplitHostPort(c.WakerAddr)
	if err != nil {
		return "", 0, fmt.Errorf("YACHT_WAKER_ADDR must be ip:port, such as 10.0.0.5:8090: %w", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.IsLoopback() || ip.IsUnspecified() {
		return "", 0, fmt.Errorf("YACHT_WAKER_ADDR %q must name an IP address the cluster's pods can reach — "+
			"this node's address, not a hostname or loopback", c.WakerAddr)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("YACHT_WAKER_ADDR %q has no usable port", c.WakerAddr)
	}
	return ip.String(), int32(port), nil
}

// WakerListenAddr is where the waker is served: WakerListen, or every
// interface on WakerAddr's port. Empty when there is no waker.
func (c Config) WakerListenAddr() string {
	if c.WakerListen != "" {
		return c.WakerListen
	}
	if _, port, err := c.Waker(); err == nil {
		return ":" + strconv.Itoa(int(port))
	}
	return ""
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
		if c.OpenSignup {
			return []error{errors.New(
				"YACHT_OPEN_SIGNUP is set without YACHT_BASE_URL — nobody can sign in, so nobody can sign up")}
		}
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
	var transports []string
	for _, t := range []struct{ name, value string }{
		{"YACHT_SMTP_ADDR", c.SMTPAddr},
		{"YACHT_RESEND_API_KEY", c.ResendAPIKey},
		{"YACHT_CLOUDFLARE_EMAIL_TOKEN", c.CloudflareEmailToken},
	} {
		if t.value != "" {
			transports = append(transports, t.name)
		}
	}
	if len(transports) > 1 {
		errs = append(errs, fmt.Errorf(
			"%s are all set — configure one mail transport", strings.Join(transports, " and ")))
	}
	if c.SMTPAddr != "" && c.SMTPFrom == "" {
		errs = append(errs, errors.New("YACHT_SMTP_FROM is required with YACHT_SMTP_ADDR"))
	}
	if c.ResendAPIKey != "" && c.ResendFrom == "" {
		errs = append(errs, errors.New("YACHT_RESEND_FROM is required with YACHT_RESEND_API_KEY"))
	}
	if c.CloudflareEmailToken != "" && c.CloudflareEmailAccountID == "" {
		errs = append(errs, errors.New(
			"YACHT_CLOUDFLARE_EMAIL_ACCOUNT_ID is required with YACHT_CLOUDFLARE_EMAIL_TOKEN"))
	}
	if c.CloudflareEmailToken != "" && c.CloudflareEmailFrom == "" {
		errs = append(errs, errors.New(
			"YACHT_CLOUDFLARE_EMAIL_FROM is required with YACHT_CLOUDFLARE_EMAIL_TOKEN"))
	}
	// A zero or negative lifetime issues credentials that are already expired,
	// which presents as sign-in silently never working.
	if c.SessionTTL <= 0 {
		errs = append(errs, errors.New("YACHT_SESSION_TTL must be positive"))
	}
	if c.MagicLinkTTL <= 0 {
		errs = append(errs, errors.New("YACHT_MAGIC_LINK_TTL must be positive"))
	}
	return append(errs, c.signupFaults()...)
}

// signupFaults refuses open sign-up on an install that would hand strangers
// more than an account.
func (c Config) signupFaults() []error {
	if c.RequestAccessURL != "" {
		if u, err := url.Parse(c.RequestAccessURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return []error{errors.New("YACHT_REQUEST_ACCESS_URL must be a full http(s) address, such as https://example.com/request-access")}
		}
	}
	// Half a Turnstile site draws a widget nothing checks, or checks a token
	// no widget handed out — and the second refuses every sign-in.
	if (c.TurnstileSiteKey == "") != (c.TurnstileSecret == "") {
		return []error{errors.New("YACHT_TURNSTILE_SITE_KEY and YACHT_TURNSTILE_SECRET go together — set both, or neither")}
	}
	if !c.OpenSignup {
		return nil
	}
	var errs []error
	// With no operators named, the owner of whichever team a request acts as
	// is the operator — and every stranger who signs up owns a team, so every
	// one of them would have the Admin area: every team, every node.
	if len(c.Operators) == 0 {
		errs = append(errs, errors.New(
			"YACHT_OPEN_SIGNUP requires YACHT_OPERATORS — without it everybody who signs up "+
				"owns a team, and so runs the install"))
	}
	// The team the install runs as goes to the configured owner only. With no
	// owner named it goes to nobody, and its apps with it.
	if c.OwnerEmail == "" {
		errs = append(errs, errors.New(
			"YACHT_OPEN_SIGNUP requires YACHT_OWNER_EMAIL — the install's own team is kept for that address"))
	}
	// Links in the log reach the operator, not the stranger asking for one.
	if c.MailTransport() == "log" {
		errs = append(errs, errors.New(
			"YACHT_OPEN_SIGNUP requires a mail transport — set YACHT_SMTP_ADDR, YACHT_RESEND_API_KEY or YACHT_CLOUDFLARE_EMAIL_TOKEN"))
	}
	return errs
}

// GitHubAppEnabled reports whether the install has a GitHub App: any of its
// settings given. Validation has refused a partial one by the time anything
// else asks.
func (c Config) GitHubAppEnabled() bool {
	return c.GitHubAppID != 0 || c.GitHubAppSlug != "" || c.GitHubAppPrivateKey != "" ||
		c.GitHubAppPrivateKeyFile != "" || c.GitHubAppWebhookSecret != "" ||
		c.GitHubAppClientID != "" || c.GitHubAppClientSecret != ""
}

// GitHubApp is the App as the github package takes it, with its key read and
// parsed.
func (c Config) GitHubApp() (github.Config, error) {
	pemText := strings.ReplaceAll(c.GitHubAppPrivateKey, `\n`, "\n")
	if c.GitHubAppPrivateKeyFile != "" {
		data, err := os.ReadFile(c.GitHubAppPrivateKeyFile)
		if err != nil {
			return github.Config{}, fmt.Errorf("YACHT_GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		}
		pemText = string(data)
	}
	key, err := github.ParsePrivateKey([]byte(pemText))
	if err != nil {
		return github.Config{}, fmt.Errorf("YACHT_GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	return github.Config{
		AppID:         c.GitHubAppID,
		Slug:          c.GitHubAppSlug,
		ClientID:      c.GitHubAppClientID,
		ClientSecret:  c.GitHubAppClientSecret,
		PrivateKey:    key,
		WebhookSecret: c.GitHubAppWebhookSecret,
	}, nil
}

// gitHubFaults refuses a GitHub App with any of its settings missing. Half an
// App fails at the first person to press Connect, or the first push, where
// nobody connects the failure to a variable left out.
func (c Config) gitHubFaults() []error {
	if !c.GitHubAppEnabled() {
		return nil
	}
	var errs []error
	var missing []string
	for _, v := range []struct {
		name string
		set  bool
	}{
		{"YACHT_GITHUB_APP_ID", c.GitHubAppID != 0},
		{"YACHT_GITHUB_APP_SLUG", c.GitHubAppSlug != ""},
		{"YACHT_GITHUB_APP_PRIVATE_KEY", c.GitHubAppPrivateKey != "" || c.GitHubAppPrivateKeyFile != ""},
		{"YACHT_GITHUB_APP_WEBHOOK_SECRET", c.GitHubAppWebhookSecret != ""},
		{"YACHT_GITHUB_APP_CLIENT_ID", c.GitHubAppClientID != ""},
		{"YACHT_GITHUB_APP_CLIENT_SECRET", c.GitHubAppClientSecret != ""},
	} {
		if !v.set {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("a GitHub App needs %s too", strings.Join(missing, ", ")))
	}
	if c.GitHubAppID < 0 {
		errs = append(errs, errors.New("YACHT_GITHUB_APP_ID must be the App's numeric id"))
	}
	if c.GitHubAppSlug != "" && !github.ValidSlug(c.GitHubAppSlug) {
		errs = append(errs, fmt.Errorf(
			"YACHT_GITHUB_APP_SLUG %q is not an App's slug — the name in github.com/apps/<slug>",
			c.GitHubAppSlug))
	}
	if c.GitHubAppPrivateKey != "" && c.GitHubAppPrivateKeyFile != "" {
		errs = append(errs, errors.New(
			"YACHT_GITHUB_APP_PRIVATE_KEY and YACHT_GITHUB_APP_PRIVATE_KEY_FILE are both set — give the key one way"))
	} else if c.GitHubAppPrivateKey != "" || c.GitHubAppPrivateKeyFile != "" {
		if _, err := c.GitHubApp(); err != nil {
			errs = append(errs, err)
		}
	}
	// A short secret signs deliveries anyone could forge by guessing it.
	if c.GitHubAppWebhookSecret != "" && len(c.GitHubAppWebhookSecret) < 16 {
		errs = append(errs, errors.New("YACHT_GITHUB_APP_WEBHOOK_SECRET must be at least 16 characters"))
	}
	if !c.AccountsEnabled() {
		errs = append(errs, errors.New(
			"a GitHub App requires YACHT_BASE_URL — GitHub sends people back to "+
				"<YACHT_BASE_URL>/github/callback, and connecting is done by a signed-in team"))
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

// envInt64 reads a number, and -1 for one that is set and is not a number, so
// validation can say so rather than the setting quietly reading as unset.
func envInt64(key string) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}

func envInt32(key string, fallback int32) int32 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
	if err != nil {
		return fallback
	}
	return int32(n)
}

// String redacts secrets so a Config can be logged safely.
// Isolation is the network isolation the settings ask for.
func (c Config) Isolation() (orchestrator.Isolation, error) {
	return orchestrator.ParseIsolation(c.NetworkIsolation, c.EgressAllow)
}

func (c Config) String() string {
	token := "unset"
	if c.AuthToken != "" {
		token = "set"
	}
	isolation := "invalid"
	if iso, err := c.Isolation(); err == nil {
		isolation = string(iso.Mode)
	}
	return fmt.Sprintf(
		"addr=%s db=%s kubeconfig=%s in_cluster=%t auth_token=%s owner=%s "+
			"accounts=%t open_signup=%t turnstile=%t mail=%s waker=%s isolation=%s github_app=%s logs=%s metrics=%s debug=%t",
		c.Addr, redactDSN(c.DatabaseURL), orNone(c.Kubeconfig),
		c.KubeInCluster, token, c.OwnerID,
		c.AccountsEnabled(), c.OpenSignup, c.TurnstileSiteKey != "", c.MailTransport(), orOff(c.WakerAddr), isolation,
		orOff(c.GitHubAppSlug), orOff(c.LogsURL), orOff(c.MetricsURL), c.Debug,
	)
}

// BackupStore is the bucket database backups go to, or nil when none is
// configured.
//
// Half a bucket is refused at startup: an endpoint without keys would leave
// every backup on a volume while the operator believed they were off-site,
// which is the failure nobody discovers until they need one.
func (c Config) BackupStore() (*backup.S3, error) {
	set := map[string]string{
		"YACHT_BACKUP_S3_ENDPOINT":   c.BackupS3Endpoint,
		"YACHT_BACKUP_S3_BUCKET":     c.BackupS3Bucket,
		"YACHT_BACKUP_S3_ACCESS_KEY": c.BackupS3AccessKey,
		"YACHT_BACKUP_S3_SECRET_KEY": c.BackupS3SecretKey,
	}
	var missing []string
	given := 0
	for _, k := range []string{"YACHT_BACKUP_S3_ENDPOINT", "YACHT_BACKUP_S3_BUCKET",
		"YACHT_BACKUP_S3_ACCESS_KEY", "YACHT_BACKUP_S3_SECRET_KEY"} {
		if set[k] == "" {
			missing = append(missing, k)
		} else {
			given++
		}
	}
	if given == 0 {
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("database backups to S3 need %s as well", strings.Join(missing, ", "))
	}
	s3, err := backup.NewS3(c.BackupS3Endpoint, c.BackupS3Region, c.BackupS3Bucket,
		c.BackupS3Prefix, c.BackupS3AccessKey, c.BackupS3SecretKey)
	if err != nil {
		return nil, fmt.Errorf("YACHT_BACKUP_S3_*: %w", err)
	}
	return s3, nil
}

// BucketStore is the Garage customers' buckets are made on, or nil when the
// install has none. Half of one is refused at startup, as half a backup
// bucket is: Add › Bucket would be offered and fail.
func (c Config) BucketStore() (*bucket.Garage, error) {
	if c.BucketsAdminURL == "" && c.BucketsAdminToken == "" && c.BucketsEndpoint == "" {
		return nil, nil
	}
	if c.BucketsAdminURL == "" || c.BucketsAdminToken == "" || c.BucketsEndpoint == "" {
		return nil, errors.New("buckets need YACHT_BUCKETS_ADMIN_URL, YACHT_BUCKETS_ADMIN_TOKEN and YACHT_BUCKETS_ENDPOINT together")
	}
	g := &bucket.Garage{
		AdminURL: c.BucketsAdminURL, Token: c.BucketsAdminToken,
		// The engine reaching the S3 API the way apps do, when it is not
		// told of a shorter way in the cluster.
		S3URL:    cmp.Or(c.BucketsS3URL, c.BucketsEndpoint),
		Endpoint: c.BucketsEndpoint, Region: c.BucketsRegion,
	}
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("YACHT_BUCKETS_*: %w", err)
	}
	return g, nil
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
	case c.CloudflareEmailToken != "":
		return "cloudflare"
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

func orOff(s string) string {
	if s == "" {
		return "off"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
