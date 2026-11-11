package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// escapedE is "e" written as a JSON escape, \u0065: a key spelt with it is
// the same key once decoded.
const escapedE = `\u0065`

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// memLayout is a layout in memory. Open refuses what a layout reader in a pod
// refuses: anything missing, and anything over the limit.
type memLayout struct {
	files  map[string][]byte
	refuse map[string]error
	opened []string
}

func (m *memLayout) Open(_ context.Context, path string, limit int64) (io.ReadCloser, int64, error) {
	m.opened = append(m.opened, path)
	if err := m.refuse[path]; err != nil {
		return nil, 0, err
	}
	b, ok := m.files[path]
	if !ok {
		return nil, 0, fmt.Errorf("%s: no such file", path)
	}
	if int64(len(b)) > limit {
		return nil, 0, fmt.Errorf("%s is over %d bytes", path, limit)
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

// testImage is the parts of an image layout, assembled by layout().
type testImage struct {
	config    []byte
	layers    [][]byte
	manifest  map[string]any
	rawManif  []byte // when set, used as the manifest as it is
	indexMore []map[string]any
	indexType string
}

func defaultImage() *testImage {
	// An image whose configuration carries an ONBUILD trigger that would
	// print the credential if anything ran it.
	config := []byte(`{"architecture":"amd64","os":"linux",` +
		`"config":{"OnBuild":["RUN cat /proc/[0-9]*/root/registry/config.json | base64"],"onbuild":["RUN id"]},` +
		`"rootfs":{"type":"layers","diff_ids":[]}}`)
	layers := [][]byte{[]byte("layer one"), []byte("layer two, a little longer")}
	img := &testImage{config: config, layers: layers}
	var ls []map[string]any
	for _, l := range layers {
		ls = append(ls, map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    sha(l), "size": len(l),
			// Where a pusher that honours annotations would mount from.
			"annotations": map[string]string{
				"containerd.io/distribution.source.registry.test": "yacht/other-team",
			},
		})
	}
	img.manifest = map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociManifestType,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    sha(config), "size": len(config),
		},
		"layers": ls,
	}
	return img
}

// layout returns the layout's files and the manifest's digest.
func (img *testImage) layout(t *testing.T) (*memLayout, string, []byte) {
	t.Helper()
	raw := img.rawManif
	if raw == nil {
		var err error
		if raw, err = json.Marshal(img.manifest); err != nil {
			t.Fatal(err)
		}
	}
	digest := sha(raw)
	mt := img.indexType
	if mt == "" {
		mt = ociManifestType
	}
	entries := append([]map[string]any{{
		"mediaType": mt, "digest": digest, "size": len(raw),
		"annotations": map[string]string{"org.opencontainers.image.ref.name": "latest"},
	}}, img.indexMore...)
	index, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": entries})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"index.json":              index,
		blobPath(digest):          raw,
		blobPath(sha(img.config)): img.config,
	}
	for _, l := range img.layers {
		files[blobPath(sha(l))] = l
	}
	return &memLayout{files: files}, digest, raw
}

// pushRegistry is a distribution API that checks what a real one checks: the
// credential, and that a blob is what its digest says.
type pushRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	types     map[string]string
	requests  []string
	location  string // overrides the upload Location when set
	bearer    bool
	scopes    []string
}

const (
	testUser = "yacht"
	testPass = "registry-password"
)

func newPushRegistry() *pushRegistry {
	return &pushRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}, types: map[string]string{}}
}

func (f *pushRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())

	if r.URL.Path == "/token" {
		user, pass, ok := r.BasicAuth()
		if !ok || user != testUser || pass != testPass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		scope := r.URL.Query().Get("scope")
		f.scopes = append(f.scopes, scope)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "token-for:" + scope})
		return
	}

	scope := "repository:yacht/owner-web:pull"
	if r.Method != http.MethodHead && r.Method != http.MethodGet {
		scope += ",push"
	}
	if f.bearer {
		if r.Header.Get("Authorization") != "Bearer token-for:"+scope {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="http://%s/token",service="fake",scope=%q`, r.Host, scope))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	} else if user, pass, ok := r.BasicAuth(); !ok || user != testUser || pass != testPass {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	const prefix = "/v2/yacht/owner-web/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case r.Method == http.MethodHead && strings.HasPrefix(rest, "blobs/"):
		if _, ok := f.blobs[strings.TrimPrefix(rest, "blobs/")]; ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && rest == "blobs/uploads/":
		loc := "/v2/yacht/owner-web/blobs/uploads/u1?_state=kept"
		if f.location != "" {
			loc = f.location
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "blobs/uploads/"):
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		digest := r.URL.Query().Get("digest")
		if r.URL.Query().Get("_state") != "kept" || sha(body) != digest {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errors":[{"code":"DIGEST_INVALID"}]}`)
			return
		}
		f.blobs[digest] = body
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "manifests/"):
		body, _ := io.ReadAll(r.Body)
		tag := strings.TrimPrefix(rest, "manifests/")
		f.manifests[tag] = body
		f.types[tag] = r.Header.Get("Content-Type")
		w.Header().Set("Docker-Content-Digest", sha(body))
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func pushTo(t *testing.T, f *pushRegistry, layout LayoutFiles, digest string) error {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ref, err := parseReference(strings.TrimPrefix(srv.URL, "http://") + "/yacht/owner-web:d1")
	if err != nil {
		t.Fatal(err)
	}
	client := manifestClient{
		http:     srv.Client(),
		creds:    &Credentials{Host: ref.host, Username: testUser, Password: testPass},
		insecure: true, actions: "pull,push",
	}
	return pushLayout(context.Background(), client, ref, digest, layout, nil)
}

