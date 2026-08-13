package registry

// Manifest resolution: given a tag, what digest is behind it right now.
//
// Here rather than in the orchestrator because a digest is a fact about the
// registry, and this is the package already allowed to hold the credential
// that asks. Written against the distribution API directly, for the reason the
// Resend mailer is: it is one GET and one token exchange, and a client library
// for that is a dependency to keep updated forever.
//
// The point of the capability is that a pinned digest is the only image
// identity that cannot change underneath a deploy. A tag can be pushed over;
// a digest cannot. Everything that has to reproduce an earlier runnable state
// — releases, rollback, recovery after a crash between push and record — needs
// this one question answered, and answering it in one place means the happy
// path and the recovery path run the same query.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// ErrManifestNotFound means the registry answered, authoritatively, that the
// image is not there.
//
// A separate answer from a failure because the two lead to opposite decisions.
// An image that is genuinely absent will not appear by waiting, so the deploy
// that wanted it has to fail and say to build again. Anything short of an
// authoritative answer says nothing about whether the image exists, so failing
// on it would discard an image that is sitting in the registry.
//
// Two responses produce it, and nothing else does: a 404, and a registry that
// names the image unknown — MANIFEST_UNKNOWN or NAME_UNKNOWN — once Yacht has
// authenticated. The second exists because some registries answer a missing
// repository with 401 rather than 404, and after the credential has been
// presented "unknown" is about the image rather than about the caller.
//
// Everything else is deliberately excluded, and the exclusions are the point.
// A registry that says "unknown image" before Yacht has authenticated at all is
// describing what an anonymous caller can see, not what is there. A registry
// that answers something unclassifiable is describing nothing. Both are "we do
// not know yet", because the cost of the two mistakes is not symmetric: a false
// "unreachable" retries, and a false "not found" throws away a successfully
// pushed image.
var ErrManifestNotFound = errors.New("registry: the image is not in the registry")

// ErrUnreachable means nothing was learned about the image.
//
// It covers the transport failing, the request being cancelled, the registry
// refusing service for now (429), the registry failing on its own account
// (5xx), and the registry answering something that is not an answer — an
// unexpected status, a body that is not a manifest, a digest that contradicts
// its own content. All of them are the same answer to the only question a
// caller has here: no, not yet, and nothing has been ruled out.
//
// The cause is kept in the chain, so errors.Is finds context.Canceled and
// context.DeadlineExceeded through it as well as this.
var ErrUnreachable = errors.New("registry: the registry could not be asked about the image")

// ErrCredentialsRejected means authentication could not be completed.
//
// Either the registry refused the credential, or its challenge could not be
// answered safely — a token endpoint Yacht will not send a request to is the
// same practical outcome as one that refused it. Distinguished from both of the
// above because it is neither transient nor a statement about the image: it is
// a configuration fault, and the person who can fix it is the one who set the
// registry up.
var ErrCredentialsRejected = errors.New("registry: the registry could not be authenticated to")

// ResolveDigest returns the digest behind an image reference.
//
// A reference that already names a digest is returned unchanged without a
// request, because it is already the answer and a lookup could only disagree
// with itself.
//
// The install's credential is read per call rather than held, the way
// DockerConfig reads it, so it is unsealed only when something is about to ask
// and a registry password changed between two deploys takes effect on the
// second. It is sent only to the host this install configured: a reference to
// somebody else's registry is resolved anonymously rather than by presenting
// this install's password to a host that never asked for it.
func (s *Store) ResolveDigest(ctx context.Context, ref string) (string, error) {
	parsed, err := parseReference(ref)
	if err != nil {
		return "", err
	}
	if parsed.digest != "" {
		return parsed.digest, nil
	}
	client, err := s.clientForManifest(ctx, parsed)
	if err != nil {
		return "", err
	}
	return client.resolve(ctx, parsed)
}

