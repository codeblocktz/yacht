package registry

// These run against a fake registry rather than a real one. What a fake can
// prove is the part Yacht decides: which answer each kind of failure is, that
// the credential goes to one host and no other, and that the digest comes from
// the bytes rather than from a header. What it cannot prove is media-type
// negotiation against a real implementation, which is what the integration
// test in manifest_integration_test.go is for.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A manifest small enough to read and real enough in shape to be one.
var testManifest = []byte(`{"schemaVersion":2,` +
	`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
	`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":7,` +
	`"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"},` +
	`"layers":[]}`)

func digestOfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// serveManifest answers the way a registry does, media type and all.
//
// A helper rather than a header repeated in every handler, because the header
// is not incidental: without it the body is not a manifest and resolution
// refuses it before hashing. Tests that are about something else should not
// have to restate that to get past it — and a test that forgot would otherwise
// go green on the refusal rather than on what it names.
func serveManifest(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	w.Write(body) //nolint:errcheck
}

// fakeRegistry serves a handler over plain HTTP and returns the host to
// address it by, which is what an image reference carries.
func fakeRegistry(t *testing.T, h http.HandlerFunc) (host string, client manifestClient) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"),
		manifestClient{http: srv.Client(), insecure: true}
}

// recorder keeps what a handler saw, for a test goroutine to read afterwards.
// Locked rather than plain fields because the handler runs on the server's
// goroutine and `make test` runs with -race.
type recorder struct {
	mu     sync.Mutex
	values map[string]string
}

func (r *recorder) set(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
}

func (r *recorder) get(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[key]
}

func resolveRef(t *testing.T, c manifestClient, ref string) (string, error) {
	t.Helper()
	parsed, err := parseReference(ref)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}
	return c.resolve(context.Background(), parsed)
}

// The digest is what the manifest hashes to.
//
// Computed rather than read out of Docker-Content-Digest, because not every
// registry sends that header and because a digest taken on trust is a digest
// that can disagree with what a kubelet will compute when it pulls.
func TestATagResolvesToTheDigestOfWhatWasServed(t *testing.T) {
	want := digestOfBytes(testManifest)

	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/acme/web/manifests/abc123" {
			t.Errorf("asked for %q", r.URL.Path)
		}
		// A multi-architecture image should resolve to its index, so the
		// request has to say it can read one.
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "image.index") ||
			!strings.Contains(accept, "manifest.list") {
			t.Errorf("index media types are not accepted: %q", accept)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", want)
		w.Write(testManifest) //nolint:errcheck
	})

	got, err := resolveRef(t, client, host+"/acme/web:abc123")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != want {
		t.Errorf("digest is %q, want %q", got, want)
	}
}

// A registry with no Docker-Content-Digest still answers.
//
// The media type is still sent, because that is the header resolution needs;
// the one deliberately missing here is the digest.
func TestARegistryThatSendsNoDigestHeaderIsStillUnderstood(t *testing.T) {
	host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		if got := w.Header().Get("Docker-Content-Digest"); got != "" {
			t.Errorf("the test set the header it is about: %q", got)
		}
		serveManifest(w, testManifest)
	})

	got, err := resolveRef(t, client, host+"/acme/web:abc123")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != digestOfBytes(testManifest) {
		t.Errorf("digest is %q", got)
	}
}

// An absent tag is an answer, not a failure to get one.
//
// The whole point of the distinction: an image that is genuinely not there
// will not appear by waiting, so the deploy that wanted it must fail and say
// to build again. Nothing that reaches this must look like a transient fault.
func TestAnAbsentTagIsAnAnswerRatherThanAFailure(t *testing.T) {
	host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"unknown"}]}`)
	})

	_, err := resolveRef(t, client, host+"/acme/web:gone")
	if !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("an absent tag is %v, want ErrManifestNotFound", err)
	}
	if errors.Is(err, ErrUnreachable) || errors.Is(err, ErrCredentialsRejected) {
		t.Errorf("an absent tag also reads as a failure to reach: %v", err)
	}
	// The error names where it looked. "not in the registry" without the
	// reference is true of every image that was never pushed.
	if !strings.Contains(err.Error(), "acme/web:gone") {
		t.Errorf("the error does not name the reference: %v", err)
	}
}

// Some registries hide a missing repository behind 401 rather than 404.
//
// Reading the spec's own error code rather than the status keeps that from
// sending somebody to check a password that is fine. The credential has been
// presented by the time the registry says it, which is what makes "unknown"
// mean the image and not the caller.
func TestAnImageNamedUnknownIsMissingWhateverTheStatusSays(t *testing.T) {
	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository not known"}]}`)
	})
	client.creds = &Credentials{Host: host, Username: "bot", Password: "hunter2"}

	_, err := resolveRef(t, client, host+"/acme/never-pushed:v1")
	if !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("a repository named unknown reads as %v, want ErrManifestNotFound", err)
	}
}

