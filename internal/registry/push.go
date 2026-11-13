package registry

// Pushing an image a Dockerfile build left as an OCI layout.
//
// Here, in the engine, rather than in the build. The build runs a team's
// Dockerfile, and anything in the build's pod that holds the registry
// credential is something that Dockerfile can reach: a RUN line under
// BuildKit's no-process-sandbox mode sees every process in its container, and
// a FROM of the layout runs the image's ONBUILD triggers. The registry
// account holds every team's images. So the build writes the image into a
// layout and stops, and the engine — which already holds the credential to
// resolve digests — reads the layout out of the pod and pushes it, executing
// nothing from it.
//
// Written against the distribution API directly, as manifest resolution is:
// a HEAD, a POST and a PUT a blob, and a PUT for the manifest, which is not
// worth a client library to keep updated forever.
//
// The layout is the team's own data and is treated as hostile. Nothing in it
// is followed or obeyed: no symlink (the reader refuses anything that is not a
// plain file), no annotation (a containerd.io/distribution.source label is
// how a pusher is talked into mounting a blob from another repository, and
// this one never sends a mount at all), no URL (a foreign layer is refused,
// not fetched), and no image configuration, which is pushed as the bytes it
// is and never read. Every blob is hashed on its way out and refused if it is
// not what its digest says.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// LayoutFiles is an OCI image layout somewhere the engine cannot open
// directly — inside a build's pod.
type LayoutFiles interface {
	// Open returns the file at path inside the layout, which is
	// "index.json" or "blobs/sha256/<hex>", and its size. It refuses
	// anything that is not a regular file and anything larger than limit,
	// so a symlink in the layout is never followed and a huge file is never
	// read.
	Open(ctx context.Context, path string, limit int64) (io.ReadCloser, int64, error)
}

// ErrBadLayout means the layout a build left is not an image Yacht will push.
var ErrBadLayout = errors.New("registry: the built image is not one Yacht will push")

// concurrentPushes is how many images one engine pushes at once. A push
// waiting for its turn waits within its own deadline, which the caller sets.
const concurrentPushes = 2

// emptyDigest is the digest of no bytes, the only one a zero-length blob may
// have.
const emptyDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// The media types a build's image may be made of: OCI's and Docker's image
// configuration, and their layer types, uncompressed, gzip and zstd. Anything
// else — an artifact, a foreign or non-distributable layer — is not what a
// Dockerfile build makes, and is refused rather than pushed as if it were.
var (
	configTypes = map[string]bool{
		"application/vnd.oci.image.config.v1+json":       true,
		"application/vnd.docker.container.image.v1+json": true,
	}
	layerTypes = map[string]bool{
		"application/vnd.oci.image.layer.v1.tar":            true,
		"application/vnd.oci.image.layer.v1.tar+gzip":       true,
		"application/vnd.oci.image.layer.v1.tar+zstd":       true,
		"application/vnd.docker.image.rootfs.diff.tar":      true,
		"application/vnd.docker.image.rootfs.diff.tar.gzip": true,
		"application/vnd.docker.image.rootfs.diff.tar.zstd": true,
	}
)

// Bounds on what a layout may hold. A build's image is the team's to make as
// large as they like within reason; these are the reason.
const (
	indexLimit  = 1 << 20
	configLimit = 8 << 20
	// A blob is read through tar, and plain tar headers hold sizes below
	// 8 GiB.
	blobLimit  = 8<<30 - 1
	imageLimit = 32 << 30
	layerLimit = 512
)

// The manifest types an image layout's one entry may be. An index is refused:
// a build makes one image for one platform, and an index is a way to make the
// push carry more than that.
const (
	ociManifestType    = "application/vnd.oci.image.manifest.v1+json"
	dockerManifestType = "application/vnd.docker.distribution.manifest.v2+json"
)

// descriptor is the part of an OCI descriptor this reads. Everything else —
// annotations above all — is not read, so it cannot be obeyed.
type descriptor struct {
	MediaType string
	Digest    string
	Size      int64
	URLs      []string
}