// CheckDigest proves that a pinned image is still present. Unlike
// ResolveDigest, it deliberately asks the registry: rollback needs an
// availability answer before it may create an operation or touch the cluster.
func (s *Store) CheckDigest(ctx context.Context, ref string) error {
	parsed, err := parseReference(ref)
	if err != nil {
		return err
	}
	if parsed.digest == "" {
		return errors.New("registry: availability checks require a digest reference")
	}
	client, err := s.clientForManifest(ctx, parsed)
	if err != nil {
		return err
	}
	_, err = client.resolve(ctx, parsed)
	return err
}

func (s *Store) clientForManifest(
	ctx context.Context, parsed reference,
) (manifestClient, error) {

	set, err := s.Settings(ctx)
	if err != nil {
		return manifestClient{}, err
	}

	client := manifestClient{http: s.http}
	if ownHost(set, parsed) {
		// Plain HTTP only for the host the install already declared insecure.
		// Anything else is asked over TLS regardless, because a reference is
		// not a place to opt out of it.
		client.insecure = set.Insecure

		creds, err := s.Credentials(ctx)
		switch {
		case err == nil:
			client.creds = &creds
		case errors.Is(err, ErrNotConfigured), errors.Is(err, ErrNoSecretKey):
			// No credential to send. A public registry still answers, and
			// failing here would make an install without a secret key unable
			// to resolve images it never needed a password for.
		default:
			return manifestClient{}, err
		}
	}
	return client, nil
}

// ownHost reports whether a reference points at the registry this install
// configured, and so whether this install's credential belongs on the wire.
func ownHost(set Settings, ref reference) bool {
	return set.Configured() && normalizeHost(set.Host) == ref.host
}

// manifestClient asks one registry one question.
//
// Credentials and the insecure flag are passed in rather than read here, so
// the wire behaviour can be tested against a fake registry without a database
// standing behind it.
type manifestClient struct {
	http     *http.Client
	creds    *Credentials
	insecure bool
}

// manifestTypes is what Yacht can be handed back, and the only thing it will
// hash into a digest.
//
// The index types come first because a multi-architecture image should resolve
// to the digest of its index rather than of one platform's manifest: pinning a
// single platform would quietly turn a portable release into an amd64-only one
// on a cluster that has both.
//
// One list for both the Accept header and the check on the way back in. A
// registry that ignores Accept, a proxy that returns a sign-in page, a
// schema1 manifest from something very old — none of them is a manifest, and
// hashing whatever arrived would pin a release to the digest of an HTML page.
var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

var manifestAccept = strings.Join(manifestTypes, ", ")

// isManifestType reports whether a Content-Type is one Yacht will hash.
//
// Parsed rather than compared, because the header carries parameters —
// registries send charset, and a prefix match on the raw header would accept
// "application/vnd.oci.image.index.v1+json.evil".
func isManifestType(header string) bool {
	parsed, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	parsed = strings.ToLower(strings.TrimSpace(parsed))
	for _, want := range manifestTypes {
		if parsed == want {
			return true
		}
	}
	return false
}

// manifestLimit caps what will be read as a manifest.
//
// A manifest is kilobytes. The cap is here because the digest is computed over
// whatever arrives, and "whatever arrives" from a host Yacht does not control
// is not a thing to read without a bound.
const manifestLimit = 4 << 20

