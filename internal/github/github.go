// Package github is the install's GitHub App, as GitHub's API sees it: the
// App's own JWT, installation tokens, the user authorization that proves a
// person may use an installation, and the webhook's signature.
//
// It holds no state about teams or apps. Which team may use which
// installation is the app service's, in the database; this package answers
// what GitHub says, and keeps only the installation tokens it minted, in
// memory, until shortly before they expire.
//
// Every request carries its credential in the Authorization header and never
// in a URL, so no error this package returns — a transport error quotes the
// URL — can contain a token.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Where GitHub is. Fields on Config rather than constants only so a test can
// point them at a fake.
const (
	DefaultAPIURL = "https://api.github.com"
	DefaultWebURL = "https://github.com"
)

// Refusals a caller tells apart.
var (
	// ErrInstallationGone is an installation GitHub no longer has: the App
	// was uninstalled from the account.
	ErrInstallationGone = errors.New("github: the GitHub App is no longer installed there")

	// ErrInstallationSuspended is an installation its account suspended.
	// It mints no tokens until it is unsuspended.
	ErrInstallationSuspended = errors.New("github: the GitHub App's installation is suspended")

	// ErrRepoNotAccessible is a repository the installation does not reach:
	// never chosen when it was installed, taken away since, or deleted.
	ErrRepoNotAccessible = errors.New("github: the GitHub App cannot reach that repository")

	// ErrNotAuthorized is a user authorization GitHub refused: a code that
	// was spent, expired or never issued.
	ErrNotAuthorized = errors.New("github: GitHub did not accept the authorization")
)

// Config is the App as registered on GitHub.
type Config struct {
	// AppID is the App's numeric id, the JWT's issuer.
	AppID int64

	// Slug is the App's name in its public URL, github.com/apps/<slug>.
	Slug string

	// ClientID and ClientSecret are the App's OAuth credentials, for the
	// user authorization during installation.
	ClientID     string
	ClientSecret string

	// PrivateKey signs the App's JWT.
	PrivateKey *rsa.PrivateKey

	// WebhookSecret signs every delivery to the App's webhook.
	WebhookSecret string

	// APIURL and WebURL default to GitHub's own.
	APIURL string
	WebURL string

	// HTTP defaults to a client with a timeout. Now defaults to time.Now.
	HTTP *http.Client
	Now  func() time.Time
}

// App is a configured GitHub App.
type App struct {
	cfg Config

	mu     sync.Mutex
	tokens map[tokenKey]Token
}

// tokenKey is what one cached token covers: an installation, and one of its
// repositories or — RepoID zero — the installation's list of them.
type tokenKey struct {
	installation int64
	repo         int64
}

