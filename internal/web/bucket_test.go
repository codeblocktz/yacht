package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	objstore "github.com/codeblocktz/yacht/internal/bucket"
)

// fakeS3 answers the few S3 requests the Files tab makes: one listing, and
// whatever is put or deleted, remembered.
type fakeS3 struct {
	mu   sync.Mutex
	puts map[string]string
	dels []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Query().Get("X-Amz-Signature") == "" {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/media-abc123/")
	switch r.Method {
	case http.MethodPut:
		if key == "huge.bin" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Forbidden: Bucket size quota is reached</Message></Error>`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.puts[key] = string(b)
	case http.MethodDelete:
		f.dels = append(f.dels, key)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<ListBucketResult>
  <CommonPrefixes><Prefix>photos/</Prefix></CommonPrefixes>
  <Contents><Key>report.pdf</Key><Size>2048</Size><LastModified>2026-10-27T10:00:00Z</LastModified></Contents>
  <IsTruncated>false</IsTruncated>
</ListBucketResult>`)
	}
}

type bucketApps struct {
	*fakeApps
	garage   *objstore.Garage
	deployed int
	measured int
}

func newBucketApps(t *testing.T) (*bucketApps, *fakeS3) {
	t.Helper()
	s3 := &fakeS3{puts: map[string]string{}}
	srv := httptest.NewServer(s3)
	t.Cleanup(srv.Close)
	media := sampleApp("owner-1", "media")
	media.Source, media.Image, media.Replicas, media.Port, media.Internal = app.SourceBucket, app.BucketImage, 0, 0, true
	media.StatusKnown = false
	web := sampleApp("owner-1", "web")
	return &bucketApps{
		fakeApps: newFakeApps(media, web),
		garage: &objstore.Garage{
			AdminURL: srv.URL, Token: strings.Repeat("t", 32),
			S3URL: srv.URL, Endpoint: "https://s3.example.com", Region: "datahuru",
		},
	}, s3
}

func (b *bucketApps) Capabilities(context.Context) app.Capabilities {
	return app.Capabilities{CanBucket: true}
}

func (b *bucketApps) DeployBucket(_ context.Context, ownerID string, _ uuid.UUID) (app.App, error) {
	b.deployed++
	a := sampleApp(ownerID, "bucket")
	a.Source = app.SourceBucket
	b.byOwner[ownerID] = append(b.byOwner[ownerID], a)
	return a, nil
}

func (b *bucketApps) Bucket(_ context.Context, _, name string) (app.Bucket, error) {
	if name != "media" {
		return app.Bucket{}, app.ErrNotABucket
	}
	return app.Bucket{
		Name: "media-abc123", Endpoint: "https://s3.example.com", Region: "datahuru",
		Bytes: 3 << 20, Objects: 2, MeasuredAt: time.Now().Add(-time.Minute),
		TeamLimit: 5 << 30, TeamBytes: 1 << 30,
	}, nil
}

func (b *bucketApps) BucketFiles(_ context.Context, _, name string) (*objstore.Files, error) {
	if name != "media" {
		return nil, app.ErrNotABucket
	}
	return b.garage.Files(objstore.Created{Name: "media-abc123", KeyID: "GKkey", Secret: "secret"}), nil
}

func (b *bucketApps) MeasureBucket(context.Context, string, string) error {
	b.measured++
	return nil
}

func TestAddBucketIsOfferedOnlyWithAStore(t *testing.T) {
	apps, _ := newBucketApps(t)
	h := testServer(t, Options{Apps: apps})
	if canvas := get(t, h, "/projects/default").Body.String(); !strings.Contains(canvas, `action="/apps/new/bucket"`) {
		t.Error("the canvas's Add has no Bucket")
	}
	rec := post(t, h, "/apps/new/bucket", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/bucket" || apps.deployed != 1 {
		t.Fatalf("Add › Bucket = %d %s, deployed %d", rec.Code, rec.Header().Get("Location"), apps.deployed)
	}

	plain := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})
	if canvas := get(t, plain, "/projects/default").Body.String(); strings.Contains(canvas, "/apps/new/bucket") {
		t.Error("Add offers a bucket on an install with no store")
	}
	if picker := get(t, h, "/apps/new").Body.String(); strings.Contains(picker, "source=bucket") {
		t.Error("the image and repository picker lists Bucket, which Add › Bucket makes in one click")
	}
}