// PushLayout pushes the one image in a build's layout to image, which must be
// a tag in this install's registry. digest is the manifest the build said it
// built; the layout must hold exactly that one.
func (s *Store) PushLayout(
	ctx context.Context, image, digest string, layout LayoutFiles, logf func(string),
) error {
	ref, err := parseReference(image)
	if err != nil {
		return err
	}
	if ref.digest != "" {
		return fmt.Errorf("registry: a build pushes to a tag, not to %s", image)
	}
	set, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	// A build's image is named by the engine, but the push holds the
	// install's credential, so where it goes is checked here, where the
	// credential is.
	if !ownHost(set, ref) || !inRepository(set, ref) {
		return fmt.Errorf("registry: %s is not in this install's registry, so it is not pushed", image)
	}
	creds, err := s.Credentials(ctx)
	if err != nil {
		return err
	}
	// A client of its own: the resolver's has a 30-second timeout, which is a
	// manifest's lifetime, not a layer's. The push's own deadline bounds it.
	client := manifestClient{
		http:     &http.Client{Transport: s.http.Transport},
		creds:    &creds,
		insecure: set.Insecure,
		actions:  "pull,push",
	}
	release, err := s.takePushSlot(ctx, image, logf)
	if err != nil {
		return err
	}
	defer release()
	return pushLayout(ctx, client, ref, digest, layout, logf)
}

// takePushSlot waits for one of the engine's push slots, saying so when it
// has to wait, and returns how to give it back.
func (s *Store) takePushSlot(ctx context.Context, image string, logf func(string)) (func(), error) {
	release := func() { <-s.pushes }
	select {
	case s.pushes <- struct{}{}:
		return release, nil
	default:
	}
	if logf != nil {
		logf("Waiting for another image to finish pushing.\n")
	}
	select {
	case s.pushes <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("registry: waiting to push %s: %w", image, ctx.Err())
	}
}

// inRepository reports whether a reference is an image of this install's,
// which is one path segment under the configured repository: the path
// ImageRef builds. The credential may reach further than that — one account
// holds every team's images — and the push is where that is stopped.
func inRepository(set Settings, ref reference) bool {
	name := ref.name
	if set.Repository != "" {
		var ok bool
		if name, ok = strings.CutPrefix(name, set.Repository+"/"); !ok {
			return false
		}
	} else if ref.host == dockerHubHost {
		// Docker Hub's library/ is what parseReference adds to a one-segment
		// name, which is the only kind an empty repository makes.
		name = strings.TrimPrefix(name, "library/")
	}
	return name != "" && !strings.Contains(name, "/")
}

func pushLayout(
	ctx context.Context, client manifestClient, ref reference, digest string,
	layout LayoutFiles, logf func(string),
) error {
	if logf == nil {
		logf = func(string) {}
	}
	if !refDigestRE.MatchString(digest) {
		return fmt.Errorf("%w: the build did not say which image it built", ErrBadLayout)
	}

	entry, err := readIndex(ctx, layout, digest)
	if err != nil {
		return err
	}
	raw, err := readSmallBlob(ctx, layout, entry.Digest, entry.Size, manifestLimit)
	if err != nil {
		return err
	}
	config, layers, err := parseManifest(raw, entry.MediaType)
	if err != nil {
		return err
	}

	p := pusher{client: client, ref: ref}
	blobs := append([]descriptor{config}, layers...)
	for i, b := range blobs {
		what := fmt.Sprintf("layer %d of %d", i, len(layers))
		if i == 0 {
			what = "configuration"
		}
		if err := p.pushBlob(ctx, layout, b, what, logf); err != nil {
			return err
		}
	}
	if err := p.putManifest(ctx, raw, entry.MediaType, digest); err != nil {
		return err
	}
	logf("Pushed " + ref.String() + "@" + digest + "\n")
	return nil
}

