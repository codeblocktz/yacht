package backup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The worked example in AWS's documentation for a presigned GET
// ("Authenticating Requests: Using Query Parameters"), whose signature is
// published. Matching it is what says the signing is SigV4 and not merely
// something that looks like it.
func TestPresignMatchesAWSsPublishedExample(t *testing.T) {
	s := &S3{
		Endpoint:  &url.URL{Scheme: "https", Host: "examplebucket.s3.amazonaws.com"},
		Region:    "us-east-1",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	// The example is virtual-hosted: the bucket is in the host, and the path
	// is the key alone. Signed the same way, with the bucket left out.
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got, err := s.presignPath(http.MethodGet, "/test.txt", 86400*time.Second, at)
	if err != nil {
		t.Fatal(err)
	}
	const want = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(got, "&X-Amz-Signature="+want) {
		t.Fatalf("signed URL = %s\nwant signature %s", got, want)
	}
	if !strings.Contains(got, "X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request") {
		t.Errorf("credential scope is not encoded as SigV4 wants: %s", got)
	}
}

func TestKeysAreEscapedAndPrefixed(t *testing.T) {
	s, err := NewS3("https://acct.r2.cloudflarestorage.com", "auto", "datahuru-backups", "/databases/",
		"AKIDACCESS", "SECRETSECRET")
	if err != nil {
		t.Fatal(err)
	}
	key := s.Key("yacht-abc/db/2026 10 25.pgdump")
	if key != "databases/yacht-abc/db/2026 10 25.pgdump" {
		t.Errorf("key = %q", key)
	}
	u, err := s.Presign(http.MethodPut, key, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://acct.r2.cloudflarestorage.com/datahuru-backups/databases/yacht-abc/db/2026%2010%2025.pgdump?") {
		t.Errorf("url = %s", u)
	}
	if s.Where() != "acct.r2.cloudflarestorage.com/datahuru-backups/databases" {
		t.Errorf("where = %q", s.Where())
	}
	if strings.Contains(s.Where(), "AKID") || strings.Contains(u, "SECRETSECRET") {
		t.Errorf("a credential is in what the dashboard shows or in the URL: %q", s.Where())
	}
}

func TestBadSettingsAreRefusedWithTheReason(t *testing.T) {
	for _, tc := range []struct{ endpoint, bucket, access, secret string }{
		{"s3.example.com", "b-1", "k", "s"},
		{"https://s3.example.com/bucket", "b-1", "k", "s"},
		{"https://s3.example.com", "B", "k", "s"},
		{"https://s3.example.com", "b-1", "", "s"},
	} {
		if _, err := NewS3(tc.endpoint, "", tc.bucket, "", tc.access, tc.secret); err == nil {
			t.Errorf("NewS3(%+v) was accepted", tc)
		}
	}
	s, err := NewS3("http://minio.lan:9000", "", "backups", "", "k", "s")
	if err != nil || s.Region != "us-east-1" {
		t.Fatalf("a plain-HTTP store on the LAN with no region: %v %+v", err, s)
	}
}

func TestDeleteSendsASignedDeleteAndTreatsMissingAsDone(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		if r.URL.Query().Get("X-Amz-Signature") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/locked") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s, err := NewS3(srv.URL, "", "backups", "p", "k", "s")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Delete(ctx, s.Key("one")); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := s.Delete(ctx, s.Key("gone")); err != nil {
		t.Errorf("deleting what is not there: %v", err)
	}
	if err := s.Delete(ctx, s.Key("locked")); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("a refusal reads %v", err)
	}
	if got[0] != "DELETE /backups/p/one" {
		t.Errorf("requests = %v", got)
	}
}
