package registry

// What a hostile registry can talk Yacht into.
//
// A WWW-Authenticate header is not a fact, it is input: over plain HTTP anyone
// on the path writes it, and over TLS it is written by whichever registry an
// app happens to point at — which is anyone with a public image. These tests
// are the ones that fail if that is ever forgotten again, so they are written
// as attacks rather than as cases.
//
// Their own file because they read as a set. A reviewer asking "what stops a
// challenge from sending the registry password somewhere else" should find the
// answer in one place rather than interleaved with the happy path.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// challengeTo returns a fake registry that answers everything with a bearer
// challenge pointing wherever the test says.
func challengeTo(t *testing.T, realm string) (string, manifestClient) {
	t.Helper()
	return fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm=%q,service="fake"`, realm))
		w.WriteHeader(http.StatusUnauthorized)
	})
}

// A realm that is not a place Yacht will ask is refused.
//
// The list is the shapes that turn a token exchange into something else: a
// scheme that is not HTTP at all, a URL carrying its own credentials, a
// relative reference resolved against who-knows-what, an authority that is not
// one. None of them is a thing a registry needs, and each of them is a thing
// somebody wants.
func TestARealmThatIsNotAPlaceYachtWillAskIsRefused(t *testing.T) {
	for _, tc := range []struct{ realm, why string }{
		{"", "no realm at all"},
		{"/token", "relative, so the authority comes from nowhere"},
		{"token", "not even a path"},
		{"file:///etc/passwd", "not an HTTP scheme"},
		{"gopher://internal:70/", "not an HTTP scheme"},
		{"https://", "no authority"},
		{"https://user:password@auth.example.com/token", "credentials in the URL"},
		{"mailto:someone@example.com", "opaque, not a location"},
	} {
		host, client := challengeTo(t, tc.realm)
		client.creds = &Credentials{Host: host, Username: "bot", Password: "hunter2"}

		_, err := resolveRef(t, client, host+"/acme/web:v1")
		if !errors.Is(err, ErrCredentialsRejected) {
			t.Errorf("realm %q (%s) gave %v, want ErrCredentialsRejected",
				tc.realm, tc.why, err)
		}
		if errors.Is(err, ErrManifestNotFound) {
			t.Errorf("realm %q (%s) reads as an absent image: %v", tc.realm, tc.why, err)
		}
	}
}

// A refused realm is never contacted at all.
//
// The rest of the realm tests assert on the error. This one asserts on the
// absence of a connection, which is the property that actually matters: the
// point of the policy is not that Yacht reports a problem afterwards, it is
// that a host named by a challenge does not get a request — no probe of an
// internal address, no credential, nothing.
func TestARefusedRealmIsNeverContacted(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("a refused realm was asked for a token: %s %s (auth %q)",
				r.Method, r.URL, r.Header.Get("Authorization"))
			fmt.Fprint(w, `{"token":"should-never-be-issued"}`)
		}))
	t.Cleanup(elsewhere.Close)

	// The registry is plain HTTP, so it may only issue tokens from its own
	// address — and this realm is a different authority that is very much
	// listening.
	host, client := challengeTo(t, elsewhere.URL+"/token")
	client.creds = &Credentials{Host: host, Username: "bot", Password: "hunter2"}

	_, err := resolveRef(t, client, host+"/acme/web:v1")
	if !errors.Is(err, ErrCredentialsRejected) {
		t.Fatalf("a cross-authority realm gave %v, want ErrCredentialsRejected", err)
	}
	if !strings.Contains(err.Error(), "own address") {
		t.Errorf("the error does not say why the realm was refused: %v", err)
	}
}

// A TLS registry cannot move the credential onto a plain-HTTP connection.
//
// The attack this closes is the cheapest one available: answer a perfectly
// ordinary HTTPS manifest request with a challenge naming an http:// realm, and
// the password arrives in clear text at a host of the challenger's choosing. A
// downgrade is never something a registry needs and always something somebody
// is doing on purpose.
func TestATLSRegistryCannotSendTheCredentialOverPlainHTTP(t *testing.T) {
	// insecure stays false, so the registry is treated as a TLS one whatever
	// the fake is actually listening on.
	client := manifestClient{http: http.DefaultClient}
	client.creds = &Credentials{Username: "bot", Password: "hunter2"}

	for _, realm := range []string{
		"http://auth.example.com/token",
		"http://127.0.0.1:9/token",
		"http://169.254.169.254/latest/meta-data/",
	} {
		u, err := client.realmFor(realm, reference{host: "registry.example.com", name: "acme/web"})
		if err == nil {
			t.Errorf("a plain-HTTP realm %q was accepted as %s", realm, u)
			continue
		}
		if !errors.Is(err, ErrCredentialsRejected) {
			t.Errorf("realm %q gave %v, want ErrCredentialsRejected", realm, err)
		}
	}
}

// A plain-HTTP registry may only issue tokens from its own address.
//
// Everything a plain-HTTP registry says is writable by anyone on the path, so
// the realm it names proves nothing. Restricting it to the registry's own
// authority means the worst an on-path attacker can do is send the credential
// to the host it was already going to — rather than to one they run, over
// HTTPS, where it keeps.
func TestAnInsecureRegistryCannotDelegateItsTokenEndpointElsewhere(t *testing.T) {
	client := manifestClient{http: http.DefaultClient, insecure: true}
	client.creds = &Credentials{Username: "bot", Password: "hunter2"}
	ref := reference{host: "registry.internal:5000", name: "acme/web", tag: "v1"}

	for _, realm := range []string{
		"http://attacker.example.com/token",
		"https://attacker.example.com/token",
		"http://registry.internal:5001/token",
		"http://169.254.169.254/latest/meta-data/",
	} {
		if _, err := client.realmFor(realm, ref); !errors.Is(err, ErrCredentialsRejected) {
			t.Errorf("realm %q gave %v, want ErrCredentialsRejected", realm, err)
		}
	}

	// Its own address is still fine, which is what keeps a local registry with
	// token auth working at all.
	for _, realm := range []string{
		"http://registry.internal:5000/token",
		"https://registry.internal:5000/token",
	} {
		if _, err := client.realmFor(realm, ref); err != nil {
			t.Errorf("realm %q on the registry's own address was refused: %v", realm, err)
		}
	}
}

// The delegation Docker Hub actually uses still works.
//
// This is the case the whole policy has to survive: registry-1.docker.io
// challenges to auth.docker.io, a different host entirely, and refusing that
// would mean no public image on Docker Hub ever resolves. GitLab's registry
// does the same thing to gitlab.com. Both are HTTPS to HTTPS, which is exactly
// the row of the table that stays open.
func TestCrossHostHTTPSDelegationStillWorks(t *testing.T) {
	client := manifestClient{http: http.DefaultClient}
	client.creds = &Credentials{Username: "bot", Password: "hunter2"}

	for _, tc := range []struct{ registry, realm string }{
		{"registry-1.docker.io", "https://auth.docker.io/token"},
		{"registry.gitlab.com", "https://gitlab.com/jwt/auth"},
		{"ghcr.io", "https://ghcr.io/token"},
		{"quay.io", "https://quay.io/v2/auth"},
	} {
		ref := reference{host: tc.registry, name: "acme/web", tag: "v1"}
		u, err := client.realmFor(tc.realm, ref)
		if err != nil {
			t.Errorf("%s delegating to %s was refused: %v", tc.registry, tc.realm, err)
			continue
		}
		if !client.mayCredential(u, ref) {
			t.Errorf("%s delegating to %s may not carry the credential", tc.registry, tc.realm)
		}
	}

	for _, tc := range []struct{ registry, realm string }{
		{"registry.example.com", "https://auth.attacker.example/token"},
		{"registry.victim.co.uk", "https://auth.attacker.co.uk/token"},
		{"registry.example.com", "https://169.254.169.254/token"},
	} {
		ref := reference{host: tc.registry, name: "acme/web", tag: "v1"}
		if _, err := client.realmFor(tc.realm, ref); !errors.Is(err, ErrCredentialsRejected) {
			t.Errorf("%s delegating to unrelated %s gave %v, want ErrCredentialsRejected",
				tc.registry, tc.realm, err)
		}
	}
}

func TestADelegatedRealmCannotResolveIntoTheInternalNetwork(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1",
		"169.254.169.254", "100.64.0.1", "::1", "fc00::1", "fe80::1",
	} {
		address := netip.MustParseAddr(raw)
		if publicRealmAddress(address) {
			t.Errorf("internal address %s may receive a delegated token request", address)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		address := netip.MustParseAddr(raw)
		if !publicRealmAddress(address) {
			t.Errorf("public address %s was refused", address)
		}
	}
}

// A redirect cannot walk the credential to another host.
//
// net/http would strip the Authorization header and follow the hop anyway,
// which turns a credentialled request into an anonymous one and reports the
// resulting 401 as if the password were wrong. Refusing says what happened.
func TestARedirectCannotCarryTheCredentialToAnotherHost(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the redirect was followed, carrying %q",
				r.Header.Get("Authorization"))
			serveManifest(w, testManifest)
		}))
	t.Cleanup(elsewhere.Close)

	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	client.creds = &Credentials{Host: host, Username: "bot", Password: "hunter2"}

	_, err := resolveRef(t, client, host+"/acme/web:v1")
	if err == nil {
		t.Fatal("a cross-host redirect carrying a credential was followed")
	}
	if errors.Is(err, ErrManifestNotFound) {
		t.Errorf("a refused redirect reads as an absent image: %v", err)
	}
}

// A redirect cannot drop TLS.
//
// Tested on the policy directly rather than through a server, because standing
// up a real HTTPS origin only to have it redirect to plain HTTP tests net/http
// more than it tests this.
func TestARedirectCannotDropTLS(t *testing.T) {
	from := func(rawurl string) *http.Request {
		u, err := url.Parse(rawurl)
		if err != nil {
			t.Fatalf("parse %q: %v", rawurl, err)
		}
		return &http.Request{URL: u, Header: http.Header{}}
	}

	secure := from("https://registry.example.com/v2/acme/web/manifests/v1")
	if err := refuseUnsafeRedirect(
		from("http://registry.example.com/v2/acme/web/manifests/v1"),
		[]*http.Request{secure},
	); err == nil {
		t.Error("a redirect from HTTPS to HTTP was allowed")
	}

	// Same authority over the same scheme is ordinary and stays allowed.
	if err := refuseUnsafeRedirect(
		from("https://registry.example.com/v2/acme/web/manifests/sha256-abc"),
		[]*http.Request{secure},
	); err != nil {
		t.Errorf("an ordinary same-host redirect was refused: %v", err)
	}

	// A scheme that is not HTTP at all.
	if err := refuseUnsafeRedirect(
		from("file:///etc/passwd"), []*http.Request{secure},
	); err == nil {
		t.Error("a redirect to a file:// URL was allowed")
	}

	// A default port and its explicit spelling are the same authority.
	withCredential := from("https://registry.example.com/v2/acme/web/manifests/v1")
	withCredential.Header.Set("Authorization", "Basic c2VjcmV0")
	if err := refuseUnsafeRedirect(
		from("https://registry.example.com:443/v2/acme/web/manifests/v1"),
		[]*http.Request{withCredential},
	); err != nil {
		t.Errorf("an explicit :443 was treated as another host: %v", err)
	}
	if err := refuseUnsafeRedirect(
		from("https://registry.example.com:8443/v2/acme/web/manifests/v1"),
		[]*http.Request{withCredential},
	); err == nil {
		t.Error("a redirect to another port carried the credential")
	}

	// A chain long enough to be somebody keeping Yacht busy.
	chain := make([]*http.Request, maxRedirects+1)
	for i := range chain {
		chain[i] = secure
	}
	if err := refuseUnsafeRedirect(secure, chain); err == nil {
		t.Error("an unbounded redirect chain was allowed")
	}
}

// A body that is not a manifest does not become a digest.
//
// Every one of these hashes perfectly well. That is the danger: the result is a
// real sha256 of the wrong thing, and a release pinned to it is wrong in a way
// nothing downstream can detect.
func TestOnlyAManifestMediaTypeBecomesADigest(t *testing.T) {
	for _, tc := range []struct{ contentType, body, why string }{
		{"text/html; charset=utf-8", "<html>sign in</html>", "a proxy's sign-in page"},
		{"", "", "an empty response from something in the middle"},
		{"application/json", `{"schemaVersion":2}`, "JSON that is not a manifest type"},
		{"application/vnd.docker.distribution.manifest.v1+prettyjws",
			`{"schemaVersion":1}`, "schema1, which Yacht does not pin"},
		{"application/vnd.oci.image.manifest.v1+json.evil", `{}`,
			"a media type that merely starts like one"},
		{"application/vnd.oci.image.config.v1+json", `{}`, "a config, not a manifest"},
	} {
		host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
			if tc.contentType != "" {
				w.Header().Set("Content-Type", tc.contentType)
			}
			fmt.Fprint(w, tc.body)
		})

		_, err := resolveRef(t, client, host+"/acme/web:v1")
		if err == nil {
			t.Errorf("%s was hashed into a digest", tc.why)
			continue
		}
		if !errors.Is(err, ErrUnreachable) {
			t.Errorf("%s gave %v, want ErrUnreachable", tc.why, err)
		}
		if errors.Is(err, ErrManifestNotFound) {
			t.Errorf("%s reads as an absent image: %v", tc.why, err)
		}
	}

	// The types a registry legitimately answers with are all accepted, with
	// parameters, so the check is on the media type rather than the header.
	for _, ct := range []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.index.v1+json; charset=utf-8",
		"APPLICATION/VND.OCI.IMAGE.INDEX.V1+JSON",
	} {
		if !isManifestType(ct) {
			t.Errorf("%q is not accepted as a manifest", ct)
		}
	}
}

// A cancelled lookup says so, without losing which kind of no it was.
//
// Both halves matter. The typed answer is what decides whether a deploy fails
// or retries; the cause is what tells an operator their own timeout fired
// rather than the registry being down.
func TestACancelledLookupKeepsBothItsTypeAndItsCause(t *testing.T) {
	release := make(chan struct{})
	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	parsed, err := parseReference(host + "/acme/web:v1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = client.resolve(ctx, parsed)
	close(release)

	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("a cancelled lookup is %v, want ErrUnreachable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation is not in the chain: %v", err)
	}
	if errors.Is(err, ErrManifestNotFound) {
		t.Errorf("a cancelled lookup reads as an absent image: %v", err)
	}
}

// A deadline is the same, and is the one an operator actually hits.
func TestALookupPastItsDeadlineKeepsBothItsTypeAndItsCause(t *testing.T) {
	release := make(chan struct{})
	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	parsed, err := parseReference(host + "/acme/web:v1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = client.resolve(ctx, parsed)
	close(release)

	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("a lookup past its deadline is %v, want ErrUnreachable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the deadline is not in the chain: %v", err)
	}
}

// Every status lands on one of the three answers.
//
// The ones that used to fall through are the point: a 400, a 3xx nobody
// followed, a 2xx that is not 200. An unclassified error escaping here would
// leave recovery after a crash between push and record deciding for itself
// which of the three it meant, and the wrong guess discards a pushed image.
func TestEveryStatusLandsOnOneOfTheThreeAnswers(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusBadRequest, ErrUnreachable},
		{http.StatusMethodNotAllowed, ErrUnreachable},
		{http.StatusNotAcceptable, ErrUnreachable},
		{http.StatusGone, ErrUnreachable},
		{http.StatusUnsupportedMediaType, ErrUnreachable},
		{http.StatusTeapot, ErrUnreachable},
		{http.StatusNoContent, ErrUnreachable},
		{http.StatusAccepted, ErrUnreachable},
		{http.StatusMultipleChoices, ErrUnreachable},
		{http.StatusNotImplemented, ErrUnreachable},
		{http.StatusRequestTimeout, ErrUnreachable},
		{http.StatusTooEarly, ErrUnreachable},
		{http.StatusTooManyRequests, ErrUnreachable},
		{http.StatusBadGateway, ErrUnreachable},
		{http.StatusInsufficientStorage, ErrUnreachable},
		{http.StatusForbidden, ErrCredentialsRejected},
		{http.StatusProxyAuthRequired, ErrCredentialsRejected},
		{http.StatusNotFound, ErrManifestNotFound},
	} {
		host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		})

		_, err := resolveRef(t, client, host+"/acme/web:v1")
		if !errors.Is(err, tc.want) {
			t.Errorf("%d gave %v, want %v", tc.status, err, tc.want)
		}
		// The asymmetry that matters: nothing but a genuine not-found may
		// read as one.
		if tc.want != ErrManifestNotFound && errors.Is(err, ErrManifestNotFound) {
			t.Errorf("%d reads as an absent image: %v", tc.status, err)
		}
	}
}

// A 404 is an absent image whatever its body says.
func TestA404IsAbsentEvenWhenItsBodySaysSomethingElse(t *testing.T) {
	host, client := fakeRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}`)
	})

	// Documented as it stands: the status wins, because a 404 is what every
	// registry means by absent and DENIED alongside it is not a shape any of
	// them actually sends. Recorded as a test so that if the classification
	// ever changes, it changes deliberately.
	_, err := resolveRef(t, client, host+"/acme/web:v1")
	if !errors.Is(err, ErrManifestNotFound) {
		t.Errorf("a 404 gave %v, want ErrManifestNotFound", err)
	}
}

