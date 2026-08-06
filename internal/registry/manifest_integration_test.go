package registry

// Resolution against a real registry.
//
// A fake registry cannot prove the part that actually breaks. Media-type
// negotiation, whether a multi-architecture image comes back as an index or as
// one platform's manifest, whether the digest header is sent at all, what a
// token endpoint wants, whether a real implementation agrees that the bytes it
// served hash to the digest it named — these are the details a fake is written
// to agree with, so agreement proves nothing.
//
// There are two ways to point this at one, and they are for different jobs:
//
//	YACHT_TEST_REGISTRY      a writable registry, which the test seeds itself
//	YACHT_TEST_REGISTRY_REF  a reference that already exists somewhere
//
// The first is the one CI uses. Given an empty `registry:2` it pushes a fixture
// image over the Distribution API — no docker CLI, no network beyond the
// container, the same bytes and therefore the same digest every run — and then
// asks about what it pushed. That makes this an ordinary automatic test rather
// than one somebody has to remember.
//
// The second is for asking a registry Yacht did not set up: Docker Hub, for the
// bearer-token flow and a real multi-architecture index, or the disposable K3s
// cluster's own registry for a tag Yacht's builder pushed:
//
//	YACHT_TEST_REGISTRY_REF=docker.io/library/alpine:3.20 go test ./internal/registry -run Real
//	YACHT_TEST_REGISTRY_REF=localhost:5111/owner-local-gitapp:<deployment-tag> \
//	  YACHT_TEST_REGISTRY_INSECURE=1 go test ./internal/registry -run Real
//
// Those stay manual and out of CI on purpose: a test that fails when Docker Hub
// rate-limits is a test people learn to ignore.
//
// Credentials for either, when the registry wants them:
//
//	YACHT_TEST_REGISTRY_USERNAME, YACHT_TEST_REGISTRY_PASSWORD
//	YACHT_TEST_REGISTRY_INSECURE=1 for a plain-HTTP registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// The fixture image. Fixed bytes, so its digest is the same on every run and in
// every checkout — which is what lets the test assert on resolution rather than
// on whatever happened to be pushed.
var (
	fixtureConfig = []byte(`{"architecture":"amd64","os":"linux",` +
		`"rootfs":{"type":"layers","diff_ids":[]},"config":{}}`)
	fixtureLayer = []byte("yacht manifest resolution fixture layer\n")
)

const fixtureRepository = "yacht-test/manifest-fixture"

func fixtureManifest() []byte {
	return fmt.Appendf(nil, `{"schemaVersion":2,`+
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json",`+
		`"size":%d,"digest":%q},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",`+
		`"size":%d,"digest":%q}]}`,
		len(fixtureConfig), digestOfBytes(fixtureConfig),
		len(fixtureLayer), digestOfBytes(fixtureLayer))
}

// realRegistry returns a reference that exists and a client that can ask about
// it, or skips.
func realRegistry(t *testing.T) (string, manifestClient) {
	t.Helper()

	client := manifestClient{
		http:     &http.Client{Timeout: 30 * time.Second},
		insecure: os.Getenv("YACHT_TEST_REGISTRY_INSECURE") == "1",
	}
	if user := os.Getenv("YACHT_TEST_REGISTRY_USERNAME"); user != "" {
		client.creds = &Credentials{
			Username: user,
			Password: os.Getenv("YACHT_TEST_REGISTRY_PASSWORD"),
			Insecure: client.insecure,
		}
	}

	if host := os.Getenv("YACHT_TEST_REGISTRY"); host != "" {
		// A writable registry: it is only plain HTTP that a local one is
		// usefully run as, and CI's is, so default to that rather than making
		// every caller say so.
		if os.Getenv("YACHT_TEST_REGISTRY_INSECURE") == "" {
			client.insecure = true
		}
		seedFixture(t, client, host)
		return host + "/" + fixtureRepository + ":fixture", client
	}
	if ref := os.Getenv("YACHT_TEST_REGISTRY_REF"); ref != "" {
		return ref, client
	}
	t.Skip("set YACHT_TEST_REGISTRY (a writable registry, seeded here) or " +
		"YACHT_TEST_REGISTRY_REF (an image that already exists) to run these")
	return "", client
}

// seedFixture pushes the fixture image, so the test has something of its own to
// ask about rather than depending on what somebody left in the registry.
//
// Pushed over the Distribution API rather than with the docker CLI, because
// requiring a container runtime to test an HTTP client is a dependency that
// eventually stops the test running at all.
func seedFixture(t *testing.T, client manifestClient, host string) {
	t.Helper()
	scheme := "https"
	if client.insecure {
		scheme = "http"
	}
	base := scheme + "://" + host

	waitForRegistry(t, client, base)
	for _, blob := range [][]byte{fixtureConfig, fixtureLayer} {
		pushBlob(t, client, base, blob)
	}

	manifest := fixtureManifest()
	req := newFixtureRequest(t, client, http.MethodPut,
		base+"/v2/"+fixtureRepository+"/manifests/fixture", manifest)
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	do(t, client, req, http.StatusCreated)
}