// readIndex reads index.json and returns its one entry, which must be the
// manifest the build said it built.
func readIndex(ctx context.Context, layout LayoutFiles, digest string) (descriptor, error) {
	rc, _, err := layout.Open(ctx, "index.json", indexLimit)
	if err != nil {
		return descriptor{}, fmt.Errorf("%w: index.json: %w", ErrBadLayout, err)
	}
	raw, err := io.ReadAll(io.LimitReader(rc, indexLimit+1))
	rc.Close() //nolint:errcheck
	if err != nil {
		return descriptor{}, fmt.Errorf("%w: index.json: %w", ErrBadLayout, err)
	}
	if len(raw) > indexLimit {
		return descriptor{}, fmt.Errorf("%w: index.json is larger than %d bytes", ErrBadLayout, indexLimit)
	}

	fields, err := strictObject(raw, "index.json", "schemaVersion", "mediaType", "manifests")
	if err != nil {
		return descriptor{}, err
	}
	if err := schemaVersion2(fields, "index.json"); err != nil {
		return descriptor{}, err
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(fields["manifests"], &entries); err != nil {
		return descriptor{}, fmt.Errorf("%w: index.json has no list of manifests", ErrBadLayout)
	}
	if len(entries) != 1 {
		return descriptor{}, fmt.Errorf("%w: index.json names %d manifests, and a build makes one",
			ErrBadLayout, len(entries))
	}
	entry, err := parseDescriptor(entries[0], "index.json's manifest")
	if err != nil {
		return descriptor{}, err
	}
	if entry.Digest != digest {
		return descriptor{}, fmt.Errorf("%w: index.json names %s, not the %s the build made",
			ErrBadLayout, entry.Digest, digest)
	}
	switch entry.MediaType {
	case ociManifestType, dockerManifestType:
	default:
		return descriptor{}, fmt.Errorf("%w: index.json's manifest is a %q, not an image manifest",
			ErrBadLayout, entry.MediaType)
	}
	return entry, nil
}

// parseManifest returns an image manifest's config and layers.
func parseManifest(raw []byte, mediaType string) (descriptor, []descriptor, error) {
	fields, err := strictObject(raw, "the manifest", "schemaVersion", "mediaType", "config", "layers", "manifests", "subject")
	if err != nil {
		return descriptor{}, nil, err
	}
	if err := schemaVersion2(fields, "the manifest"); err != nil {
		return descriptor{}, nil, err
	}
	if _, ok := fields["manifests"]; ok {
		return descriptor{}, nil, fmt.Errorf("%w: the manifest is an index", ErrBadLayout)
	}
	// A subject makes the manifest a referrer of another image, which a
	// registry indexes against that image — possibly another team's. A build
	// makes an image, not a statement about one.
	if _, ok := fields["subject"]; ok {
		return descriptor{}, nil, fmt.Errorf("%w: the manifest names a subject", ErrBadLayout)
	}
	if mt, ok := fields["mediaType"]; ok {
		var stated string
		if json.Unmarshal(mt, &stated) != nil || stated != mediaType {
			return descriptor{}, nil, fmt.Errorf("%w: the manifest says it is %s, and index.json says %s",
				ErrBadLayout, mt, mediaType)
		}
	} else if mediaType == dockerManifestType {
		return descriptor{}, nil, fmt.Errorf("%w: a Docker manifest without its media type", ErrBadLayout)
	}

	cfgRaw, ok := fields["config"]
	if !ok {
		return descriptor{}, nil, fmt.Errorf("%w: the manifest has no config", ErrBadLayout)
	}
	config, err := parseDescriptor(cfgRaw, "the config")
	if err != nil {
		return descriptor{}, nil, err
	}
	if config.Size > configLimit {
		return descriptor{}, nil, fmt.Errorf("%w: the config is larger than %d bytes", ErrBadLayout, configLimit)
	}
	if !configTypes[config.MediaType] {
		return descriptor{}, nil, fmt.Errorf("%w: the config is a %q, not an image configuration",
			ErrBadLayout, config.MediaType)
	}

	var rawLayers []json.RawMessage
	if l, ok := fields["layers"]; !ok || json.Unmarshal(l, &rawLayers) != nil {
		return descriptor{}, nil, fmt.Errorf("%w: the manifest has no list of layers", ErrBadLayout)
	}
	if len(rawLayers) > layerLimit {
		return descriptor{}, nil, fmt.Errorf("%w: %d layers, more than %d", ErrBadLayout, len(rawLayers), layerLimit)
	}
	total := config.Size
	layers := make([]descriptor, 0, len(rawLayers))
	for i, l := range rawLayers {
		d, err := parseDescriptor(l, fmt.Sprintf("layer %d", i+1))
		if err != nil {
			return descriptor{}, nil, err
		}
		// A foreign layer is one the puller fetches from somewhere else; a
		// build has no business making one, and pushing one would be pushing
		// a pointer rather than the image.
		if len(d.URLs) > 0 || !layerTypes[d.MediaType] {
			return descriptor{}, nil, fmt.Errorf("%w: layer %d is a %q, not a layer Yacht pushes",
				ErrBadLayout, i+1, d.MediaType)
		}
		total += d.Size
		if total > imageLimit {
			return descriptor{}, nil, fmt.Errorf("%w: the image is larger than %d bytes", ErrBadLayout, imageLimit)
		}
		layers = append(layers, d)
	}
	return config, layers, nil
}

// parseDescriptor reads one descriptor: a sha256 digest, a size within
// bounds, a media type.
func parseDescriptor(raw json.RawMessage, what string) (descriptor, error) {
	fields, err := strictObject(raw, what, "mediaType", "digest", "size", "urls")
	if err != nil {
		return descriptor{}, err
	}
	var d descriptor
	if json.Unmarshal(fields["mediaType"], &d.MediaType) != nil || d.MediaType == "" {
		return descriptor{}, fmt.Errorf("%w: %s has no media type", ErrBadLayout, what)
	}
	if json.Unmarshal(fields["digest"], &d.Digest) != nil || !refDigestRE.MatchString(d.Digest) {
		return descriptor{}, fmt.Errorf("%w: %s has no sha256 digest", ErrBadLayout, what)
	}
	if json.Unmarshal(fields["size"], &d.Size) != nil || d.Size < 0 || d.Size > blobLimit {
		return descriptor{}, fmt.Errorf("%w: %s has no size within bounds", ErrBadLayout, what)
	}
	if u, ok := fields["urls"]; ok && json.Unmarshal(u, &d.URLs) != nil {
		return descriptor{}, fmt.Errorf("%w: %s has unreadable urls", ErrBadLayout, what)
	}
	if d.Size == 0 && d.Digest != emptyDigest {
		return descriptor{}, fmt.Errorf("%w: %s is empty and says it is %s", ErrBadLayout, what, d.Digest)
	}
	return d, nil
}

func schemaVersion2(fields map[string]json.RawMessage, what string) error {
	var v int
	if json.Unmarshal(fields["schemaVersion"], &v) != nil || v != 2 {
		return fmt.Errorf("%w: %s is not schema version 2", ErrBadLayout, what)
	}
	return nil
}

// strictObject reads a JSON object's top-level keys and refuses one that
// names a key twice — "layers" and "layers" are the same key once
// decoded — or that spells a known key in another case.
//
// encoding/json matches field names without regard to case and lets the
// last duplicate win, so {"layers":[A],"Layers":[B]} is read one way here
// and possibly another by whatever pulls the image. A manifest that can be
// read two ways is refused rather than pushed.
func strictObject(raw []byte, what string, known ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("%w: %s is not a JSON object", ErrBadLayout, what)
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %s is not valid JSON", ErrBadLayout, what)
		}
		key, _ := tok.(string)
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("%w: %s names %q twice", ErrBadLayout, what, key)
		}
		for _, k := range known {
			if key != k && strings.EqualFold(key, k) {
				return nil, fmt.Errorf("%w: %s spells %q as %q", ErrBadLayout, what, k, key)
			}
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("%w: %s is not valid JSON", ErrBadLayout, what)
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%w: %s is not valid JSON", ErrBadLayout, what)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: %s has more after its object", ErrBadLayout, what)
	}
	return fields, nil
}