// resolve asks the registry for a manifest and returns its digest.
func (m manifestClient) resolve(ctx context.Context, ref reference) (string, error) {
	scheme := "https"
	if m.insecure {
		scheme = "http"
	}
	endpoint := fmt.Sprintf("%s://%s/v2/%s/manifests/%s",
		scheme, ref.endpoint(), ref.name, ref.target())

	// Two attempts at most: the first unauthenticated, the second carrying
	// whatever the registry's challenge said it wanted. A loop without that
	// bound is a loop a registry can keep going forever by challenging again.
	var authorization string
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", fmt.Errorf("registry: build manifest request: %w", err)
		}
		req.Header.Set("Accept", manifestAccept)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}

		resp, err := m.do(req)
		if err != nil {
			// The cause stays in the chain rather than being flattened into
			// the message, so a caller can still see a cancelled context or a
			// blown deadline behind the classification.
			return "", fmt.Errorf("%w: %s: %w", ErrUnreachable, ref.host, err)
		}

		if resp.StatusCode == http.StatusOK {
			digest, err := digestOf(resp, ref)
			resp.Body.Close() //nolint:errcheck
			return digest, err
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		challenge := resp.Header.Get("WWW-Authenticate")
		status, statusText := resp.StatusCode, resp.Status
		resp.Body.Close() //nolint:errcheck

		// A registry that names the image unknown has answered the question,
		// whatever status it chose to say so with. Some hide a missing
		// repository behind 401 rather than 404, and treating that as a
		// credential fault would send somebody to check a password that is
		// fine.
		//
		// Believed only once the credential has been presented, or when the
		// status says absent on its own account. Other registries refuse to
		// tell an anonymous caller apart from an absent repository and answer
		// a private image with NAME_UNKNOWN — and absent is the one answer
		// that fails a deploy outright rather than retrying it, so believing
		// it here would discard an image that is sitting in the registry on
		// the strength of a question that was never properly asked.
		if namesAnUnknownImage(body) &&
			(status == http.StatusNotFound || authorization != "") {
			return "", fmt.Errorf("%w: %s", ErrManifestNotFound, ref)
		}

		if status == http.StatusUnauthorized && attempt == 0 {
			next, err := m.authorize(ctx, challenge, ref)
			if err != nil {
				return "", err
			}
			if next == "" || next == authorization {
				return "", fmt.Errorf("%w: %s", ErrCredentialsRejected, ref.host)
			}
			authorization = next
			continue
		}
		return "", classifyStatus(status, statusText, ref)
	}
	return "", fmt.Errorf("%w: %s", ErrCredentialsRejected, ref.host)
}

// classifyStatus turns a response Yacht cannot use into exactly one of the
// three answers, with nothing falling through untyped.
//
// The default arm is the point of the function. A 400 about a reference the
// registry would not parse, a 3xx with no usable Location, a 200-that-was-not,
// an HTML error page from something sitting in front of the registry: none of
// them says the image is absent, and an unclassified error escaping here would
// leave the one caller that matters — recovery after a crash between push and
// record — deciding which of the three it meant.
func classifyStatus(status int, statusText string, ref reference) error {
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrManifestNotFound, ref)

	case status == http.StatusUnauthorized,
		status == http.StatusForbidden,
		status == http.StatusProxyAuthRequired:
		return fmt.Errorf("%w: %s answered %s", ErrCredentialsRejected, ref.host, statusText)

	case status == http.StatusRequestTimeout,
		status == http.StatusTooEarly,
		status == http.StatusTooManyRequests,
		status >= 500:
		return fmt.Errorf("%w: %s answered %s", ErrUnreachable, ref.host, statusText)

	default:
		return fmt.Errorf("%w: %s answered %s for %s, which is not an answer about the image",
			ErrUnreachable, ref.host, statusText, ref)
	}
}

// maxRedirects bounds a redirect chain. Registries redirect manifest requests
// rarely and blob requests often; five is past anything legitimate and short
// of anything a host can use to keep Yacht busy.
const maxRedirects = 5

// do sends a request under Yacht's own redirect policy.
//
// The client is copied by value rather than mutated: it shares the transport
// and keeps the timeout, and the policy travels with the request instead of
// depending on how whoever built the client happened to configure it.
func (m manifestClient) do(req *http.Request) (*http.Response, error) {
	client := *m.client()
	client.CheckRedirect = refuseUnsafeRedirect
	return client.Do(req)
}

