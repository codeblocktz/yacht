// Package bucket is customers' object storage: Add › Bucket, an S3-compatible
// bucket on the install's own Garage (garagehq.deuxfleurs.fr), with an access
// key that can reach that bucket and nothing else.
//
// The engine speaks Garage's admin API v2 for what only an operator could do
// otherwise — make a bucket and a key, allow the one on the other, read how
// much is stored — and the S3 API, with the bucket's own key, for its Files
// tab. The store's admin token never leaves the engine; a customer's app is
// given its bucket's key alone.
package bucket

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Garage is an install's Garage, as the engine reaches it.
type Garage struct {
	// AdminURL is its admin API, http://garage.datahuru-buckets.svc:3903;
	// Token is the admin token it was started with.
	AdminURL string
	Token    string

	// S3URL is its S3 API as the engine reaches it, in the cluster, for the
	// Files tab. Endpoint is the same API as apps and browsers reach it,
	// https://s3.example.com, which is what a bucket's ENDPOINT says and
	// what a download link points at. Region is what requests are signed
	// for.
	S3URL    string
	Endpoint string
	Region   string

	// HTTP makes the requests. Nil is a client with a timeout.
	HTTP *http.Client
}

// Validate checks the settings, so an install with a typo in them is told at
// start rather than at the first Add › Bucket.
func (g *Garage) Validate() error {
	for name, raw := range map[string]string{"admin URL": g.AdminURL, "S3 URL": g.S3URL, "endpoint": g.Endpoint} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Trim(u.Path, "/") != "" {
			return fmt.Errorf("bucket: the %s is an http or https address with no path, not %q", name, raw)
		}
	}
	if len(g.Token) < 16 {
		return errors.New("bucket: the admin token is missing or too short")
	}
	if !regionRE.MatchString(g.Region) {
		return fmt.Errorf("bucket: %q is not a region name", g.Region)
	}
	return nil
}

var regionRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Created is a bucket just made, with its key. The secret is in nothing the
// engine keeps but the variable it is sealed into.
type Created struct {
	// ID is Garage's id for the bucket; Name its one global alias, what S3
	// requests name.
	ID, Name string
	// KeyID is the access key's id, which is also what S3 calls it.
	KeyID, Secret string
}

// Usage is what a bucket holds.
type Usage struct {
	Bytes, Objects int64
}

// ErrUnavailable is Garage not answering, or answering with something the
// engine cannot use. Said as such on the page rather than as a status code.
var ErrUnavailable = errors.New("bucket: the store is not answering")

// layoutCapacity is the capacity given to each node of a layout. Garage uses
// it only to weigh nodes against each other, and every node here is weighed
// alike: what one can hold is its volume's business.
const layoutCapacity = 1 << 40

// EnsureLayout gives every node without one a place in the layout, and
// applies it. A Garage just started has none, and refuses every read and
// write until it does. Nothing is changed for a node that has one.
func (g *Garage) EnsureLayout(ctx context.Context, zone string) error {
	var status struct {
		LayoutVersion int64 `json:"layoutVersion"`
		Nodes         []struct {
			ID   string          `json:"id"`
			IsUp bool            `json:"isUp"`
			Role json.RawMessage `json:"role"`
		} `json:"nodes"`
	}
	if err := g.call(ctx, http.MethodGet, "/v2/GetClusterStatus", nil, &status); err != nil {
		return err
	}
	type role struct {
		ID       string   `json:"id"`
		Zone     string   `json:"zone"`
		Capacity int64    `json:"capacity"`
		Tags     []string `json:"tags"`
	}
	var roles []role
	for _, n := range status.Nodes {
		if n.IsUp && (len(n.Role) == 0 || string(n.Role) == "null") {
			roles = append(roles, role{ID: n.ID, Zone: zone, Capacity: layoutCapacity, Tags: []string{}})
		}
	}
	if len(roles) == 0 {
		return nil
	}
	if err := g.call(ctx, http.MethodPost, "/v2/UpdateClusterLayout", map[string]any{"roles": roles}, nil); err != nil {
		return err
	}
	var layout struct {
		Version int64 `json:"version"`
	}
	if err := g.call(ctx, http.MethodGet, "/v2/GetClusterLayout", nil, &layout); err != nil {
		return err
	}
	return g.call(ctx, http.MethodPost, "/v2/ApplyClusterLayout", map[string]any{"version": layout.Version + 1}, nil)
}