// waitForRegistry gives a registry that is still starting a moment to answer.
// A service container is up before it is listening, and a test that fails on
// that is a flaky test rather than a failing one.
func waitForRegistry(t *testing.T, client manifestClient, base string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; ; attempt++ {
		req := newFixtureRequest(t, client, http.MethodGet, base+"/v2/", nil)
		resp, err := client.do(req)
		if err == nil {
			resp.Body.Close() //nolint:errcheck
			// 401 counts: an authenticated registry that challenges is a
			// registry that is listening.
			if resp.StatusCode < 500 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer /v2/ within 30s (last error: %v)", base, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func pushBlob(t *testing.T, client manifestClient, base string, blob []byte) {
	t.Helper()
	start := newFixtureRequest(t, client, http.MethodPost,
		base+"/v2/"+fixtureRepository+"/blobs/uploads/", nil)
	resp := do(t, client, start, http.StatusAccepted)
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("the registry accepted an upload without saying where to put it")
	}

	// The Location may be relative to the registry, and is the one place the
	// registry gets to choose the URL.
	upload, err := resp.Request.URL.Parse(location)
	if err != nil {
		t.Fatalf("parse upload location %q: %v", location, err)
	}
	query := upload.Query()
	query.Set("digest", digestOfBytes(blob))
	upload.RawQuery = query.Encode()

	put := newFixtureRequest(t, client, http.MethodPut, upload.String(), blob)
	put.Header.Set("Content-Type", "application/octet-stream")
	do(t, client, put, http.StatusCreated)
}

func newFixtureRequest(
	t *testing.T, client manifestClient, method, url string, body []byte,
) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if client.creds != nil {
		req.SetBasicAuth(client.creds.Username, client.creds.Password)
	}
	return req
}

func do(t *testing.T, client manifestClient, req *http.Request, want int) *http.Response {
	t.Helper()
	resp, err := client.do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != want {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("%s %s answered %s, want %d: %s",
			req.Method, req.URL, resp.Status, want, body)
	}
	return resp
}

// A tag on a real registry resolves, and resolving the answer returns it.
//
// The second half is what makes the first half worth anything: a digest that
// cannot be asked about again is a digest nothing can be pinned to.
func TestARealRegistryAnswersWithADigestThatResolvesToItself(t *testing.T) {
	ref, client := realRegistry(t)

	parsed, err := parseReference(ref)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}

	digest, err := client.resolve(context.Background(), parsed)
	if err != nil {
		t.Fatalf("resolve %s: %v", parsed, err)
	}
	if !refDigestRE.MatchString(digest) {
		t.Fatalf("%s resolved to %q, which is not a digest", parsed, digest)
	}

	pinned := parsed
	pinned.tag, pinned.digest = "", digest
	again, err := client.resolve(context.Background(), pinned)
	if err != nil {
		t.Fatalf("resolve the digest %s returned: %v", digest, err)
	}
	if again != digest {
		t.Errorf("%s resolved to %q the second time", pinned, again)
	}
}

// The fixture resolves to the digest its bytes say it should.
//
// Only meaningful for the registry this test seeded, where what was pushed is
// known. It is the end-to-end version of the thing a fake cannot check: a real
// implementation stored these bytes, served them back under a real media type,
// and Yacht computed the same digest a puller would.
func TestARealRegistryAgreesWithTheDigestOfWhatWasPushed(t *testing.T) {
	if os.Getenv("YACHT_TEST_REGISTRY") == "" {
		t.Skip("set YACHT_TEST_REGISTRY to check resolution against a known fixture")
	}
	ref, client := realRegistry(t)

	parsed, err := parseReference(ref)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}
	digest, err := client.resolve(context.Background(), parsed)
	if err != nil {
		t.Fatalf("resolve %s: %v", parsed, err)
	}
	if want := digestOfBytes(fixtureManifest()); digest != want {
		t.Errorf("%s resolved to %s, want %s — the digest of the manifest pushed",
			parsed, digest, want)
	}
}

// An absent tag on a repository that exists is an answer.
//
// Against a real registry rather than a fake, because this is the case ticket 8
// makes a correctness decision on: a build whose push succeeded before Yacht
// stopped is recovered by resolving its tag, and a build whose push did not
// must fail here rather than look temporarily unavailable.
func TestARealRegistryDistinguishesAnAbsentTagFromAFailure(t *testing.T) {
	ref, client := realRegistry(t)

	parsed, err := parseReference(ref)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}
	parsed.tag, parsed.digest = "yacht-tag-that-was-never-pushed", ""

	_, err = client.resolve(context.Background(), parsed)
	if !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("an absent tag is %v, want ErrManifestNotFound", err)
	}
	if errors.Is(err, ErrUnreachable) || errors.Is(err, ErrCredentialsRejected) {
		t.Errorf("an absent tag also reads as a failure to ask: %v", err)
	}
}

// A wrong password against a real registry is a credential answer.
//
// Only meaningful where a credential is configured at all: a public registry
// hands out anonymous pull tokens and would answer regardless.
func TestARealRegistryRejectsAWrongCredential(t *testing.T) {
	ref, client := realRegistry(t)
	if client.creds == nil {
		t.Skip("set YACHT_TEST_REGISTRY_USERNAME to test a rejected credential")
	}

	parsed, err := parseReference(ref)
	if err != nil {
		t.Fatalf("parse %q: %v", ref, err)
	}
	wrong := *client.creds
	wrong.Password = "not-the-password-" + strings.Repeat("x", 8)
	client.creds = &wrong

	if _, err := client.resolve(context.Background(), parsed); !errors.Is(
		err, ErrCredentialsRejected) {
		t.Fatalf("a wrong password is %v, want ErrCredentialsRejected", err)
	}
}