// refuseUnsafeRedirect stops a redirect that would move a credential or drop
// the connection's protection.
//
// Written out rather than left to net/http's own rule, which strips
// Authorization on a cross-host hop and follows it anyway. Stripping is the
// right default for a browser-shaped client; here it turns a credentialled
// request into an anonymous one and produces a confusing 401 instead of saying
// that a registry tried to move the credential. The hop is refused instead, and
// the refusal names what happened.
func refuseUnsafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("registry: more than %d redirects", maxRedirects)
	}
	previous := via[len(via)-1]

	switch req.URL.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("registry: refused a redirect to a %q URL", req.URL.Scheme)
	}
	if req.URL.User != nil {
		return errors.New("registry: refused a redirect to a URL carrying credentials")
	}
	if previous.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("registry: refused a redirect from HTTPS to %s://%s",
			req.URL.Scheme, req.URL.Host)
	}
	// The first request is the one that carries the credential; net/http may
	// already have stripped it from this one, which is exactly the case to
	// refuse rather than silently continue without it.
	if via[0].Header.Get("Authorization") != "" && !sameAuthority(req.URL, via[0].URL) {
		return fmt.Errorf("registry: refused a redirect from %s to %s while carrying a credential",
			authorityOf(via[0].URL), authorityOf(req.URL))
	}
	return nil
}

// authorityOf is host:port with the scheme's default port made explicit, so
// that example.com and example.com:443 over HTTPS compare as the one host they
// are.
func authorityOf(u *url.URL) string {
	host, port := u.Hostname(), u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return strings.ToLower(net.JoinHostPort(host, port))
}

func sameAuthority(a, b *url.URL) bool { return authorityOf(a) == authorityOf(b) }

// sameRegistrySite permits the cross-host delegation real registries use
// without accepting an unrelated host named by a challenge.
//
// EffectiveTLDPlusOne matters here: comparing the last two labels by hand
// would treat auth.attacker.co.uk and registry.victim.co.uk as related. The
// public-suffix list knows that co.uk is the suffix and keeps those sites
// separate. Hosts without a registrable domain (IP literals, localhost and
// single-label internal names) are never cross-authority delegates; a registry
// using one of those can still keep its token endpoint on its own authority.
func sameRegistrySite(a, b *url.URL) bool {
	aSite, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimSuffix(
		strings.ToLower(a.Hostname()), "."))
	if err != nil {
		return false
	}
	bSite, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimSuffix(
		strings.ToLower(b.Hostname()), "."))
	return err == nil && aSite == bSite
}

// digestOf returns the digest of the manifest a registry just served.
//
// Computed over the bytes rather than taken from the header. Not every
// registry sends Docker-Content-Digest, and the digest is by definition what
// the content hashes to — so computing it works everywhere and cannot drift
// from what a puller would compute. When the header is there it is checked:
// a registry whose header disagrees with its own body is one whose answer
// should not be pinned into a release.
// What it will not do is hash something that is not a manifest. A 200 is not
// on its own a reason to believe the body: a proxy answering with a sign-in
// page, a registry that ignored Accept, an empty response — hashing any of
// them produces a digest that is a real sha256 of the wrong thing, and pinning
// a release to it is a failure nothing downstream can detect.
func digestOf(resp *http.Response, ref reference) (string, error) {
	if ct := resp.Header.Get("Content-Type"); !isManifestType(ct) {
		return "", fmt.Errorf("%w: %s served %q for %s, which is not a manifest",
			ErrUnreachable, ref.host, ct, ref)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, manifestLimit+1))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrUnreachable, ref.host, err)
	}
	if len(body) > manifestLimit {
		return "", fmt.Errorf("%w: %s served more than %d bytes as a manifest for %s",
			ErrUnreachable, ref.host, manifestLimit, ref)
	}

	sum := sha256.Sum256(body)
	computed := "sha256:" + hex.EncodeToString(sum[:])

	if stated := resp.Header.Get("Docker-Content-Digest"); stated != "" &&
		!strings.EqualFold(stated, computed) {
		return "", fmt.Errorf("%w: %s says %s is %s but served content that is %s",
			ErrUnreachable, ref.host, ref, stated, computed)
	}
	// Asking by digest and being handed something else is the one case where
	// the content is provably not what was asked for, whatever any header says.
	if ref.digest != "" && !strings.EqualFold(ref.digest, computed) {
		return "", fmt.Errorf("%w: %s served %s when asked for %s",
			ErrUnreachable, ref.host, computed, ref)
	}
	return computed, nil
}