// A layout is pushed as the bytes it holds: every blob the manifest names,
// then the manifest itself, unchanged, by tag. An ONBUILD in the image's
// configuration is a string in a blob — pushed, never read — and the
// annotations naming another team's repository as a blob's source are never
// acted on: no request carries a mount.
func TestALayoutIsPushedAsDataAndNothingInItIsObeyed(t *testing.T) {
	img := defaultImage()
	layout, digest, raw := img.layout(t)
	f := newPushRegistry()
	// One layer is already in the repository, and is not sent again.
	f.blobs[sha(img.layers[0])] = img.layers[0]

	if err := pushTo(t, f, layout, digest); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.manifests["d1"], raw) || f.types["d1"] != ociManifestType {
		t.Errorf("the manifest was not pushed as it was: %s (%s)", f.manifests["d1"], f.types["d1"])
	}
	if !bytes.Equal(f.blobs[sha(img.config)], img.config) {
		t.Error("the configuration was not pushed byte for byte")
	}
	if !bytes.Equal(f.blobs[sha(img.layers[1])], img.layers[1]) {
		t.Error("a layer was not pushed")
	}
	for _, r := range f.requests {
		if strings.Contains(r, "mount=") || strings.Contains(r, "from=") {
			t.Errorf("a request mounts a blob: %s", r)
		}
		if strings.HasPrefix(r, "PUT /v2/yacht/owner-web/blobs/uploads/") &&
			strings.Contains(r, strings.TrimPrefix(sha(img.layers[0]), "sha256:")) {
			t.Errorf("a blob the repository had was sent again: %s", r)
		}
	}
	for _, p := range layout.opened {
		if p != "index.json" && !strings.HasPrefix(p, "blobs/sha256/") {
			t.Errorf("opened %s", p)
		}
	}
}

// The push asks for a token to push to the one repository it pushes to.
func TestThePushAsksForAPushTokenForItsOwnRepository(t *testing.T) {
	layout, digest, _ := defaultImage().layout(t)
	f := newPushRegistry()
	f.bearer = true
	if err := pushTo(t, f, layout, digest); err != nil {
		t.Fatal(err)
	}
	pushed := false
	for _, s := range f.scopes {
		if !strings.HasPrefix(s, "repository:yacht/owner-web:") {
			t.Errorf("asked for a token for %s", s)
		}
		pushed = pushed || strings.HasSuffix(s, "pull,push")
	}
	if !pushed {
		t.Errorf("never asked to push: %q", f.scopes)
	}
}