// blobPath is where a digest's blob is in a layout.
func blobPath(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

// readSmallBlob reads a blob whole and checks it is what its digest says.
func readSmallBlob(ctx context.Context, layout LayoutFiles, digest string, size, limit int64) ([]byte, error) {
	if size > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrBadLayout, digest, limit)
	}
	rc, err := openBlob(ctx, layout, descriptor{Digest: digest, Size: size})
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}

// openBlob opens a blob through a reader that fails, rather than ending,
// when the content is not the digest and size the descriptor says.
func openBlob(ctx context.Context, layout LayoutFiles, d descriptor) (io.ReadCloser, error) {
	rc, size, err := layout.Open(ctx, blobPath(d.Digest), d.Size)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrBadLayout, d.Digest, err)
	}
	if size != d.Size {
		rc.Close() //nolint:errcheck
		return nil, fmt.Errorf("%w: %s is %d bytes, and the manifest says %d",
			ErrBadLayout, d.Digest, size, d.Size)
	}
	return &verifyingReader{r: io.LimitReader(rc, d.Size), c: rc, want: d, h: sha256.New()}, nil
}

// verifyingReader hashes what it reads and refuses a blob that is not what
// its descriptor says before the blob is complete.
//
// Checked on the read that would deliver the last byte, and that read's
// bytes withheld on a mismatch: net/http stops reading a body once it has
// Content-Length bytes, so a check at EOF can come after the registry already
// has all of it. Withholding the end means a wrong blob is never sent whole.
type verifyingReader struct {
	r    io.Reader
	c    io.Closer
	want descriptor
	h    hash.Hash
	n    int64
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	v.n += int64(n)
	if v.n == v.want.Size || err == io.EOF {
		got := "sha256:" + hex.EncodeToString(v.h.Sum(nil))
		if v.n != v.want.Size || got != v.want.Digest {
			return 0, fmt.Errorf("%w: %s holds content that is %s (%d bytes)",
				ErrBadLayout, v.want.Digest, got, v.n)
		}
	}
	return n, err
}

