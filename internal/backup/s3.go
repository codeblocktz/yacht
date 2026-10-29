// Package backup is where database backups are kept: an S3-compatible bucket
// the install names, reached with URLs signed for one object at a time.
//
// Signed URLs rather than credentials handed to the Job that writes the dump.
// The Job runs in the customer's namespace, beside the customer's database;
// the bucket's keys open every team's backups. A URL signed for one PUT of
// one object, valid for a few hours, is the whole of what that Job can do
// with the bucket — even if somebody got into its container, there is nothing
// in it that reads another team's data.
//
// Signature Version 4 query signing, written out rather than imported: it is
// one well-specified function, and the SDKs that provide it bring a dependency
// tree larger than the rest of the engine's. Path-style addressing
// (endpoint/bucket/key), which every S3-compatible store accepts — R2, MinIO,
// Ceph, Garage — and AWS still does.
package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 is a bucket on an S3-compatible store.
type S3 struct {
	// Endpoint is the store's base URL, such as https://s3.eu-central-1.amazonaws.com
	// or https://<account>.r2.cloudflarestorage.com.
	Endpoint *url.URL
	Region   string
	Bucket   string
	// Prefix is the folder within the bucket every key is written under, so a
	// bucket shared with the install's own backups keeps them apart.
	Prefix    string
	AccessKey string
	SecretKey string

	// Client makes the requests the engine sends itself — deleting an object.
	// Nil is a client with a timeout.
	Client *http.Client
}

var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// NewS3 validates a bucket's settings.
func NewS3(endpoint, region, bucket, prefix, accessKey, secretKey string) (*S3, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(endpoint), "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("backup: the endpoint must be an http or https URL, such as https://s3.example.com")
	}
	if u.Path != "" || u.RawQuery != "" {
		return nil, fmt.Errorf("backup: the endpoint is the store's address alone, without a path — the bucket is named separately")
	}
	if !bucketRE.MatchString(bucket) {
		return nil, fmt.Errorf("backup: %q is not a bucket name", bucket)
	}
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("backup: an access key and a secret key are both required")
	}
	if region == "" {
		// What R2 and most S3-compatible stores accept for "no region", and
		// AWS's own default.
		region = "us-east-1"
	}
	prefix = strings.Trim(prefix, "/")
	return &S3{
		Endpoint: u, Region: region, Bucket: bucket, Prefix: prefix,
		AccessKey: accessKey, SecretKey: secretKey,
	}, nil
}

// Key is the object a name is stored at, under the prefix.
func (s *S3) Key(name string) string {
	if s.Prefix == "" {
		return name
	}
	return s.Prefix + "/" + name
}

// Where says where backups go, for the dashboard: the bucket and prefix, never
// the keys.
func (s *S3) Where() string {
	where := s.Endpoint.Host + "/" + s.Bucket
	if s.Prefix != "" {
		where += "/" + s.Prefix
	}
	return where
}

// Presign returns a URL that performs one request — GET, PUT, HEAD or DELETE —
// on one object, until it expires. Anybody holding it can do exactly that and
// nothing else.
func (s *S3) Presign(method, key string, expires time.Duration, now time.Time) (string, error) {
	return s.presignPath(method, s.objectPath(key), nil, expires, now)
}

// PresignQuery is Presign with a query signed in: one part of a multipart
// upload, say, named by its uploadId.
func (s *S3) PresignQuery(method, key string, query url.Values, expires time.Duration, now time.Time) (string, error) {
	return s.presignPath(method, s.objectPath(key), query, expires, now)
}

// PresignBucket returns a URL for one request on the bucket itself, with the
// query it is given signed in: a listing, ListObjectsV2's list-type=2 and
// prefix.
func (s *S3) PresignBucket(method string, query url.Values, expires time.Duration, now time.Time) (string, error) {
	return s.presignPath(method, "/"+uriEncode(s.Bucket), query, expires, now)
}

func (s *S3) presignPath(method, path string, extra url.Values, expires time.Duration, now time.Time) (string, error) {
	if expires <= 0 || expires > 7*24*time.Hour {
		return "", errors.New("backup: a signed URL lasts between one second and seven days")
	}
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	scope := day + "/" + s.Region + "/s3/aws4_request"

	q := url.Values{}
	for k, v := range extra {
		q[k] = append([]string(nil), v...)
	}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", s.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")

	canonical := strings.Join([]string{
		method,
		path,
		canonicalQuery(q),
		"host:" + s.Endpoint.Host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	toSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256(canonical),
	}, "\n")

	key0 := hmacSHA256([]byte("AWS4"+s.SecretKey), day)
	key1 := hmacSHA256(key0, s.Region)
	key2 := hmacSHA256(key1, "s3")
	signing := hmacSHA256(key2, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signing, toSign))

	return s.Endpoint.Scheme + "://" + s.Endpoint.Host + path + "?" +
		canonicalQuery(q) + "&X-Amz-Signature=" + signature, nil
}

// Delete removes an object. Deleting one that is not there succeeds, as S3's
// own DELETE does: the outcome somebody asked for is that it is gone.
func (s *S3) Delete(ctx context.Context, key string) error {
	u, err := s.Presign(http.MethodDelete, key, 5*time.Minute, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("backup: delete %s: %w", key, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	switch {
	case resp.StatusCode == http.StatusNoContent, resp.StatusCode == http.StatusOK,
		resp.StatusCode == http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("backup: delete %s: the store answered %s: %s",
			key, resp.Status, strings.TrimSpace(string(body)))
	}
}

// objectPath is /bucket/key with each segment escaped the way SigV4 requires:
// everything but unreserved characters, with the slashes between segments
// kept.
func (s *S3) objectPath(key string) string {
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = uriEncode(seg)
	}
	return "/" + uriEncode(s.Bucket) + "/" + strings.Join(segs, "/")
}

// canonicalQuery sorts by key and encodes with %20 rather than +, which is
// where url.Values.Encode and SigV4 disagree.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