// An "unknown" said before the credential was presented is not an answer.
//
// Registries that will not tell an anonymous caller apart from an absent
// repository answer a private image with NAME_UNKNOWN, and the image is
// sitting there the whole time. Absent is the one answer that fails a deploy
// outright rather than retrying it, so it is the answer to be slow to give:
// believing this one would discard a pushed image on the strength of a
// question that was never properly asked.
func TestAnUnknownSaidBeforeAuthenticatingIsNotAnAbsentImage(t *testing.T) {
	seen := &recorder{}
	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository not known"}]}`)
			return
		}
		seen.set("authenticated", "yes")
		serveManifest(w, testManifest)
	})
	client.creds = &Credentials{Host: host, Username: "bot", Password: "hunter2"}

	got, err := resolveRef(t, client, host+"/acme/private:v1")
	if err != nil {
		t.Fatalf("an image that is in the registry was not found: %v", err)
	}
	if got != digestOfBytes(testManifest) {
		t.Errorf("digest is %q, want %q", got, digestOfBytes(testManifest))
	}
	if seen.get("authenticated") != "yes" {
		t.Error("resolution gave up before presenting the credential")
	}
}

// A reference that already names a digest is already the answer.
//
// Asked of a Store with no database and no HTTP client, so the test fails
// rather than passes if resolution ever reaches for either: pass-through has
// to happen before the credential is unsealed, not after.
func TestAReferenceThatAlreadyPinsADigestIsNotLookedUp(t *testing.T) {
	want := digestOfBytes(testManifest)

	got, err := (&Store{}).ResolveDigest(context.Background(),
		"ghcr.io/acme/web@"+want)
	if err != nil {
		t.Fatalf("resolve a pinned reference: %v", err)
	}
	if got != want {
		t.Errorf("digest is %q, want %q", got, want)
	}
}

// A tag alongside a digest does not win.
func TestADigestBeatsATagPinnedNextToIt(t *testing.T) {
	want := digestOfBytes(testManifest)

	got, err := (&Store{}).ResolveDigest(context.Background(),
		"ghcr.io/acme/web:v2@"+want)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != want {
		t.Errorf("digest is %q, want %q", got, want)
	}
}

// A digest is addressable in its own right.
//
// ResolveDigest never asks — a pinned reference is already the answer — but
// the wire layer has to be able to, because "is this release's image still in
// the registry" is the question rollback availability turns on and only the
// registry can answer it.
func TestADigestCanBeAskedAboutInItsOwnRight(t *testing.T) {
	want := digestOfBytes(testManifest)
	seen := &recorder{}

	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		seen.set("path", r.URL.Path)
		serveManifest(w, testManifest)
	})

	got, err := resolveRef(t, client, host+"/acme/web@"+want)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != want {
		t.Errorf("digest is %q, want %q", got, want)
	}
	if seen.get("path") != "/v2/acme/web/manifests/"+want {
		t.Errorf("asked at %q, want the digest in the path", seen.get("path"))
	}
}