func (v *verifyingReader) Close() error { return v.c.Close() }

// pusher sends one image's blobs and manifest to one repository, keeping the
// authorization the registry last gave it.
type pusher struct {
	client manifestClient
	ref    reference
	auth   string
}

func (p *pusher) base() string {
	return p.client.registryURL(p.ref).String() + "/v2/" + p.ref.name
}

// send makes a request, answering one challenge if the registry asks for
// authorization. body is called for each attempt, so a refused request can be
// sent again with what the registry asked for.
func (p *pusher) send(
	ctx context.Context, method, target string, header http.Header,
	body func() (io.ReadCloser, int64, error),
) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var rc io.ReadCloser = http.NoBody
		var size int64
		if body != nil {
			var err error
			if rc, size, err = body(); err != nil {
				return nil, err
			}
		}
		if size == 0 && rc != http.NoBody {
			// An empty blob, its digest already checked to be the empty
			// one. Sent as no body rather than a reader of none, which
			// net/http would send chunked.
			rc.Close() //nolint:errcheck
			rc = http.NoBody
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rc)
		if err != nil {
			rc.Close() //nolint:errcheck
			return nil, fmt.Errorf("registry: build %s request: %w", method, err)
		}
		req.ContentLength = size
		for k, v := range header {
			req.Header[k] = v
		}
		if p.auth != "" {
			req.Header.Set("Authorization", p.auth)
		}
		resp, err := p.do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt == 1 {
			return resp, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		drain(resp)
		next, err := p.client.authorize(ctx, challenge, p.ref)
		if err != nil {
			return nil, err
		}
		if next == "" || next == p.auth {
			return nil, fmt.Errorf("%w: %s", ErrCredentialsRejected, p.ref.host)
		}
		p.auth = next
	}
	return nil, fmt.Errorf("%w: %s", ErrCredentialsRejected, p.ref.host)
}