// authorize answers a registry's challenge and returns the Authorization
// header to retry with, or "" when there is nothing to offer.
func (m manifestClient) authorize(
	ctx context.Context, challenge string, ref reference,
) (string, error) {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "bearer":
		return m.bearerToken(ctx, params, ref)
	case "basic":
		if m.creds == nil {
			return "", nil
		}
		return "Basic " + basicAuth(*m.creds), nil
	default:
		return "", nil
	}
}

// registryURL is the registry as Yacht addresses it, which is what a realm has
// to be judged against.
func (m manifestClient) registryURL(ref reference) *url.URL {
	scheme := "https"
	if m.insecure {
		scheme = "http"
	}
	return &url.URL{Scheme: scheme, Host: ref.endpoint()}
}

// realmFor decides whether a challenge's token endpoint may be asked at all.
//
// A WWW-Authenticate header is attacker-controlled input in every case that
// matters: over plain HTTP anyone on the path writes it, and over TLS it is
// written by whichever registry an app happens to point at, which is anybody
// with a public image. Following it unconditionally makes Yacht a general
// outbound HTTP client aimed wherever a header says, holding the install's
// registry password.
//
// The rule is the narrowest one that leaves the Distribution flow working:
//
//	registry reached over │ realm scheme │ realm authority
//	──────────────────────┼──────────────┼────────────────────────────
//	HTTPS                 │ https only   │ same authority or registrable site
//	HTTP (declared        │ http or      │ the registry's own, exactly
//	insecure by operator) │ https        │
//
// The top row is what Docker Hub needs — registry-1.docker.io challenges to
// auth.docker.io, and GitLab's registry to gitlab.com — so delegation to a
// separate token service keeps working. The shared registrable-site rule is
// the trust boundary: auth.docker.io and registry-1.docker.io are both under
// docker.io, while a realm under an unrelated domain is refused. What it
// cannot do is move the credential onto a plain-HTTP connection or name an
// arbitrary HTTPS host, because neither is something a registry needs.
//
// The bottom row is where the credential would otherwise be genuinely at risk.
// A plain-HTTP registry can be rewritten in flight by anyone on the path, so a
// realm it names proves nothing; restricting it to the registry's own address
// means the worst an on-path attacker can do is send the credential to the host
// it was already going to. That does need same-authority, which is why an
// insecure registry cannot delegate to a separate token service — a shape no
// install that has declared plain HTTP is realistically running, and the one
// place the policy is deliberately narrower than the spec.
//
// This needs no operator-configured trust setting, which is the outcome worth
// having: the rule is derived from how the registry is already addressed rather
// than from a list somebody has to maintain and eventually gets wrong.
func (m manifestClient) realmFor(realm string, ref reference) (*url.URL, error) {
	if realm == "" {
		return nil, fmt.Errorf("%w: %s asked for a bearer token but named no realm",
			ErrCredentialsRejected, ref.host)
	}
	u, err := url.Parse(realm)
	if err != nil {
		return nil, fmt.Errorf("%w: %s named %q as its token endpoint: %w",
			ErrCredentialsRejected, ref.host, realm, err)
	}

	switch {
	case !u.IsAbs(), u.Opaque != "", u.Host == "":
		return nil, refusedRealm(ref, realm, "it is not an absolute http or https URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, refusedRealm(ref, realm, "only http and https token endpoints are used")
	case u.User != nil:
		// Userinfo in the realm is a way to make the request carry a
		// credential of the challenger's choosing, and no registry needs it.
		return nil, refusedRealm(ref, realm, "it carries credentials in the URL")
	}

	registry := m.registryURL(ref)
	switch {
	case !m.insecure && u.Scheme != "https":
		return nil, refusedRealm(ref, realm,
			"a registry reached over HTTPS may not send the credential over plain HTTP")
	case !m.insecure && !sameAuthority(u, registry) && !sameRegistrySite(u, registry):
		return nil, refusedRealm(ref, realm,
			"an external token endpoint must share the registry's registrable domain")
	case m.insecure && !sameAuthority(u, registry):
		return nil, refusedRealm(ref, realm,
			"a registry configured for plain HTTP may only issue tokens from its own address")
	}
	return u, nil
}

// mayCredential reports whether the install's credential may go to a realm
// that realmFor has already accepted. See realmFor for why these are the two
// safe cases.
func (m manifestClient) mayCredential(realm *url.URL, ref reference) bool {
	registry := m.registryURL(ref)
	return sameAuthority(realm, registry) ||
		(!m.insecure && sameRegistrySite(realm, registry))
}

func refusedRealm(ref reference, realm, why string) error {
	return fmt.Errorf("%w: %s asked for a token from %q, which Yacht will not request: %s",
		ErrCredentialsRejected, ref.host, realm, why)
}

// bearerToken exchanges the install's credential for a scoped pull token.
//
// The scope the registry asked for is preferred over one built here. A
// registry knows what it wants to be asked for, and a scope guessed from the
// repository path is the kind of near-miss that fails only against the one
// registry nobody tested.
func (m manifestClient) bearerToken(
	ctx context.Context, params map[string]string, ref reference,
) (string, error) {
	u, err := m.realmFor(params["realm"], ref)
	if err != nil {
		return "", err
	}

	query := u.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + ref.name + ":pull"
	}
	query.Set("scope", scope)
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("registry: build token request: %w", err)
	}
	// Restated rather than assumed. realmFor has already refused every realm
	// this would be wrong for, so the condition is true whenever there is a
	// credential at all — but a later loosening of the realm rule should not
	// silently become a loosening of where the password goes.
	if m.creds != nil && m.mayCredential(u, ref) {
		req.SetBasicAuth(m.creds.Username, m.creds.Password)
	}

	resp, err := m.doToken(req, ref)
	if err != nil {
		if errors.Is(err, errUnsafeRealmAddress) {
			return "", fmt.Errorf("%w: %s: %w", ErrCredentialsRejected, u.Host, err)
		}
		return "", fmt.Errorf("%w: %s: %w", ErrUnreachable, u.Host, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusProxyAuthRequired:
		return "", fmt.Errorf("%w: %s", ErrCredentialsRejected, ref.host)
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooEarly,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return "", fmt.Errorf("%w: %s answered %s", ErrUnreachable, u.Host, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: the token endpoint for %s answered %s",
			ErrUnreachable, ref.host, resp.Status)
	}

	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return "", fmt.Errorf("%w: the token endpoint for %s did not answer with a token: %w",
			ErrUnreachable, ref.host, err)
	}
	// Two names for one thing: the distribution spec says "token", OAuth2 says
	// "access_token", and registries in the wild send one or the other.
	value := token.Token
	if value == "" {
		value = token.AccessToken
	}
	if value == "" {
		return "", nil
	}
	return "Bearer " + value, nil
}