// A registry that hands back something other than what was asked for.
//
// Served as a real manifest, media type and all, so the refusal has to come
// from comparing the content with the digest asked for. Everything about this
// response is well-formed except which image it is — which is the only version
// of the case worth testing, since a malformed one is refused a step earlier
// and would leave this branch covered by nothing.
func TestARegistryServingTheWrongContentForADigestIsRefused(t *testing.T) {
	other := []byte(`{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	asked := digestOfBytes(testManifest)

	host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		serveManifest(w, other)
	})

	_, err := resolveRef(t, client, host+"/acme/web@"+asked)
	if err == nil {
		t.Fatal("content that is not the digest asked for was accepted")
	}
	if errors.Is(err, ErrManifestNotFound) {
		t.Errorf("wrong content reads as an absent image: %v", err)
	}
	// Asserted on the message, not just the type: the media-type refusal and
	// the digest-contradiction refusal are both ErrUnreachable, so the type
	// alone cannot tell whether this reached the branch it is named for.
	if !strings.Contains(err.Error(), digestOfBytes(other)) ||
		!strings.Contains(err.Error(), asked) {
		t.Errorf("the error does not say what was served against what was asked: %v", err)
	}
}

// A registry that cannot be reached has said nothing about the image.
//
// Failing an attempt on this would discard an image that is sitting in the
// registry, which is exactly the recovery case the digest exists for.
func TestARegistryThatCannotBeReachedIsNotAnAbsentImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {}))
	host := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	client := manifestClient{http: &http.Client{Timeout: 5 * time.Second}, insecure: true}
	_, err := resolveRef(t, client, host+"/acme/web:abc123")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a dead registry is %v, want ErrUnreachable", err)
	}
	if errors.Is(err, ErrManifestNotFound) {
		t.Errorf("a dead registry also reads as an absent image: %v", err)
	}
}

// A registry failing on its own account is the same answer as an absent one.
func TestARegistryFailingOnItsOwnAccountIsUnreachable(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})

		_, err := resolveRef(t, client, host+"/acme/web:abc123")
		if !errors.Is(err, ErrUnreachable) {
			t.Errorf("%d is %v, want ErrUnreachable", status, err)
		}
		if errors.Is(err, ErrManifestNotFound) {
			t.Errorf("%d reads as an absent image: %v", status, err)
		}
	}
}

// Rejected credentials are neither transient nor a statement about the image.
func TestRejectedCredentialsAreTheirOwnAnswer(t *testing.T) {
	var tokenHost string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if user, _, ok := r.BasicAuth(); !ok || user != "bot" {
			t.Errorf("the credential did not reach the token endpoint: %q", user)
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="http://%s/token",service="fake"`, tokenHost))
		w.WriteHeader(http.StatusUnauthorized)
	})

	host, client := fakeRegistry(t, mux.ServeHTTP)
	tokenHost = host
	client.creds = &Credentials{Host: host, Username: "bot", Password: "wrong"}

	_, err := resolveRef(t, client, host+"/acme/web:abc123")
	if !errors.Is(err, ErrCredentialsRejected) {
		t.Fatalf("a refused credential is %v, want ErrCredentialsRejected", err)
	}
	if errors.Is(err, ErrManifestNotFound) || errors.Is(err, ErrUnreachable) {
		t.Errorf("a refused credential reads as something retryable or absent: %v", err)
	}
}

// A 401 with no credential to offer is still a credential answer.
func TestAChallengeWithNothingToOfferIsARefusal(t *testing.T) {
	host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := resolveRef(t, client, host+"/acme/web:abc123")
	if !errors.Is(err, ErrCredentialsRejected) {
		t.Fatalf("an unanswerable challenge is %v, want ErrCredentialsRejected", err)
	}
}

// The bearer flow: challenge, token, retry.
//
// The scope the registry asked for is used rather than one built here, because
// a registry knows what it wants to be asked for.
func TestABearerChallengeIsAnsweredWithTheScopeItAskedFor(t *testing.T) {
	var tokenHost string
	seen := &recorder{}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		seen.set("scope", r.URL.Query().Get("scope"))
		if user, pass, ok := r.BasicAuth(); !ok || user != "bot" || pass != "hunter2" {
			t.Errorf("the credential did not reach the token endpoint")
		}
		fmt.Fprint(w, `{"token":"issued-token"}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer realm="http://%s/token",service="fake",`+
					`scope="repository:acme/web:pull,push"`, tokenHost))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		seen.set("authorization", auth)
		serveManifest(w, testManifest)
	})

	host, client := fakeRegistry(t, mux.ServeHTTP)
	tokenHost = host
	client.creds = &Credentials{Host: host, Username: "bot", Password: "hunter2"}

	got, err := resolveRef(t, client, host+"/acme/web:abc123")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != digestOfBytes(testManifest) {
		t.Errorf("digest is %q", got)
	}
	if seen.get("authorization") != "Bearer issued-token" {
		t.Errorf("the token was not presented: %q", seen.get("authorization"))
	}
	// A comma inside the quoted scope is part of the value, not a separator.
	if seen.get("scope") != "repository:acme/web:pull,push" {
		t.Errorf("scope is %q, want the one the registry asked for", seen.get("scope"))
	}
}

