package bucket

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBucketNamesAreTheServiceWithARandomEnding(t *testing.T) {
	a, b := bucketName("media"), bucketName("media")
	if !strings.HasPrefix(a, "media-") || len(a) != len("media-")+6 || a == b {
		t.Fatalf("names %q and %q: want media-<6 random>, each different", a, b)
	}
	long := bucketName(strings.Repeat("x", 63))
	if !nameRE.MatchString(long) {
		t.Fatalf("a long service's bucket %q is not a bucket name", long)
	}
}

func TestValidKey(t *testing.T) {
	for key, ok := range map[string]bool{
		"photo.jpg": true, "a/b/c.txt": true, "with space.pdf": true,
		"": false, "/abs": false, "a//b": false, "../x": false, "a/./b": false, "a\nb": false,
		strings.Repeat("k", 1025): false,
	} {
		if err := ValidKey(key); (err == nil) != ok {
			t.Errorf("ValidKey(%q) = %v, want ok %v", key, err, ok)
		}
	}
}

// TestAgainstGarage runs the whole of a bucket's life against a real Garage:
// YACHT_TEST_GARAGE_ADMIN=http://localhost:13903,
// YACHT_TEST_GARAGE_S3=http://localhost:13900 and YACHT_TEST_GARAGE_TOKEN.
// Its layout is given it here if it has none.
func TestAgainstGarage(t *testing.T) {
	admin := os.Getenv("YACHT_TEST_GARAGE_ADMIN")
	if admin == "" {
		t.Skip("YACHT_TEST_GARAGE_ADMIN not set")
	}
	ctx := context.Background()
	s3 := os.Getenv("YACHT_TEST_GARAGE_S3")
	g := &Garage{
		AdminURL: admin, Token: os.Getenv("YACHT_TEST_GARAGE_TOKEN"),
		S3URL: s3, Endpoint: s3, Region: "datahuru",
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := g.EnsureLayout(ctx, "test"); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	// Twice: a node with a place keeps it.
	if err := g.EnsureLayout(ctx, "test"); err != nil {
		t.Fatalf("EnsureLayout again: %v", err)
	}

	// Its data partition, which the install's room for buckets is read
	// from.
	d, err := g.Disk(ctx)
	if err != nil || d.Total <= 0 || d.Available <= 0 || d.Available > d.Total {
		t.Fatalf("Disk = %+v, %v; want the data partition's size and what is free on it", d, err)
	}

	b, err := g.Create(ctx, "media")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.ID == "" || b.KeyID == "" || b.Secret == "" || !strings.HasPrefix(b.Name, "media-") {
		t.Fatalf("Create = %+v", b)
	}
	f := g.Files(b)
	for key, body := range map[string]string{
		"hello.txt": "hello", "photos/2026/a.jpg": "jpeg bytes", "photos/b.jpg": "more",
	} {
		if err := f.Put(ctx, key, strings.NewReader(body), int64(len(body)), "text/plain"); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}

	top, err := f.List(ctx, "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(top.Objects) != 1 || top.Objects[0].Key != "hello.txt" || top.Objects[0].Size != 5 ||
		len(top.Folders) != 1 || top.Folders[0] != "photos/" {
		t.Fatalf("the top folder = %+v, want hello.txt and photos/", top)
	}
	photos, err := f.List(ctx, "photos/", "")
	if err != nil {
		t.Fatalf("List photos/: %v", err)
	}
	if len(photos.Objects) != 1 || photos.Objects[0].Name() != "b.jpg" || len(photos.Folders) != 1 {
		t.Fatalf("photos/ = %+v", photos)
	}

	link, err := f.DownloadURL("photos/2026/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != "jpeg bytes" {
		t.Fatalf("download = %s %q", resp.Status, got)
	}

	// The key reaches its own bucket and no other.
	other, err := g.Create(ctx, "other")
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}
	cross := g.Files(Created{Name: other.Name, KeyID: b.KeyID, Secret: b.Secret})
	if err := cross.Put(ctx, "x.txt", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("one bucket's key wrote into another bucket")
	}

	// Every bucket keeps the rule that ends unfinished uploads, added once.
	for range 2 {
		if err := f.KeepAbortRule(ctx); err != nil {
			t.Fatalf("KeepAbortRule: %v", err)
		}
	}
	if rules := lifecycle(t, f); strings.Count(rules, abortRuleID) != 1 || !strings.Contains(rules, "<DaysAfterInitiation>1<") {
		t.Fatalf("the bucket's lifecycle = %s", rules)
	}

	// A customer's key may replace the lifecycle with rules of its own: the
	// next keeping adds the rule back beside them.
	theirs := `<LifecycleConfiguration><Rule><ID>theirs</ID><Status>Enabled</Status><Filter><Prefix>tmp/</Prefix></Filter>` +
		`<Expiration><Days>30</Days></Expiration></Rule></LifecycleConfiguration>`
	put, err := f.inside.PresignBucket(http.MethodPut, url.Values{"lifecycle": {""}}, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := f.do(ctx, http.MethodPut, put, strings.NewReader(theirs), int64(len(theirs)), "application/xml"); err != nil {
		t.Fatalf("a customer's lifecycle: %v", err)
	} else {
		resp.Body.Close()
	}
	if err := f.KeepAbortRule(ctx); err != nil {
		t.Fatalf("KeepAbortRule after a customer's: %v", err)
	}
	if rules := lifecycle(t, f); !strings.Contains(rules, "<ID>theirs</ID>") || !strings.Contains(rules, "<Prefix>tmp/</Prefix>") ||
		strings.Count(rules, abortRuleID) != 1 {
		t.Fatalf("the lifecycle after keeping it beside a customer's = %s", rules)
	}

	// The parts of an unfinished upload are counted, and ended.
	part := startUpload(t, f, "big.bin", strings.Repeat("p", 1000))
	if !strings.Contains(part, "200") {
		t.Fatalf("a part = %s", part)
	}
	var mid Usage
	for range 50 {
		if mid, err = g.Usage(ctx, b.ID); err == nil && mid.Uploading == 1000 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if mid.Uploading != 1000 || mid.Bytes < 1000 {
		t.Fatalf("Usage with an upload unfinished = %+v, %v; want its 1000 bytes counted", mid, err)
	}
	if n, err := f.AbortUploads(ctx); err != nil || n != 1 {
		t.Fatalf("AbortUploads = %d, %v; want the one", n, err)
	}

	// Garage counts in the background: give it a moment.
	var u Usage
	for range 50 {
		if u, err = g.Usage(ctx, b.ID); err == nil && u.Objects == 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil || u.Objects != 3 || u.Bytes != int64(len("hello")+len("jpeg bytes")+len("more")) {
		t.Fatalf("Usage = %+v, %v; want 3 objects of 19 bytes", u, err)
	}

	// A limit is Garage's to keep: past it a write is refused, and lifting
	// it lets the write in.
	if err := g.SetLimit(ctx, b.ID, 25); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	if l, err := g.Limit(ctx, b.ID); err != nil || l != 25 {
		t.Fatalf("Limit = %d, %v; want 25", l, err)
	}
	big := strings.Repeat("x", 10)
	if err := f.Put(ctx, "big.txt", strings.NewReader(big), 10, ""); !errors.Is(err, ErrFull) {
		t.Fatalf("a write past the limit = %v, want ErrFull", err)
	}
	if err := g.SetLimit(ctx, b.ID, 0); err != nil {
		t.Fatalf("SetLimit 0: %v", err)
	}
	if l, err := g.Limit(ctx, b.ID); err != nil || l != 0 {
		t.Fatalf("Limit after lifting = %d, %v; want none", l, err)
	}
	if err := f.Put(ctx, "big.txt", strings.NewReader(big), 10, ""); err != nil {
		t.Fatalf("a write once the limit is lifted: %v", err)
	}

	if err := f.Delete(ctx, "hello.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := f.Delete(ctx, "hello.txt"); err != nil {
		t.Fatalf("Delete of a file already gone: %v", err)
	}

	// Files still in it: emptied, then gone, with its key.
	for _, c := range []Created{b, other} {
		if err := g.Delete(ctx, c); err != nil {
			t.Fatalf("Delete %s: %v", c.Name, err)
		}
	}
	if _, err := g.Usage(ctx, b.ID); err == nil {
		t.Fatal("the bucket is still there")
	}
	if err := g.Delete(ctx, b); err != nil {
		t.Fatalf("Delete of a bucket already gone: %v", err)
	}
}

// The room is the data partition of the node that stores objects. More than
// one is refused rather than guessed at: what they hold together depends on a
// replication factor the admin API does not say.
func TestDiskIsTheStoringNodesDataPartitions(t *testing.T) {
	var status string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/GetClusterStatus" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, status)
	}))
	t.Cleanup(srv.Close)
	g := &Garage{AdminURL: srv.URL, Token: strings.Repeat("t", 32)}
	node := func(id, role string, up bool, partition string) string {
		return `{"id":"` + id + `","isUp":` + strconv.FormatBool(up) + `,"role":` + role +
			`,"dataPartition":` + partition + `}`
	}
	const storing, gateway = `{"zone":"z","capacity":1099511627776,"tags":[]}`, `{"zone":"z","capacity":null,"tags":[]}`
	for _, c := range []struct {
		name  string
		nodes []string
		want  Disk
		fails bool
	}{
		{"one node", []string{node("a", storing, true, `{"available":600,"total":1000}`)}, Disk{1000, 600}, false},
		{"more than one storing node", []string{
			node("a", storing, true, `{"available":600,"total":1000}`),
			node("b", storing, true, `{"available":900,"total":2000}`),
		}, Disk{}, true},
		{"gateways and nodes with no role store nothing", []string{
			node("a", storing, true, `{"available":600,"total":1000}`),
			node("b", gateway, true, `{"available":1,"total":1}`),
			node("c", "null", true, `{"available":1,"total":1}`),
		}, Disk{1000, 600}, false},
		{"a storing node down", []string{
			node("a", storing, true, `{"available":600,"total":1000}`),
			node("b", storing, false, "null"),
		}, Disk{}, true},
		{"no node laid out", []string{node("a", "null", true, `{"available":600,"total":1000}`)}, Disk{}, true},
	} {
		status = `{"layoutVersion":1,"nodes":[` + strings.Join(c.nodes, ",") + `]}`
		d, err := g.Disk(context.Background())
		if (err != nil) != c.fails || d != c.want {
			t.Errorf("%s: Disk = %+v, %v; want %+v, failing %v", c.name, d, err, c.want, c.fails)
		}
		if err != nil && !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v is not ErrUnavailable", c.name, err)
		}
	}
}

// lifecycle is the bucket's lifecycle configuration, as its own key reads it.
func lifecycle(t *testing.T, f *Files) string {
	t.Helper()
	u, err := f.inside.PresignBucket(http.MethodGet, url.Values{"lifecycle": {""}}, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.do(context.Background(), http.MethodGet, u, nil, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// startUpload begins a multipart upload and sends one part of it, and never
// completes it; it says how the part was answered.
func startUpload(t *testing.T, f *Files, key, body string) string {
	t.Helper()
	ctx := context.Background()
	u, err := f.inside.PresignQuery(http.MethodPost, key, url.Values{"uploads": {""}}, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.do(ctx, http.MethodPost, u, nil, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	if err != nil || started.UploadID == "" {
		t.Fatalf("start an upload: %v", err)
	}
	u, err = f.inside.PresignQuery(http.MethodPut, key, url.Values{"partNumber": {"1"}, "uploadId": {started.UploadID}},
		time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resp, err = f.do(ctx, http.MethodPut, u, strings.NewReader(body), int64(len(body)), "")
	if err != nil {
		return err.Error()
	}
	resp.Body.Close()
	return resp.Status
}
