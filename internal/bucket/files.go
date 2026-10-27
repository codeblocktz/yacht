package bucket

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/backup"
)

// Files is one bucket's contents, reached with its own key: what its Files
// tab lists, uploads and deletes. Nothing here can reach another bucket.
type Files struct {
	// inside signs the requests the engine sends itself, to Garage in the
	// cluster; outside the links a browser follows, to the endpoint apps use.
	inside, outside *backup.S3
	http            *http.Client
	err             error
}

// Files is a bucket's contents.
func (g *Garage) Files(b Created) *Files {
	client := g.HTTP
	if client == nil {
		client = filesClient
	}
	f := &Files{http: client}
	f.inside, f.err = backup.NewS3(g.S3URL, g.Region, b.Name, "", b.KeyID, b.Secret)
	if f.err == nil {
		f.outside, f.err = backup.NewS3(g.Endpoint, g.Region, b.Name, "", b.KeyID, b.Secret)
	}
	return f
}

// filesClient has no timeout on the whole request, which an upload of a
// large file would outlast: the caller's context ends it, and a store that
// does not answer at all is given 30 seconds.
var filesClient = &http.Client{Transport: &http.Transport{
	Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second,
	MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second,
}}

// Object is a file in a bucket.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Name is the last part of the key, what a listing of its folder shows.
func (o Object) Name() string { return o.Key[strings.LastIndex(o.Key, "/")+1:] }

// Listing is one folder of a bucket: the files directly in it, and the
// folders below it — S3 has no folders, only keys with slashes in them, and
// these are the parts before the next one.
type Listing struct {
	Prefix  string
	Folders []string
	Objects []Object
	// Next continues the listing where it stopped, empty when it did not.
	Next string
}

// listPage is the most a listing asks for at once.
const listPage = 200

var errNoSuchBucket = errors.New("bucket: no such bucket")

// List is one page of a folder.
func (f *Files) List(ctx context.Context, prefix, after string) (Listing, error) {
	if f.err != nil {
		return Listing{}, f.err
	}
	q := url.Values{
		"list-type": {"2"}, "delimiter": {"/"}, "prefix": {prefix},
		"max-keys": {strconv.Itoa(listPage)},
	}
	if after != "" {
		q.Set("continuation-token", after)
	}
	u, err := f.inside.PresignBucket(http.MethodGet, q, 5*time.Minute, time.Now())
	if err != nil {
		return Listing{}, err
	}
	resp, err := f.do(ctx, http.MethodGet, u, nil, 0, "")
	if err != nil {
		return Listing{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Contents []struct {
			Key          string `xml:"Key"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		} `xml:"Contents"`
		CommonPrefixes []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
		IsTruncated           bool   `xml:"IsTruncated"`
		NextContinuationToken string `xml:"NextContinuationToken"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return Listing{}, fmt.Errorf("%w: a listing it could not read: %v", ErrUnavailable, err)
	}
	l := Listing{Prefix: prefix}
	for _, p := range out.CommonPrefixes {
		l.Folders = append(l.Folders, p.Prefix)
	}
	for _, c := range out.Contents {
		// The folder itself, as some tools make it: an empty object named
		// with the slash.
		if c.Key == prefix {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, c.LastModified)
		l.Objects = append(l.Objects, Object{Key: c.Key, Size: c.Size, Modified: at})
	}
	if out.IsTruncated {
		l.Next = out.NextContinuationToken
	}
	return l, nil
}

// Put stores a file, replacing one already at its key.
func (f *Files) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	if f.err != nil {
		return f.err
	}
	if err := ValidKey(key); err != nil {
		return err
	}
	u, err := f.inside.Presign(http.MethodPut, key, 15*time.Minute, time.Now())
	if err != nil {
		return err
	}
	resp, err := f.do(ctx, http.MethodPut, u, body, size, contentType)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Delete removes a file. One already gone is not an error.
func (f *Files) Delete(ctx context.Context, key string) error {
	if f.err != nil {
		return f.err
	}
	u, err := f.inside.Presign(http.MethodDelete, key, 5*time.Minute, time.Now())
	if err != nil {
		return err
	}
	resp, err := f.do(ctx, http.MethodDelete, u, nil, 0, "")
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if resp != nil {
		resp.Body.Close()
	}
	return nil
}

// DownloadURL is a link a browser downloads one file from, through the
// endpoint apps use, good for a few minutes and for that file alone.
func (f *Files) DownloadURL(key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.outside.Presign(http.MethodGet, key, 5*time.Minute, time.Now())
}

// Empty deletes every file, which Garage asks of a bucket before it will
// delete it.
func (f *Files) Empty(ctx context.Context) error {
	if f.err != nil {
		return f.err
	}
	for range 10000 {
		l, err := f.listAll(ctx)
		if err != nil {
			return err
		}
		if len(l) == 0 {
			return nil
		}
		for _, key := range l {
			if err := f.Delete(ctx, key); err != nil {
				return err
			}
		}
	}
	return ErrNotEmpty
}

// listAll is up to a page of keys anywhere in the bucket, folders or not.
func (f *Files) listAll(ctx context.Context) ([]string, error) {
	q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
	u, err := f.inside.PresignBucket(http.MethodGet, q, 5*time.Minute, time.Now())
	if err != nil {
		return nil, err
	}
	resp, err := f.do(ctx, http.MethodGet, u, nil, 0, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: a listing it could not read: %v", ErrUnavailable, err)
	}
	keys := make([]string, 0, len(out.Contents))
	for _, c := range out.Contents {
		keys = append(keys, c.Key)
	}
	return keys, nil
}

// ValidKey refuses a key S3 would, or that the Files tab could not show: empty,
// over 1024 bytes, or with a part that is empty, "." or "..".
func ValidKey(key string) error {
	if key == "" || len(key) > 1024 {
		return errors.New("a file's name is 1 to 1024 characters")
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "//") {
		return errors.New("a file's name has no empty folder in it")
	}
	for _, part := range strings.Split(key, "/") {
		if part == "." || part == ".." {
			return errors.New(`a file's name has no "." or ".." folder in it`)
		}
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return errors.New("a file's name has no control characters in it")
		}
	}
	return nil
}

// do sends one signed request and turns an error status into an error.
func (f *Files) do(ctx context.Context, method, u string, body io.Reader, size int64, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(raw, &e)
	switch {
	case e.Code == "NoSuchBucket":
		return nil, errNoSuchBucket
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	}
	return nil, fmt.Errorf("bucket: the store answered %s: %s %s", resp.Status, e.Code, e.Message)
}