// A registry that wants Basic gets Basic.
func TestABasicChallengeIsAnswered(t *testing.T) {
	seen := &recorder{}
	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		seen.set("user", user)
		seen.set("password", pass)
		serveManifest(w, testManifest)
	})
	client.creds = &Credentials{Host: host, Username: "bot", Password: "hunter2"}

	if _, err := resolveRef(t, client, host+"/acme/web:abc123"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if seen.get("user") != "bot" || seen.get("password") != "hunter2" {
		t.Errorf("the credential was not presented: %q/%q",
			seen.get("user"), seen.get("password"))
	}
}

// A registry whose header disagrees with its own body is refused.
//
// Not pinned into a release on either reading. One of the two is wrong, and
// there is no way to tell which — so the honest outcome is a failure that
// names both rather than a digest that may not be the image.
func TestAManifestWhoseStatedDigestDisagreesIsRefused(t *testing.T) {
	stated := "sha256:" + strings.Repeat("a", 64)

	host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", stated)
		serveManifest(w, testManifest)
	})

	_, err := resolveRef(t, client, host+"/acme/web:abc123")
	if err == nil {
		t.Fatal("a registry contradicting itself was accepted")
	}
	if errors.Is(err, ErrManifestNotFound) {
		t.Errorf("a contradiction reads as an absent image: %v", err)
	}
	// Both readings named, so the failure says what disagreed with what — and
	// so this cannot quietly start passing on the media-type refusal instead,
	// which returns the same type.
	if !strings.Contains(err.Error(), stated) ||
		!strings.Contains(err.Error(), digestOfBytes(testManifest)) {
		t.Errorf("the error does not name both readings: %v", err)
	}
}

// The install's password goes to the install's registry and nowhere else.
//
// A person can point an app at any public image. Presenting this install's
// registry password to a host that merely asked for one would hand it to
// whoever runs that host.
func TestTheInstallsCredentialGoesOnlyToItsOwnRegistry(t *testing.T) {
	set := Settings{Host: "ghcr.io", Repository: "acme"}

	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"ghcr.io/acme/owner-web:abc123", true},
		{"ghcr.io/someone/else:v1", true},
		{"docker.io/library/postgres:16", false},
		{"postgres:16-alpine", false},
		{"registry.example.com/acme/web:v1", false},
		{"ghcr.io.evil.example.com/acme/web:v1", false},
	} {
		parsed, err := parseReference(tc.ref)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.ref, err)
		}
		if got := ownHost(set, parsed); got != tc.want {
			t.Errorf("%q gets the credential: %v, want %v", tc.ref, got, tc.want)
		}
	}

	// An install with no registry configured has nothing to send anywhere.
	parsed, _ := parseReference("ghcr.io/acme/web:v1")
	if ownHost(Settings{}, parsed) {
		t.Error("an unconfigured install offered a credential")
	}
}