// do sends a request and follows no redirect.
//
// Nothing in a push is a redirect to follow. A HEAD on a blob that a
// registry keeps in S3, GCS or Azure is answered with a redirect to a signed
// URL for it, which says the blob is there and is a credential in its own
// right; the POST and PUTs of an upload are the registry's to answer itself,
// and a redirect of one is a refusal. So the redirect is the answer, and its
// Location is never read into anything — and a failed request is reported
// by its method, host and path, never by a URL whose query could be signed.
func (p *pusher) do(req *http.Request) (*http.Response, error) {
	client := *p.client.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("registry: %s %s://%s%s: %w",
				req.Method, req.URL.Scheme, req.URL.Host, req.URL.Path, ue.Err)
		}
		return nil, err
	}
	return resp, nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close() //nolint:errcheck
}

// refused is a response that was not the one wanted, as an error.
func (p *pusher) refused(resp *http.Response, what string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s refused %s: %s", ErrCredentialsRejected, p.ref.host, what, resp.Status)
	}
	msg := strings.TrimSpace(string(body))
	if msg != "" {
		msg = ": " + msg
	}
	return fmt.Errorf("registry: %s answered %s to %s%s", p.ref.host, resp.Status, what, msg)
}

// pushBlob sends a blob unless the repository already has it.
//
// The existence check is in this repository and nowhere else, and the upload
// is a plain one: no mount parameter, so no blob of another repository's is
// ever linked into this one, whatever the layout says about where a blob came
// from.
func (p *pusher) pushBlob(ctx context.Context, layout LayoutFiles, d descriptor, what string, logf func(string)) error {
	blobURL := p.base() + "/blobs/" + d.Digest
	resp, err := p.send(ctx, http.MethodHead, blobURL, nil, nil)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		drain(resp)
		return nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// A redirect to where the blob is stored: it is there.
		drain(resp)
		return nil
	case resp.StatusCode == http.StatusNotFound:
		drain(resp)
	default:
		return p.refused(resp, "a check for "+d.Digest)
	}

	if d.Size > 1<<20 {
		logf(fmt.Sprintf("Pushing %s, %d MB\n", what, d.Size>>20))
	}
	resp, err = p.send(ctx, http.MethodPost, p.base()+"/blobs/uploads/", nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		return p.refused(resp, "starting an upload")
	}
	location := resp.Header.Get("Location")
	drain(resp)
	target, err := p.uploadURL(location, d.Digest)
	if err != nil {
		return err
	}

	header := http.Header{"Content-Type": {"application/octet-stream"}}
	resp, err = p.send(ctx, http.MethodPut, target, header, func() (io.ReadCloser, int64, error) {
		rc, err := openBlob(ctx, layout, d)
		return rc, d.Size, err
	})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return p.refused(resp, "the upload of "+d.Digest)
	}
	drain(resp)
	return nil
}

// uploadURL is where to PUT a blob, from the Location the registry gave.
//
// It may be relative, and it may carry the registry's own state in its
// query, which is kept. It is not followed off the registry's own address:
// the PUT carries the credential.
func (p *pusher) uploadURL(location, digest string) (string, error) {
	if location == "" {
		return "", fmt.Errorf("registry: %s started an upload and said nowhere to send it", p.ref.host)
	}
	base := p.client.registryURL(p.ref)
	u, err := base.Parse(location)
	if err != nil {
		return "", fmt.Errorf("registry: %s gave an upload location that is not a URL", p.ref.host)
	}
	if !sameAuthority(u, base) || (base.Scheme == "https" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("registry: refused to send a blob to %s, which is not %s",
			authorityOf(u), authorityOf(base))
	}
	q := u.Query()
	q.Set("digest", digest)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// putManifest sends the manifest, byte for byte, to the tag.
func (p *pusher) putManifest(ctx context.Context, raw []byte, mediaType, digest string) error {
	header := http.Header{"Content-Type": {mediaType}}
	resp, err := p.send(ctx, http.MethodPut, p.base()+"/manifests/"+url.PathEscape(p.ref.tag), header,
		func() (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(raw)), int64(len(raw)), nil
		})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return p.refused(resp, "the manifest")
	}
	stated := resp.Header.Get("Docker-Content-Digest")
	drain(resp)
	if stated != "" && !strings.EqualFold(stated, digest) {
		return fmt.Errorf("registry: %s stored the manifest as %s, not %s", p.ref.host, stated, digest)
	}
	return nil
}
