// Package registry answers one question: where do images this install builds
// go, and how does the cluster get them back.
//
// Its own package rather than another setting on the cluster Joiner. That type
// already holds two unrelated install-wide settings, and a third would make it
// the place install settings go rather than a thing with a job. The seam also
// earns its keep: the builder needs push credentials, the orchestrator needs
// pull credentials, and neither should reach into a database to find them.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrNotConfigured means nobody has stored a registry.
//
// Distinguished from a failure because it is the normal state of a fresh
// install, and because it is the difference between "building is broken" and
// "building is not set up yet" — which are different things to tell somebody.
var ErrNotConfigured = errors.New("registry: no image registry is configured")

// ErrNoSecretKey means a password was offered with no key to seal it.
//
// Refused rather than stored readable, for the reason the join token is: a
// credential protected in name only is worse than one nobody stored, because
// nothing about it looks wrong.
var ErrNoSecretKey = errors.New(
	"registry: no YACHT_SECRET_KEY is configured, so a registry password cannot be stored safely")

// Settings is where images go, without the password.
//
// The password is deliberately absent. Everything that renders a page takes
// this type, so there is no path by which a template can print the credential
// even by accident.
type Settings struct {
	Host       string
	Repository string
	Username   string

	// Insecure means the registry is served over plain HTTP.
	//
	// Yacht can only act on half of this. It can tell the builder to push over
	// HTTP; it cannot tell the cluster's container runtime to pull that way,
	// because that is node configuration and Yacht does not touch nodes. The
	// page says so rather than leaving somebody to discover it as
	// ImagePullBackOff.
	Insecure bool

	UpdatedAt string
}

// Configured reports whether images have somewhere to go.
func (s Settings) Configured() bool { return s.Host != "" }

// Credentials is what actually authenticates to the registry.
//
// Separate from Settings and returned by its own method, so that reading the
// password is something a caller asks for explicitly rather than something it
// receives whenever it asks where images go.
type Credentials struct {
	Host     string
	Username string
	Password string

	// Insecure means push and pull go over plain HTTP.
	Insecure bool
}

// Store reads and writes the install's registry.
type Store struct {
	q      *dbgen.Queries
	keeper *secret.Keeper
	log    *slog.Logger
}

// New returns a Store. keeper may be nil, in which case storing a password is
// refused rather than done unsafely.
func New(pool *pgxpool.Pool, keeper *secret.Keeper, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{q: dbgen.New(pool), keeper: keeper, log: log}
}

// CanStore reports whether a password can be sealed.
//
// Asked rather than discovered, so a page can say a credential cannot be kept
// safely before offering a box to type one into.
func (s *Store) CanStore() bool { return s.keeper.Configured() }

// Settings returns where images go. An unconfigured install is not an error.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	row, err := s.q.GetPlatformRegistry(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, nil
		}
		return Settings{}, fmt.Errorf("registry: read settings: %w", err)
	}
	return Settings{
		Host:       row.Host,
		Repository: row.Repository,
		Username:   row.Username,
		Insecure:   row.Insecure,
		UpdatedAt:  row.UpdatedAt.Format("2006-01-02 15:04"),
	}, nil
}

// Credentials returns what authenticates to the registry.
func (s *Store) Credentials(ctx context.Context) (Credentials, error) {
	row, err := s.q.GetPlatformRegistry(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Credentials{}, ErrNotConfigured
		}
		return Credentials{}, fmt.Errorf("registry: read credentials: %w", err)
	}
	if !s.keeper.Configured() {
		return Credentials{}, ErrNoSecretKey
	}
	password, err := s.keeper.Open(row.PasswordSealed)
	if err != nil {
		return Credentials{}, fmt.Errorf("registry: unseal password: %w", err)
	}
	return Credentials{
		Host: row.Host, Username: row.Username, Password: password,
		Insecure: row.Insecure,
	}, nil
}

// Set stores where images go.
func (s *Store) Set(
	ctx context.Context, by uuid.UUID, host, repository, username, password string, insecure bool,
) error {
	host = strings.TrimSpace(strings.ToLower(host))
	repository = strings.Trim(strings.TrimSpace(strings.ToLower(repository)), "/")
	username = strings.TrimSpace(username)

	if err := ValidateHost(host); err != nil {
		return err
	}
	if err := ValidateRepository(repository); err != nil {
		return err
	}
	if username == "" || password == "" {
		return errors.New("registry: a username and password are required — " +
			"a registry that needs no credentials still needs an account to push under")
	}
	if !s.keeper.Configured() {
		return ErrNoSecretKey
	}

	sealed, err := s.keeper.Seal(password)
	if err != nil {
		return fmt.Errorf("registry: seal password: %w", err)
	}

	if _, err := s.q.SetPlatformRegistry(ctx, dbgen.SetPlatformRegistryParams{
		Host: host, Repository: repository, Username: username,
		PasswordSealed: sealed, Insecure: insecure, UpdatedBy: pgUUID(by),
	}); err != nil {
		return fmt.Errorf("registry: store settings: %w", err)
	}
	// The password is not logged and neither is its length. What changed and
	// who changed it is the useful record; anything about the value itself is
	// a credential sitting in a log file.
	s.log.Info("registry configured",
		slog.String("host", host), slog.String("repository", repository),
		slog.String("username", username), slog.Bool("insecure", insecure))
	return nil
}

