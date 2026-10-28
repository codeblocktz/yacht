package bucket

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
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
