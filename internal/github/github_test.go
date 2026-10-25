package github

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

// clock is a settable now, for the token cache and the state's expiry.
type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(c.t.Load(), 0) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d / time.Second)) }

func newTestApp(t *testing.T, h http.Handler) (*App, *clock) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := &clock{}
	c.t.Store(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix())
	a, err := New(Config{
		AppID: 42, Slug: "yacht-test", ClientID: "Iv1.client", ClientSecret: "client-secret",
		PrivateKey: testKey, WebhookSecret: "hook-secret",
		APIURL: srv.URL, WebURL: srv.URL, Now: c.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, c
}

// verifyJWT checks a JWT the way GitHub does: RS256 over the App's key, the
// App id as issuer, and a lifetime GitHub accepts.
func verifyJWT(t *testing.T, raw string, now time.Time) {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h map[string]string
	if err := json.Unmarshal(header, &h); err != nil || h["alg"] != "RS256" || h["typ"] != "JWT" {
		t.Fatalf("JWT header = %s, want RS256 JWT", header)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("JWT signature does not verify: %v", err)
	}
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	if c.Iss != "42" {
		t.Errorf("iss = %q, want the App id", c.Iss)
	}
	if got := now.Unix() - c.Iat; got != 60 {
		t.Errorf("iat is %ds before now, want 60", got)
	}
	if c.Exp <= now.Unix() || c.Exp-c.Iat > 600 {
		t.Errorf("exp-iat = %ds, want in the future and within GitHub's ten minutes", c.Exp-c.Iat)
	}
}

func TestJWTIsWhatGitHubAccepts(t *testing.T) {
	a, c := newTestApp(t, http.NotFoundHandler())
	raw, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	verifyJWT(t, raw, c.now())
}

func TestParsePrivateKeyReadsBothForms(t *testing.T) {
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)})
	der, _ := x509.MarshalPKCS8PrivateKey(testKey)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, data := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		k, err := ParsePrivateKey(data)
		if err != nil || !k.Equal(testKey) {
			t.Errorf("%s: got %v, %v", name, k != nil, err)
		}
	}
	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Error("accepted something that is not PEM")
	}
}