// New checks a configuration and returns the App.
func New(cfg Config) (*App, error) {
	var errs []error
	if cfg.AppID <= 0 {
		errs = append(errs, errors.New("github: an App id is required"))
	}
	if !slugRE.MatchString(cfg.Slug) {
		errs = append(errs, fmt.Errorf("github: %q is not an App's slug", cfg.Slug))
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		errs = append(errs, errors.New("github: the App's client id and secret are required"))
	}
	if cfg.PrivateKey == nil {
		errs = append(errs, errors.New("github: the App's private key is required"))
	}
	if cfg.WebhookSecret == "" {
		errs = append(errs, errors.New("github: the App's webhook secret is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}
	if cfg.WebURL == "" {
		cfg.WebURL = DefaultWebURL
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	cfg.WebURL = strings.TrimRight(cfg.WebURL, "/")
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &App{cfg: cfg, tokens: map[tokenKey]Token{}}, nil
}

// An App's slug: what GitHub makes of its name for its URL.
var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

// ValidSlug reports whether s could be an App's slug, for configuration to
// refuse one at startup rather than as a broken install link.
func ValidSlug(s string) bool { return slugRE.MatchString(s) }

// ParsePrivateKey reads the PEM GitHub hands out — PKCS#1, "BEGIN RSA
// PRIVATE KEY" — or the same key converted to PKCS#8.
func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("github: the private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github: the private key is neither PKCS#1 nor PKCS#8")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: the private key is not an RSA key")
	}
	return rk, nil
}

// Slug is the App's name in its URL.
func (a *App) Slug() string { return a.cfg.Slug }

// InstallURL is where a person installs the App on an account of theirs, or
// changes which repositories an installation reaches. GitHub sends them back
// to the App's callback URL with state as it was given.
func (a *App) InstallURL(state string) string {
	return a.cfg.WebURL + "/apps/" + url.PathEscape(a.cfg.Slug) +
		"/installations/new?state=" + url.QueryEscape(state)
}

// AuthorizeURL is the user authorization alone, for a person whose account
// already has the App installed: GitHub sends them back to the callback URL
// with a code and state and no installation.
func (a *App) AuthorizeURL(state string) string {
	return a.cfg.WebURL + "/login/oauth/authorize?client_id=" +
		url.QueryEscape(a.cfg.ClientID) + "&state=" + url.QueryEscape(state)
}

// JWT is the App's own credential, which GitHub accepts only for asking about
// the App and minting installation tokens.
//
// Issued a minute in the past, as GitHub recommends, so a clock a little
// ahead of GitHub's does not have it refused as not yet valid; and for nine
// minutes from then, inside GitHub's ten.
func (a *App) JWT() (string, error) {
	now := a.cfg.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.cfg.AppID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.cfg.PrivateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github: sign the App's JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Account is the GitHub user or organisation an installation is on.
type Account struct {
	Login string `json:"login"`
	// Type is "User" or "Organization".
	Type string `json:"type"`
}

// Installation is one installation of the App.
type Installation struct {
	ID          int64      `json:"id"`
	AppID       int64      `json:"app_id"`
	Account     Account    `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

// Installation asks GitHub about one installation, as the App.
func (a *App) Installation(ctx context.Context, id int64) (Installation, error) {
	jwt, err := a.JWT()
	if err != nil {
		return Installation{}, err
	}
	var out Installation
	_, err = a.do(ctx, http.MethodGet,
		a.cfg.APIURL+"/app/installations/"+strconv.FormatInt(id, 10), "Bearer "+jwt, nil, &out)
	if errors.Is(err, errNotFound) {
		return Installation{}, ErrInstallationGone
	}
	return out, err
}

// Repository is a repository an installation reaches.
type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
}

// Token is an installation token: a bearer credential for one hour.
type Token struct {
	Value     string
	ExpiresAt time.Time

	// Repositories is what the token was scoped to, when it was.
	Repositories []Repository
}

// tokenSlack is how long before a token expires it stops being reused. A
// build can take thirty minutes, and its clone is at the start, but a
// picker's token handed out a minute before expiry would fail half way down
// a paginated list.
const tokenSlack = 10 * time.Minute

// RepoToken is a token that can read one repository's contents and nothing
// else: scoped to its id, which survives a rename, with contents: read.
//
// Cached until shortly before it expires, in this process only. It is never
// stored: a build needs one for its clone and gets one then.
func (a *App) RepoToken(ctx context.Context, installationID, repoID int64) (Token, error) {
	if repoID <= 0 {
		return Token{}, ErrRepoNotAccessible
	}
	return a.token(ctx, tokenKey{installationID, repoID}, map[string]any{
		"repository_ids": []int64{repoID},
		"permissions":    map[string]string{"contents": "read", "metadata": "read"},
	})
}

// Repository is what GitHub says about one repository the installation
// reaches, read off the scoped token's own answer.
func (a *App) Repository(ctx context.Context, installationID, repoID int64) (Repository, error) {
	tok, err := a.RepoToken(ctx, installationID, repoID)
	if err != nil {
		return Repository{}, err
	}
	for _, r := range tok.Repositories {
		if r.ID == repoID {
			return r, nil
		}
	}
	return Repository{}, ErrRepoNotAccessible
}

// listToken lists what an installation reaches, and reads nothing in it.
func (a *App) listToken(ctx context.Context, installationID int64) (Token, error) {
	return a.token(ctx, tokenKey{installationID, 0}, map[string]any{
		"permissions": map[string]string{"metadata": "read"},
	})
}

func (a *App) token(ctx context.Context, key tokenKey, body map[string]any) (Token, error) {
	now := a.cfg.Now()
	a.mu.Lock()
	cached, ok := a.tokens[key]
	a.mu.Unlock()
	if ok && cached.ExpiresAt.Sub(now) > tokenSlack {
		return cached, nil
	}

	jwt, err := a.JWT()
	if err != nil {
		return Token{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Token{}, err
	}
	var out struct {
		Token        string       `json:"token"`
		ExpiresAt    time.Time    `json:"expires_at"`
		Repositories []Repository `json:"repositories"`
	}
	_, err = a.do(ctx, http.MethodPost,
		a.cfg.APIURL+"/app/installations/"+strconv.FormatInt(key.installation, 10)+"/access_tokens",
		"Bearer "+jwt, payload, &out)
	if err != nil {
		var se *statusError
		switch {
		case errors.Is(err, errNotFound):
			// The installation is gone. A repository id it does not reach is
			// a 422, below.
			return Token{}, ErrInstallationGone
		case errors.As(err, &se) && se.code == http.StatusForbidden &&
			strings.Contains(strings.ToLower(se.message), "suspend"):
			return Token{}, ErrInstallationSuspended
		case errors.As(err, &se) && se.code == http.StatusUnprocessableEntity:
			return Token{}, ErrRepoNotAccessible
		}
		return Token{}, err
	}
	if out.Token == "" {
		return Token{}, errors.New("github: GitHub returned no installation token")
	}
	tok := Token{Value: out.Token, ExpiresAt: out.ExpiresAt, Repositories: out.Repositories}
	a.mu.Lock()
	a.tokens[key] = tok
	a.mu.Unlock()
	return tok, nil
}

// Forget drops an installation's cached tokens, when GitHub says it was
// suspended or removed: a cached token would otherwise go on working for up
// to an hour after access was taken away.
func (a *App) Forget(installationID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.tokens {
		if k.installation == installationID {
			delete(a.tokens, k)
		}
	}
}

// maxPages bounds a paginated read. A hundred to a page, so this is the
// first ten thousand repositories or branches: more than a picker shows.
const maxPages = 100

// Repositories lists every repository an installation reaches.
func (a *App) Repositories(ctx context.Context, installationID int64) ([]Repository, error) {
	tok, err := a.listToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var all []Repository
	next := a.cfg.APIURL + "/installation/repositories?per_page=100"
	for page := 0; next != "" && page < maxPages; page++ {
		var out struct {
			Repositories []Repository `json:"repositories"`
		}
		link, err := a.do(ctx, http.MethodGet, next, "token "+tok.Value, nil, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Repositories...)
		next = a.sameHost(nextLink(link))
	}
	return all, nil
}

// Branches lists a repository's branches, read with its own scoped token.
func (a *App) Branches(ctx context.Context, installationID, repoID int64) ([]string, error) {
	tok, err := a.RepoToken(ctx, installationID, repoID)
	if err != nil {
		return nil, err
	}
	var repo Repository
	for _, r := range tok.Repositories {
		if r.ID == repoID {
			repo = r
		}
	}
	if repo.FullName == "" {
		return nil, ErrRepoNotAccessible
	}
	var names []string
	next := a.cfg.APIURL + "/repos/" + repo.FullName + "/branches?per_page=100"
	for page := 0; next != "" && page < maxPages; page++ {
		var out []struct {
			Name string `json:"name"`
		}
		link, err := a.do(ctx, http.MethodGet, next, "token "+tok.Value, nil, &out)
		if errors.Is(err, errNotFound) {
			return nil, ErrRepoNotAccessible
		}
		if err != nil {
			return nil, err
		}
		for _, b := range out {
			names = append(names, b.Name)
		}
		next = a.sameHost(nextLink(link))
	}
	return names, nil
}

// sameHost keeps a pagination link only if it points back at the API this
// was configured with. The token goes wherever the link says, so a link to
// anywhere else is a page not worth reading.
func (a *App) sameHost(link string) string {
	if link == "" {
		return ""
	}
	want, err1 := url.Parse(a.cfg.APIURL)
	got, err2 := url.Parse(link)
	if err1 != nil || err2 != nil || !strings.EqualFold(want.Scheme, got.Scheme) ||
		!strings.EqualFold(want.Host, got.Host) {
		return ""
	}
	return link
}

// linkNextRE finds the rel="next" target in a Link header.
var linkNextRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		if m := linkNextRE.FindStringSubmatch(part); m != nil {
			return m[1]
		}
	}
	return ""
}

// errNotFound is a 404, which each caller says in its own terms.
var errNotFound = errors.New("github: not found")

// statusError is any other answer that was not a success: the status, and
// GitHub's own message, which never quotes a credential.
type statusError struct {
	code    int
	message string
}

func (e *statusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("github: GitHub answered %d", e.code)
	}
	return fmt.Sprintf("github: GitHub answered %d: %s", e.code, e.message)
}

// maxResponse caps what is read of one answer. A page of a hundred
// repositories is a few hundred kilobytes.
const maxResponse = 8 << 20

// do makes one request and decodes its JSON, returning the Link header.
func (a *App) do(
	ctx context.Context, method, endpoint, auth string, body []byte, out any,
) (string, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return "", fmt.Errorf("github: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.cfg.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return "", fmt.Errorf("github: read GitHub's answer: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &msg)
		return "", &statusError{code: resp.StatusCode, message: shorten(msg.Message)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return "", fmt.Errorf("github: read GitHub's answer: %w", err)
		}
	}
	return resp.Header.Get("Link"), nil
}

func shorten(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