// A bucket's panel is its files, its variables and its settings: nothing
// runs, so there is nothing to deploy, measure or open a console in.
func TestABucketsPanelIsItsFiles(t *testing.T) {
	apps, _ := newBucketApps(t)
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps/media").Body.String()
	for _, want := range []string{
		">Files<", ">Variables<", ">Settings<", "media-abc123", "3.0 MiB", `hx-get="/apps/media/files/list"`,
		"ACCESS_KEY_ID=${{media.ACCESS_KEY_ID}}", "S3-compatible bucket",
		"1.0 GiB of 5.0 GiB", "4.0 GiB left",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the bucket's panel is missing %q", want)
		}
	}
	for _, not := range []string{">Deployments<", ">Metrics<", ">Console<", "yacht.invalid"} {
		if strings.Contains(body, not) {
			t.Errorf("the bucket's panel has %q", not)
		}
	}
	if settings := get(t, h, "/apps/media/settings").Body.String(); !strings.Contains(settings, "Delete bucket") ||
		strings.Contains(settings, "Replicas") {
		t.Error("a bucket's settings are not its deletion alone")
	}
}

func TestTheFilesTabListsUploadsAndDeletes(t *testing.T) {
	apps, s3 := newBucketApps(t)
	h := testServer(t, Options{Apps: apps})

	list := get(t, h, "/apps/media/files/list").Body.String()
	for _, want := range []string{"photos", "report.pdf", "2.0 KiB", "/apps/media/files/download?key=report.pdf"} {
		if !strings.Contains(list, want) {
			t.Errorf("the listing is missing %q", want)
		}
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("prefix", "photos/")
	fw, _ := mw.CreateFormFile("file", "cat.jpg")
	_, _ = io.WriteString(fw, "meow")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/apps/media/files", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Uploaded cat.jpg.") || s3.puts["photos/cat.jpg"] != "meow" {
		t.Fatalf("upload = %d %q; store has %v", rec.Code, rec.Body.String(), s3.puts)
	}

	body.Reset()
	mw = multipart.NewWriter(&body)
	fw, _ = mw.CreateFormFile("file", "huge.bin")
	_, _ = io.WriteString(fw, "too much")
	_ = mw.Close()
	req = httptest.NewRequest(http.MethodPost, "/apps/media/files", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "http://example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "as much as its plan allows") {
		t.Fatalf("an upload past the team's quota = %q", rec.Body.String())
	}

	rec = post(t, h, "/apps/media/files/delete", url.Values{"key": {"photos/cat.jpg"}})
	if !strings.Contains(rec.Body.String(), "Deleted cat.jpg.") || len(s3.dels) != 1 || s3.dels[0] != "photos/cat.jpg" {
		t.Fatalf("delete = %q; deleted %v", rec.Body.String(), s3.dels)
	}
	if apps.measured != 2 {
		t.Errorf("measured %d times, want after the upload and the delete", apps.measured)
	}

	dl := get(t, h, "/apps/media/files/download?key=report.pdf")
	loc := dl.Header().Get("Location")
	if dl.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "https://s3.example.com/media-abc123/report.pdf?") ||
		!strings.Contains(loc, "X-Amz-Signature=") {
		t.Fatalf("download = %d %s", dl.Code, loc)
	}
	if get(t, h, "/apps/web/files/list").Code != http.StatusNotFound {
		t.Error("an app that is not a bucket has files")
	}
}