// A build's token reaches one repository, read-only, by the id that
// survives a rename — and is minted once, not per build.
func TestRepoTokenIsScopedAndCached(t *testing.T) {
	var mints atomic.Int32
	var a *App
	var c *clock
	a, c = newTestApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/7/access_tokens" {
			http.NotFound(w, r)
			return
		}
		jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			t.Errorf("minted without the App's JWT")
		}
		verifyJWT(t, jwt, c.now())
		var body struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Repositories  []string          `json:"repositories"`
			Permissions   map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 99 {
			t.Errorf("repository_ids = %v, want [99]", body.RepositoryIDs)
		}
		if body.Permissions["contents"] != "read" || len(body.Permissions) != 2 {
			t.Errorf("permissions = %v, want contents and metadata read and nothing else", body.Permissions)
		}
		n := mints.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q,"repositories":[{"id":99,"full_name":"acme/web"}]}`,
			n, c.now().Add(time.Hour).Format(time.RFC3339))
	}))

	tok, err := a.RepoToken(t.Context(), 7, 99)
	if err != nil || tok.Value != "ghs_1" {
		t.Fatalf("token = %q, %v", tok.Value, err)
	}
	c.advance(30 * time.Minute)
	if tok, _ = a.RepoToken(t.Context(), 7, 99); tok.Value != "ghs_1" || mints.Load() != 1 {
		t.Fatalf("a token with half an hour left was minted again (%q, %d mints)", tok.Value, mints.Load())
	}
	// Close to expiry it is replaced rather than handed to a clone that may
	// outlive it.
	c.advance(25 * time.Minute)
	if tok, _ = a.RepoToken(t.Context(), 7, 99); tok.Value != "ghs_2" {
		t.Fatalf("a token five minutes from expiry was reused: %q", tok.Value)
	}
	// And forgotten when GitHub says the installation was suspended.
	a.Forget(7)
	if tok, _ = a.RepoToken(t.Context(), 7, 99); tok.Value != "ghs_3" {
		t.Fatalf("a forgotten installation's token was reused: %q", tok.Value)
	}
	repo, err := a.Repository(t.Context(), 7, 99)
	if err != nil || repo.FullName != "acme/web" {
		t.Fatalf("repository = %+v, %v", repo, err)
	}
}

func TestTokenRefusalsSayWhichKind(t *testing.T) {
	cases := map[int64]struct {
		status int
		body   string
		want   error
	}{
		1: {http.StatusNotFound, `{"message":"Not Found"}`, ErrInstallationGone},
		2: {http.StatusForbidden, `{"message":"This installation has been suspended"}`, ErrInstallationSuspended},
		3: {http.StatusUnprocessableEntity, `{"message":"There is at least one repository that does not exist or is not accessible"}`, ErrRepoNotAccessible},
	}
	a, _ := newTestApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmt.Sscanf(r.URL.Path, "/app/installations/%d/access_tokens", &id)
		c := cases[id]
		w.WriteHeader(c.status)
		io.WriteString(w, c.body)
	}))
	for id, c := range cases {
		if _, err := a.RepoToken(t.Context(), id, 5); !errors.Is(err, c.want) {
			t.Errorf("installation %d: err = %v, want %v", id, err, c.want)
		}
	}
}

// The repository list is every page, and a next link to anywhere but the
// API is not followed: the token would go with it.
func TestRepositoriesFollowsPagesOnTheAPIOnly(t *testing.T) {
	var srvURL string
	a, _ := newTestApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/7/access_tokens":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, scoped := body["repository_ids"]; scoped {
				t.Errorf("the list token was scoped to repositories: %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_list","expires_at":"2099-01-01T00:00:00Z"}`)
		case r.URL.Path == "/installation/repositories":
			if r.Header.Get("Authorization") != "token ghs_list" {
				t.Errorf("listed with %q", r.Header.Get("Authorization"))
			}
			switch r.URL.Query().Get("page") {
			case "":
				w.Header().Set("Link", `<`+srvURL+`/installation/repositories?per_page=100&page=2>; rel="next", <`+srvURL+`/installation/repositories?page=3>; rel="last"`)
				io.WriteString(w, `{"total_count":3,"repositories":[{"id":1,"full_name":"acme/a"},{"id":2,"full_name":"acme/b"}]}`)
			case "2":
				w.Header().Set("Link", `<https://elsewhere.example/steal?page=3>; rel="next"`)
				io.WriteString(w, `{"total_count":3,"repositories":[{"id":3,"full_name":"acme/c","private":true}]}`)
			default:
				t.Errorf("read page %q", r.URL.Query().Get("page"))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	srvURL = a.cfg.APIURL
	repos, err := a.Repositories(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 || repos[2].FullName != "acme/c" || !repos[2].Private {
		t.Fatalf("repositories = %+v", repos)
	}
}

func TestBranchesUseTheRepositorysOwnToken(t *testing.T) {
	var srvURL string
	a, _ := newTestApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"token":"ghs_repo","expires_at":"2099-01-01T00:00:00Z","repositories":[{"id":99,"full_name":"acme/web"}]}`)
		case "/repos/acme/web/branches":
			if r.Header.Get("Authorization") != "token ghs_repo" {
				t.Errorf("branches read with %q", r.Header.Get("Authorization"))
			}
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", `<`+srvURL+`/repos/acme/web/branches?page=2>; rel="next"`)
				io.WriteString(w, `[{"name":"main"},{"name":"dev"}]`)
				return
			}
			io.WriteString(w, `[{"name":"feature/x"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	srvURL = a.cfg.APIURL
	got, err := a.Branches(t.Context(), 7, 99)
	if err != nil || strings.Join(got, ",") != "main,dev,feature/x" {
		t.Fatalf("branches = %v, %v", got, err)
	}
}

// The installations a person reaches are GitHub's answer to their own
// token, bought with the code — and a code GitHub refuses proves nothing.
func TestUserInstallationsExchangesTheCode(t *testing.T) {
	var srvURL string
	a, _ := newTestApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_ = r.ParseForm()
			if r.Form.Get("client_id") != "Iv1.client" || r.Form.Get("client_secret") != "client-secret" {
				t.Errorf("exchanged without the App's client credentials: %v", r.Form)
			}
			if r.Form.Get("code") != "good" {
				// GitHub's own answer to a bad code: 200, with an error.
				io.WriteString(w, `{"error":"bad_verification_code"}`)
				return
			}
			io.WriteString(w, `{"access_token":"ghu_user","expires_in":28800,"token_type":"bearer"}`)
		case "/user":
			io.WriteString(w, `{"login":"octocat"}`)
		case "/user/installations":
			if r.Header.Get("Authorization") != "Bearer ghu_user" {
				t.Errorf("listed with %q", r.Header.Get("Authorization"))
			}
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", `<`+srvURL+`/user/installations?page=2>; rel="next"`)
				io.WriteString(w, `{"total_count":3,"installations":[{"id":7,"app_id":42,"account":{"login":"acme","type":"Organization"}}]}`)
				return
			}
			io.WriteString(w, `{"total_count":3,"installations":[{"id":8,"app_id":42,"account":{"login":"octocat","type":"User"}},{"id":9,"app_id":1,"account":{"login":"other","type":"User"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	srvURL = a.cfg.APIURL

	if _, _, err := a.UserInstallations(t.Context(), "spent"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a refused code: err = %v, want ErrNotAuthorized", err)
	}
	login, ins, err := a.UserInstallations(t.Context(), "good")
	if err != nil {
		t.Fatal(err)
	}
	if login != "octocat" || len(ins) != 2 || ins[0].ID != 7 || ins[1].ID != 8 {
		t.Fatalf("login %q, installations %+v — want octocat's two of this App's", login, ins)
	}
}

// A GitHub that never answers is told apart from one that refused, and the
// log learns why.
func TestUnreachableGitHubKeepsTheCause(t *testing.T) {
	a, _ := newTestApp(t, http.NotFoundHandler())
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	a.cfg.APIURL, a.cfg.WebURL = dead.URL, dead.URL

	_, _, err := a.UserInstallations(t.Context(), "good")
	if !errors.Is(err, ErrUnreachable) || errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if !strings.Contains(err.Error(), "finish signing in") || !strings.Contains(err.Error(), "connect") {
		t.Errorf("err = %q, want what failed on the way", err)
	}
	if _, err := a.Installation(t.Context(), 7); !errors.Is(err, ErrUnreachable) {
		t.Errorf("an API call: err = %v, want ErrUnreachable", err)
	}
}

func TestStateIsBoundAndExpires(t *testing.T) {
	a, c := newTestApp(t, http.NotFoundHandler())
	raw := a.SignState("team-1", "user-1")
	s, err := a.OpenState(raw, StateLink)
	if err != nil || s.Team != "team-1" || s.User != "user-1" {
		t.Fatalf("state = %+v, %v", s, err)
	}
	// A choice is not a link, nor the other way round.
	if _, err := a.OpenState(raw, StateChoice); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("a link opened as a choice: %v", err)
	}
	choice := a.SignChoice("team-1", "user-1", 7, "octocat")
	if s, err := a.OpenState(choice, StateChoice); err != nil || s.Installation != 7 || s.Login != "octocat" {
		t.Errorf("choice = %+v, %v", s, err)
	}
	if _, err := a.OpenState(choice, StateLink); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("a choice opened as a link: %v", err)
	}

	// Another team's name in the payload, under the old signature.
	payload, sig, _ := strings.Cut(raw, ".")
	body, _ := base64.RawURLEncoding.DecodeString(payload)
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(body), "team-1", "team-2", 1)))
	if _, err := a.OpenState(forged+"."+sig, StateLink); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("an altered state opened: %v", err)
	}
	// Signed by an install with another client secret.
	other, _ := New(Config{AppID: 42, Slug: "yacht-test", ClientID: "x", ClientSecret: "other",
		PrivateKey: testKey, WebhookSecret: "x", Now: c.now})
	if _, err := a.OpenState(other.SignState("team-1", "user-1"), StateLink); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("another install's state opened: %v", err)
	}
	c.advance(31 * time.Minute)
	if _, err := a.OpenState(raw, StateLink); !errors.Is(err, ErrStateInvalid) {
		t.Errorf("an expired state opened: %v", err)
	}
}

func TestVerifyChecksTheSignature(t *testing.T) {
	a, _ := newTestApp(t, http.NotFoundHandler())
	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte("hook-secret"))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !a.Verify(good, body) {
		t.Fatal("a correctly signed delivery was refused")
	}
	for name, sig := range map[string]string{
		"no prefix":   strings.TrimPrefix(good, "sha256="),
		"empty":       "",
		"other body":  good[:len(good)-2] + "00",
		"not hex":     "sha256=zz",
		"sha1 header": "sha1=" + strings.TrimPrefix(good, "sha256="),
	} {
		if a.Verify(sig, body) {
			t.Errorf("%s: accepted", name)
		}
	}
	if a.Verify(good, append(body, ' ')) {
		t.Error("a signature over another body was accepted")
	}
}

func TestInstallURLCarriesTheState(t *testing.T) {
	a, _ := newTestApp(t, http.NotFoundHandler())
	got := a.InstallURL("a.b")
	if !strings.HasSuffix(got, "/apps/yacht-test/installations/new?state=a.b") {
		t.Errorf("install URL = %q", got)
	}
}