var errUnsafeRealmAddress = errors.New("registry: token realm resolves to a non-public address")

// doToken applies the ordinary redirect rules and, for a delegated token
// service, pins dialing to public addresses resolved inside the dial itself.
//
// A preflight DNS lookup is insufficient: a hostile name can answer publicly
// during validation and privately when net/http connects. Resolving and
// selecting the address in DialContext closes that gap. Same-authority realms
// are exempt because an explicitly configured private registry must remain
// usable; the request is not being delegated anywhere the operator did not
// already choose.
func (m manifestClient) doToken(req *http.Request, ref reference) (*http.Response, error) {
	client := *m.client()
	origin := req.URL
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if err := refuseUnsafeRedirect(next, via); err != nil {
			return err
		}
		if !sameAuthority(next.URL, origin) {
			return fmt.Errorf("registry: refused a token redirect from %s to %s",
				authorityOf(origin), authorityOf(next.URL))
		}
		return nil
	}

	if !sameAuthority(req.URL, m.registryURL(ref)) {
		transport, err := publicRealmTransport(client.Transport)
		if err != nil {
			return nil, err
		}
		client.Transport = transport
	}
	return client.Do(req)
}

// publicRealmTransport returns a private copy of an HTTP transport whose
// connections can reach public addresses only. Proxying is deliberately
// disabled for this one delegated request: otherwise DialContext sees the
// proxy rather than the challenged destination and cannot enforce the rule.
func publicRealmTransport(base http.RoundTripper) (*http.Transport, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	transport, ok := base.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("registry: cannot secure token dialing through %T", base)
	}
	clone := transport.Clone()
	clone.Proxy = nil
	clone.DialContext = dialPublicRealm
	return clone, nil
}