// Clear removes the registry.
func (s *Store) Clear(ctx context.Context) error {
	if err := s.q.ClearPlatformRegistry(ctx); err != nil {
		return fmt.Errorf("registry: clear settings: %w", err)
	}
	s.log.Info("registry cleared")
	return nil
}

// ImageRef is the full name an app's build is pushed to and pulled from.
//
// The path is derived from the owner and the app rather than chosen, because
// one registry account holds every team's images: a person picking their own
// path could pick another team's, and pushing over somebody else's tag is a
// way to have the cluster run your image under their name.
func (s Settings) ImageRef(ownerID, app, revision string) string {
	parts := []string{s.Host}
	if s.Repository != "" {
		parts = append(parts, s.Repository)
	}
	parts = append(parts, pathSegment(ownerID)+"-"+pathSegment(app))
	return strings.Join(parts, "/") + ":" + pathSegment(revision)
}

// DockerConfigJSON is the contents of a kubernetes.io/dockerconfigjson secret.
//
// Built here rather than in the Kubernetes layer because the shape belongs to
// the registry, and because keeping it here means the credential is turned
// into its wire form in the one package that is allowed to see it.
func (c Credentials) DockerConfigJSON() ([]byte, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
	cfg := map[string]any{
		"auths": map[string]any{
			c.Host: map[string]string{
				"username": c.Username,
				"password": c.Password,
				"auth":     auth,
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("registry: encode docker config: %w", err)
	}
	return b, nil
}

// A registry host: a hostname with optional port. No scheme and no path,
// because a container reference has neither and accepting one here produces an
// image name that fails at pull time rather than at the form that set it.
// Written label by label rather than as one character class: [-a-z0-9.]*
// accepts "ghcr..io", which is not a hostname and fails at pull time.
var hostRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(:[0-9]{1,5})?$`)

// ValidateHost checks the registry host.
func ValidateHost(host string) error {
	if host == "" {
		return errors.New("registry: a host is required, e.g. ghcr.io")
	}
	if strings.Contains(host, "://") {
		return errors.New("registry: leave the scheme off — " +
			"an image name starts at the host, as in ghcr.io/you/app")
	}
	if strings.Contains(host, "/") {
		return errors.New("registry: the host has no path — " +
			"put the organisation or user in the repository field instead")
	}
	if !hostRE.MatchString(host) {
		return fmt.Errorf("registry: %q is not a registry host", host)
	}
	return nil
}

// A repository path: slash-separated segments of the shape a registry accepts.
var repoRE = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)

// ValidateRepository checks the path images are pushed under. Empty is valid.
func ValidateRepository(repo string) error {
	if repo == "" {
		return nil
	}
	if !repoRE.MatchString(repo) {
		return fmt.Errorf("registry: %q is not a repository path — "+
			"lowercase letters, digits, and . _ - between them, separated by /", repo)
	}
	return nil
}

// pathSegment reduces a value to what a registry accepts in a path.
//
// Applied to the owner and app rather than validated, because these are not
// somebody's input at this point: they are identifiers this system already
// holds, and a rejection here would be a failure with nothing a person could
// do about it.
// A run of anything unacceptable collapses to a single dash rather than one
// dash each. Replacing character for character keeps "../" as "..-", and a
// registry path segment cannot hold two separators in a row — so the ref would
// be built, stored, and refused at push time.
func pathSegment(s string) string {
	var b strings.Builder
	dashed := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dashed = false
			continue
		}
		if !dashed {
			b.WriteByte('-')
			dashed = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "app"
	}
	return out
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// ImageFor returns the reference a build should push to.
//
// Reads the settings on every call rather than caching them. A registry that
// changed between two deploys should take effect on the second, and a cache
// here would mean builds pushing to a registry nobody configured any more.
func (s *Store) ImageFor(ctx context.Context, ownerID, app, revision string) (string, error) {
	set, err := s.Settings(ctx)
	if err != nil {
		return "", err
	}
	if !set.Configured() {
		return "", ErrNotConfigured
	}
	return set.ImageRef(ownerID, app, revision), nil
}

// Configured reports whether there is a registry to push to.
//
// Swallows the read error deliberately: this answers a question a picker asks
// while rendering, and "the database is unreachable" is not something a source
// list can act on. An install that cannot read its settings shows building as
// unavailable, which is what it is.
func (s *Store) Configured(ctx context.Context) bool {
	set, err := s.Settings(ctx)
	return err == nil && set.Configured()
}

// DockerConfig returns the credential in the form Kubernetes and BuildKit read.
func (s *Store) DockerConfig(ctx context.Context) ([]byte, error) {
	creds, err := s.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	return creds.DockerConfigJSON()
}

// Insecure reports whether the registry is served over plain HTTP.
//
// Swallows the read error like Configured does, and for the same reason: this
// answers a question asked while assembling a build, and the safe answer to
// "should this connection expect TLS" is yes.
func (s *Store) Insecure(ctx context.Context) bool {
	set, err := s.Settings(ctx)
	return err == nil && set.Insecure
}