// nameRE is what a bucket's name may be: S3's rule, which Garage keeps.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// Create makes a bucket named after the service with a random ending —
// names are the whole install's, and two teams' "media" cannot both be
// "media" — and a key allowed to read and write it and nothing else. A
// failure part-way takes back what was made.
func (g *Garage) Create(ctx context.Context, service string) (Created, error) {
	name := bucketName(service)
	if !nameRE.MatchString(name) {
		return Created{}, fmt.Errorf("bucket: %q is not a bucket name", name)
	}
	var bucket struct {
		ID string `json:"id"`
	}
	if err := g.call(ctx, http.MethodPost, "/v2/CreateBucket", map[string]any{"globalAlias": name}, &bucket); err != nil {
		return Created{}, err
	}
	var key struct {
		AccessKeyID     string `json:"accessKeyId"`
		SecretAccessKey string `json:"secretAccessKey"`
	}
	err := g.call(ctx, http.MethodPost, "/v2/CreateKey", map[string]any{"name": name}, &key)
	if err == nil && (key.AccessKeyID == "" || key.SecretAccessKey == "") {
		err = fmt.Errorf("%w: a key came back without its secret", ErrUnavailable)
	}
	if err == nil {
		err = g.call(ctx, http.MethodPost, "/v2/AllowBucketKey", map[string]any{
			"bucketId": bucket.ID, "accessKeyId": key.AccessKeyID,
			"permissions": map[string]bool{"read": true, "write": true, "owner": false},
		}, nil)
	}
	if err != nil {
		// Background, not ctx: a request cancelled part-way still has to
		// take back what it made.
		undo, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if key.AccessKeyID != "" {
			_ = g.call(undo, http.MethodPost, "/v2/DeleteKey?id="+url.QueryEscape(key.AccessKeyID), nil, nil)
		}
		_ = g.call(undo, http.MethodPost, "/v2/DeleteBucket?id="+url.QueryEscape(bucket.ID), nil, nil)
		return Created{}, err
	}
	return Created{ID: bucket.ID, Name: name, KeyID: key.AccessKeyID, Secret: key.SecretAccessKey}, nil
}

// bucketName is a service's name with six random characters after it,
// within S3's 63.
func bucketName(service string) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [6]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	base := strings.Trim(strings.ToLower(service), "-")
	if len(base) > 56 {
		base = strings.Trim(base[:56], "-")
	}
	return base + "-" + string(b[:])
}

// Usage is what a bucket holds now: every object, and the bytes of their
// latest versions.
func (g *Garage) Usage(ctx context.Context, id string) (Usage, error) {
	var info struct {
		Bytes   int64 `json:"bytes"`
		Objects int64 `json:"objects"`
	}
	if err := g.call(ctx, http.MethodGet, "/v2/GetBucketInfo?id="+url.QueryEscape(id), nil, &info); err != nil {
		return Usage{}, err
	}
	return Usage{Bytes: info.Bytes, Objects: info.Objects}, nil
}

// ErrNotEmpty is a bucket with files still in it, which Garage will not
// delete.
var ErrNotEmpty = errors.New("bucket: the bucket still has files in it")

// Delete removes a bucket and its key, emptying it first. Gone already is
// not an error: the outcome asked for is that it is gone.
func (g *Garage) Delete(ctx context.Context, b Created) error {
	_, err := g.Usage(ctx, b.ID)
	switch {
	case errors.Is(err, errNotFound):
		// Gone, and its key may be with it: nothing to empty, and nothing to
		// list it with.
	case err != nil:
		return err
	default:
		if err := g.Files(b).Empty(ctx); err != nil && !errors.Is(err, errNoSuchBucket) {
			return err
		}
		if err := g.call(ctx, http.MethodPost, "/v2/DeleteBucket?id="+url.QueryEscape(b.ID), nil, nil); err != nil && !errors.Is(err, errNotFound) {
			if strings.Contains(err.Error(), "not empty") {
				return ErrNotEmpty
			}
			return err
		}
	}
	if err := g.call(ctx, http.MethodPost, "/v2/DeleteKey?id="+url.QueryEscape(b.KeyID), nil, nil); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	return nil
}

var errNotFound = errors.New("bucket: not found")

// call sends one request to the admin API and decodes its answer into out,
// when out is not nil.
func (g *Garage) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.AdminURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", errNotFound, path)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("bucket: %s answered %s: %s", strings.SplitN(path, "?", 2)[0], resp.Status, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, path, err)
	}
	return nil
}

func (g *Garage) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}