func dialPublicRealm(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{}
	var dialErrors []error
	foundPublic := false
	for _, address := range addresses {
		address = address.Unmap()
		if !publicRealmAddress(address) {
			continue
		}
		if network == "tcp4" && !address.Is4() || network == "tcp6" && !address.Is6() {
			continue
		}
		foundPublic = true
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErrors = append(dialErrors, err)
	}
	if !foundPublic {
		return nil, fmt.Errorf("%w: %s", errUnsafeRealmAddress, host)
	}
	return nil, errors.Join(dialErrors...)
}

var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

func publicRealmAddress(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsValid() && address.IsGlobalUnicast() &&
		!address.IsPrivate() && !address.IsLoopback() &&
		!address.IsLinkLocalUnicast() && !carrierGradeNAT.Contains(address)
}

func (m manifestClient) client() *http.Client {
	if m.http != nil {
		return m.http
	}
	return http.DefaultClient
}

func basicAuth(c Credentials) string {
	return base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
}

// namesAnUnknownImage reports whether an error body says the image is not
// there, using the distribution spec's own error codes rather than the status.
func namesAnUnknownImage(body []byte) bool {
	var payload struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	for _, e := range payload.Errors {
		switch strings.ToUpper(e.Code) {
		case "MANIFEST_UNKNOWN", "NAME_UNKNOWN":
			return true
		}
	}
	return false
}

// parseChallenge splits a WWW-Authenticate value into its scheme and
// parameters. Values are read quote-aware because a scope legitimately
// contains commas, as in scope="repository:acme/web:pull,push".
func parseChallenge(v string) (string, map[string]string) {
	v = strings.TrimSpace(v)
	scheme, rest, _ := strings.Cut(v, " ")

	params := map[string]string{}
	for i := 0; i < len(rest); {
		for i < len(rest) && (rest[i] == ' ' || rest[i] == ',' || rest[i] == '\t') {
			i++
		}
		start := i
		for i < len(rest) && rest[i] != '=' && rest[i] != ',' {
			i++
		}
		if i >= len(rest) || rest[i] != '=' {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[start:i]))
		i++

		var value string
		if i < len(rest) && rest[i] == '"' {
			i++
			var b strings.Builder
			for i < len(rest) && rest[i] != '"' {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				b.WriteByte(rest[i])
				i++
			}
			if i < len(rest) {
				i++
			}
			value = b.String()
		} else {
			start := i
			for i < len(rest) && rest[i] != ',' {
				i++
			}
			value = strings.TrimSpace(rest[start:i])
		}
		if key != "" {
			params[key] = value
		}
	}
	return scheme, params
}

// reference is an image reference split into what a registry request needs.
type reference struct {
	// host is the registry as the reference names it, so that it can be
	// compared with the host this install configured.
	host string

	// name is the repository path, always fully qualified — "library/postgres"
	// rather than "postgres", because that is what the API path takes.
	name string

	tag    string
	digest string
}

// The registry Docker Hub references resolve against, and the name they are
// written with. Two values because "docker.io" is what people write and is
// what a configured host would say, while the API lives elsewhere.
const (
	dockerHubHost     = "docker.io"
	dockerHubEndpoint = "registry-1.docker.io"
)