// A challenge cannot be answered twice into a loop.
//
// A registry that answers every request with a fresh challenge would otherwise
// keep Yacht exchanging tokens for as long as it cared to.
func TestAChallengeCannotBeAnsweredForever(t *testing.T) {
	seen := &recorder{}
	attempts := 0

	var tokenHost string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		seen.set("tokens", fmt.Sprint(attempts))
		fmt.Fprintf(w, `{"token":"issued-%d"}`, attempts)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="http://%s/token",service="fake"`, tokenHost))
		w.WriteHeader(http.StatusUnauthorized)
	})

	host, client := fakeRegistry(t, mux.ServeHTTP)
	tokenHost = host
	client.creds = &Credentials{Host: host, Username: "bot", Password: "hunter2"}

	_, err := resolveRef(t, client, host+"/acme/web:v1")
	if !errors.Is(err, ErrCredentialsRejected) {
		t.Fatalf("an endless challenge gave %v, want ErrCredentialsRejected", err)
	}
	if got := seen.get("tokens"); got != "1" {
		t.Errorf("the token endpoint was asked %s times, want once", got)
	}
}

// The realm's own query is kept, and the scope is still Yacht's to set.
//
// Some registries put a required parameter in the realm itself. Rebuilding the
// URL from its parts would drop it, and the failure would only show up against
// that one registry.
func TestARealmKeepsItsOwnQuery(t *testing.T) {
	client := manifestClient{http: http.DefaultClient}
	ref := reference{host: "registry.example.com", name: "acme/web", tag: "v1"}

	u, err := client.realmFor("https://auth.example.com/token?account=bot", ref)
	if err != nil {
		t.Fatalf("realmFor: %v", err)
	}
	if u.Query().Get("account") != "bot" {
		t.Errorf("the realm's own query was dropped: %s", u)
	}
	if !strings.HasPrefix(u.String(), "https://auth.example.com/token?") {
		t.Errorf("the realm was rebuilt into %s", u)
	}
}