// Anything in a layout that is not exactly one image, each blob of it what
// its digest says, is refused — and refused before the manifest is pushed,
// so the tag is never moved to something half-checked.
func TestALayoutThatIsNotOneVerifiableImageIsRefused(t *testing.T) {
	foreign := func(img *testImage) {
		ls := img.manifest["layers"].([]map[string]any)
		ls[0]["urls"] = []string{"https://elsewhere.test/layer"}
	}
	for _, tc := range []struct {
		name   string
		change func(img *testImage)
		after  func(l *memLayout, digest string) string // may change the layout or the digest
	}{
		{"a second manifest in the index", func(img *testImage) {
			img.indexMore = []map[string]any{{"mediaType": ociManifestType, "digest": sha([]byte("x")), "size": 1}}
		}, nil},
		{"an index entry that is itself an index", func(img *testImage) {
			img.indexType = "application/vnd.oci.image.index.v1+json"
		}, nil},
		{"a different digest from the one the build made", nil, func(_ *memLayout, _ string) string {
			return sha([]byte("another manifest"))
		}},
		{"a digest that is not one", nil, func(*memLayout, string) string { return "sha256:../../registry" }},
		{"a layer whose content is not its digest", nil, func(l *memLayout, digest string) string {
			l.files[blobPath(sha([]byte("layer one")))] = []byte("layer 1!!")
			return digest
		}},
		{"a manifest whose content is not its digest", nil, func(l *memLayout, digest string) string {
			l.files[blobPath(digest)] = append([]byte(" "), l.files[blobPath(digest)]...)[:len(l.files[blobPath(digest)])]
			return digest
		}},
		{"a layer of another size than stated", func(img *testImage) {
			img.manifest["layers"].([]map[string]any)[1]["size"] = 3
		}, nil},
		{"a blob the layout reader refuses, as it refuses a symlink", nil, func(l *memLayout, digest string) string {
			l.refuse = map[string]error{blobPath(sha([]byte("layer one"))): errors.New("not a plain file")}
			return digest
		}},
		{"a foreign layer fetched from elsewhere", foreign, nil},
		{"a non-distributable layer", func(img *testImage) {
			img.manifest["layers"].([]map[string]any)[0]["mediaType"] =
				"application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"
		}, nil},
		{"a digest of another algorithm", func(img *testImage) {
			img.manifest["layers"].([]map[string]any)[0]["digest"] = "sha512:" + strings.Repeat("a", 128)
		}, nil},
		{"a configuration too large to be one", func(img *testImage) {
			img.manifest["config"].(map[string]any)["size"] = configLimit + 1
		}, nil},
		{"layers named twice, once escaped", func(img *testImage) {
			raw, _ := json.Marshal(img.manifest)
			img.rawManif = bytes.Replace(raw, []byte(`"layers":`), []byte(`"layers":[],"lay`+escapedE+`rs":`), 1)
		}, nil},
		{"layers spelt in another case", func(img *testImage) {
			raw, _ := json.Marshal(img.manifest)
			img.rawManif = bytes.Replace(raw, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"Layers":[]`), 1)
		}, nil},
		{"a digest key spelt in another case", func(img *testImage) {
			img.manifest["config"].(map[string]any)["Digest"] = sha([]byte("x"))
		}, nil},
		{"a manifest that says it is something else", func(img *testImage) {
			img.manifest["mediaType"] = dockerManifestType
		}, nil},
		{"a manifest that is an index", func(img *testImage) {
			img.manifest["manifests"] = []any{}
		}, nil},
		{"trailing data after the manifest", func(img *testImage) {
			raw, _ := json.Marshal(img.manifest)
			img.rawManif = append(raw, []byte(` {"layers":[]}`)...)
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := defaultImage()
			if tc.change != nil {
				tc.change(img)
			}
			layout, digest, _ := img.layout(t)
			if tc.after != nil {
				digest = tc.after(layout, digest)
			}
			f := newPushRegistry()
			err := pushTo(t, f, layout, digest)
			if err == nil {
				t.Fatal("pushed")
			}
			if !errors.Is(err, ErrBadLayout) {
				t.Errorf("err = %v, want ErrBadLayout", err)
			}
			if len(f.manifests) != 0 {
				t.Errorf("the manifest was pushed: %v", f.requests)
			}
		})
	}
}

// The registry says where to send a blob. The PUT carries the credential, so
// a Location on any other address is refused rather than sent to.
func TestAnUploadIsNotSentOffTheRegistry(t *testing.T) {
	layout, digest, _ := defaultImage().layout(t)
	for _, loc := range []string{
		"http://elsewhere.test/v2/yacht/owner-web/blobs/uploads/u1",
		"http://yacht:pw@127.0.0.1:1/upload",
	} {
		f := newPushRegistry()
		f.location = loc
		err := pushTo(t, f, layout, digest)
		if err == nil || !strings.Contains(err.Error(), "refused to send a blob") {
			t.Errorf("%s: err = %v", loc, err)
		}
		if len(f.manifests) != 0 {
			t.Errorf("%s: the manifest was pushed", loc)
		}
	}
}

// strictObject refuses the JSON that two parsers could read two ways.
func TestStrictObject(t *testing.T) {
	for raw, ok := range map[string]bool{
		`{"layers":[]}`:                            true,
		`{"layers":[],"annotations":{}}`:           true,
		`{"layers":[],"layers":[]}`:                false,
		`{"layers":[],"lay` + escapedE + `rs":[]}`: false,
		`{"Layers":[]}`:                            false,
		`{"LAYERS":[]}`:                            false,
		`{"layers":[]} {}`:                         false,
		`["layers"]`:                               false,
		`{"layers":[]`:                             false,
	} {
		_, err := strictObject([]byte(raw), "x", "layers")
		if (err == nil) != ok {
			t.Errorf("%s: err = %v", raw, err)
		}
	}
}