// target is what goes in the manifests path: the digest when the reference
// pins one, and otherwise the tag.
//
// A digest is addressable in its own right, which is what makes "is this
// release's image still in the registry" a question the registry can answer.
// ResolveDigest does not ask it — a reference that names a digest is already
// its own answer — but the wire layer has to be able to.
func (r reference) target() string {
	if r.digest != "" {
		return r.digest
	}
	return r.tag
}

// endpoint is the host to actually connect to.
func (r reference) endpoint() string {
	if r.host == dockerHubHost {
		return dockerHubEndpoint
	}
	return r.host
}

// String is the reference in its fully qualified form, which is what an error
// about it should name — "postgres:16" and "docker.io/library/postgres:16" are
// the same image, and only one of them says where it was looked for.
func (r reference) String() string {
	s := r.host + "/" + r.name
	if r.digest != "" {
		return s + "@" + r.digest
	}
	return s + ":" + r.tag
}

// Repository names as the distribution spec writes them. Deliberately not the
// repository regexp used for the install's own path: that one is about what a
// person may type into a settings form, and this one has to accept every name
// a real registry already serves, including "my--app" and "a__b".
var refNameRE = regexp.MustCompile(
	`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)

var refTagRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

// Only sha256. Every registry in use serves it, computing it is what proves
// the digest rather than trusting a header, and accepting an algorithm Yacht
// cannot compute would mean pinning a digest it cannot verify.
var refDigestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// parseReference splits an image reference the way a container runtime does.
//
// Including the defaults, because references without a host are what this
// system actually holds: a blueprint says "postgres:16-alpine" and a person
// types "nginx". Applying the same defaults a kubelet would means resolution
// asks about the image that would actually be pulled, rather than about a
// near-miss that happens to be spelled differently.
func parseReference(ref string) (reference, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return reference{}, errors.New("registry: an image reference is required")
	}
	if strings.Contains(ref, "://") {
		return reference{}, fmt.Errorf(
			"registry: %q carries a scheme — an image reference starts at the host", ref)
	}

	rest := ref
	var out reference

	// The digest comes off first. It contains a colon, so looking for the tag
	// before removing it finds the algorithm separator instead.
	if name, digest, found := strings.Cut(rest, "@"); found {
		if !refDigestRE.MatchString(digest) {
			return reference{}, fmt.Errorf(
				"registry: %q is not a digest — expected sha256: and 64 hex characters", digest)
		}
		out.digest = digest
		rest = name
	}

	if head, tail, found := strings.Cut(rest, "/"); found &&
		(strings.ContainsAny(head, ".:") || head == "localhost") {
		out.host = head
		rest = tail
	}

	tagged := false
	if name, tag, found := strings.Cut(rest, ":"); found {
		rest = name
		// A reference may pin both, as in acme/web:v2@sha256:… . The digest is
		// the identity; the tag alongside it is a label, and following it
		// would be asking about a name rather than about the thing named.
		if out.digest == "" {
			out.tag, tagged = tag, true
		}
	}

	out.host = normalizeHost(out.host)
	if err := ValidateHost(out.host); err != nil {
		return reference{}, err
	}

	out.name = rest
	if out.host == dockerHubHost && !strings.Contains(out.name, "/") {
		// Docker Hub's official images live under library/, and a reference to
		// "postgres" means that one.
		out.name = "library/" + out.name
	}
	if !refNameRE.MatchString(out.name) {
		return reference{}, fmt.Errorf("registry: %q is not a repository name in %q", out.name, ref)
	}

	if out.digest == "" {
		// Absent and empty are different. A reference with nothing after the
		// colon is a mistake, and defaulting it to latest would resolve a
		// typo to whatever happens to be tagged that.
		if !tagged {
			out.tag = "latest"
		}
		if !refTagRE.MatchString(out.tag) {
			return reference{}, fmt.Errorf("registry: %q is not a tag in %q", out.tag, ref)
		}
	}
	return out, nil
}

// normalizeHost reduces the several spellings of Docker Hub to one, so that a
// reference and a configured host can be compared as strings.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "", dockerHubHost, dockerHubEndpoint, "index.docker.io":
		return dockerHubHost
	}
	return host
}