// A reference is split the way a container runtime splits it.
//
// The defaults matter because references without a host are what this system
// actually holds: a blueprint says "postgres:16-alpine". Resolving a different
// spelling from the one the kubelet will pull would answer about the wrong
// image.
func TestAnImageReferenceIsSplitTheWayARuntimeSplitsIt(t *testing.T) {
	for _, tc := range []struct {
		ref                          string
		host, name, tag, endpointFor string
	}{
		{"postgres:16-alpine", "docker.io", "library/postgres", "16-alpine", "registry-1.docker.io"},
		{"nginx", "docker.io", "library/nginx", "latest", "registry-1.docker.io"},
		{"nginxinc/nginx-unprivileged:1.27-alpine", "docker.io",
			"nginxinc/nginx-unprivileged", "1.27-alpine", "registry-1.docker.io"},
		{"docker.io/library/redis:7", "docker.io", "library/redis", "7", "registry-1.docker.io"},
		{"index.docker.io/library/redis:7", "docker.io", "library/redis", "7", "registry-1.docker.io"},
		{"ghcr.io/acme/web:abc123", "ghcr.io", "acme/web", "abc123", "ghcr.io"},
		{"ghcr.io/acme/deep/path:v1", "ghcr.io", "acme/deep/path", "v1", "ghcr.io"},
		{"localhost:5000/web:v1", "localhost:5000", "web", "v1", "localhost:5000"},
		{"registry.example.com:5000/acme/web", "registry.example.com:5000",
			"acme/web", "latest", "registry.example.com:5000"},
		// Names a real registry serves that the install's own settings form
		// would refuse.
		{"ghcr.io/acme/my--app:v1", "ghcr.io", "acme/my--app", "v1", "ghcr.io"},
		{"ghcr.io/acme/a__b:v1", "ghcr.io", "acme/a__b", "v1", "ghcr.io"},
	} {
		got, err := parseReference(tc.ref)
		if err != nil {
			t.Errorf("%q was refused: %v", tc.ref, err)
			continue
		}
		if got.host != tc.host || got.name != tc.name || got.tag != tc.tag {
			t.Errorf("%q split to host=%q name=%q tag=%q, want %q/%q/%q",
				tc.ref, got.host, got.name, got.tag, tc.host, tc.name, tc.tag)
		}
		if got.endpoint() != tc.endpointFor {
			t.Errorf("%q is asked at %q, want %q", tc.ref, got.endpoint(), tc.endpointFor)
		}
	}
}

// A pinned reference keeps its digest and grows no tag.
func TestAPinnedReferenceCarriesItsDigest(t *testing.T) {
	want := digestOfBytes(testManifest)
	got, err := parseReference("ghcr.io/acme/web@" + want)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.digest != want {
		t.Errorf("digest is %q, want %q", got.digest, want)
	}
	if got.tag != "" {
		t.Errorf("a pinned reference grew the tag %q", got.tag)
	}
	if !strings.HasSuffix(got.String(), "@"+want) {
		t.Errorf("the reference reads as %q", got)
	}
}

// Malformed references are refused here rather than at pull time.
func TestAReferenceThatIsNotOneIsRefused(t *testing.T) {
	for _, tc := range []struct{ ref, why string }{
		{"", "empty"},
		{"   ", "blank"},
		{"https://ghcr.io/acme/web:v1", "carries a scheme"},
		{"ghcr.io/Acme/Web:v1", "uppercase repository"},
		{"ghcr.io/acme/web@sha256:abc", "digest is too short"},
		{"ghcr.io/acme/web@md5:" + strings.Repeat("a", 32), "digest is not sha256"},
		{"ghcr.io//web:v1", "empty path segment"},
		{"ghcr.io/acme/web:", "empty tag"},
		{"ghcr.io/acme/web:-bad", "tag starts with a dash"},
		{"ghcr..io/acme/web:v1", "host has an empty label"},
	} {
		if _, err := parseReference(tc.ref); err == nil {
			t.Errorf("%q was accepted (%s)", tc.ref, tc.why)
		}
	}
}

// A challenge is read the way an HTTP header is written, not by splitting on
// commas: a scope legitimately contains one.
func TestAChallengeIsReadQuoteAware(t *testing.T) {
	scheme, params := parseChallenge(
		`Bearer realm="https://auth.example.com/token",service="reg",` +
			`scope="repository:acme/web:pull,push",error="insufficient_scope"`)

	if !strings.EqualFold(scheme, "bearer") {
		t.Errorf("scheme is %q", scheme)
	}
	if params["realm"] != "https://auth.example.com/token" {
		t.Errorf("realm is %q", params["realm"])
	}
	if params["scope"] != "repository:acme/web:pull,push" {
		t.Errorf("scope is %q", params["scope"])
	}
	if params["error"] != "insufficient_scope" {
		t.Errorf("error is %q", params["error"])
	}

	// Unquoted values and a bare scheme are both things registries send.
	scheme, params = parseChallenge(`Basic realm=fake`)
	if !strings.EqualFold(scheme, "basic") || params["realm"] != "fake" {
		t.Errorf("unquoted challenge read as %q %v", scheme, params)
	}
}
