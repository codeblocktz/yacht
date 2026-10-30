package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/backup"
	"github.com/codeblocktz/yacht/internal/bucket"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// testGarage is a real Garage, as YACHT_TEST_GARAGE_ADMIN, _S3 and _TOKEN
// name it; the test is skipped without one.
func testGarage(t *testing.T) *bucket.Garage {
	t.Helper()
	admin := os.Getenv("YACHT_TEST_GARAGE_ADMIN")
	if admin == "" {
		t.Skip("set YACHT_TEST_GARAGE_ADMIN, _S3 and _TOKEN to run bucket tests")
	}
	s3 := os.Getenv("YACHT_TEST_GARAGE_S3")
	g := &bucket.Garage{
		AdminURL: admin, Token: os.Getenv("YACHT_TEST_GARAGE_TOKEN"),
		S3URL: s3, Endpoint: s3, Region: "datahuru",
	}
	if err := g.EnsureLayout(context.Background(), "test"); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	return g
}

// A bucket is a service with nothing running: made on the store with its own
// key, given Railway's variables, never deployed, never scaled or routed to,
// copied into an environment as a bucket of its own, and emptied and deleted
// from the store with its service.
func TestABucketIsAServiceWithNothingRunning(t *testing.T) {
	g := testGarage(t)
	s, orch, pool := testService(t, Options{Keeper: testKeeper(t), Buckets: g})
	id := owner(t, s, pool, "svc-buckets")
	ctx := context.Background()
	p, err := s.CreateProject(ctx, id, "Media")
	if err != nil {
		t.Fatal(err)
	}

	a, err := s.DeployBucket(ctx, id, p.ID)
	if err != nil {
		t.Fatalf("DeployBucket: %v", err)
	}
	if a.Name != "bucket" || a.Source != SourceBucket || a.Replicas != 0 || a.Port != 0 {
		t.Fatalf("bucket app = %+v", a)
	}
	if _, err := s.liveOperation(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Errorf("a bucket was given a deploy: %v", err)
	}
	if len(orch.events) != 0 {
		t.Errorf("a bucket reached the cluster: %v", orch.events)
	}

	name, err := s.RevealVariable(ctx, id, a.Name, BucketVar)
	if err != nil || !strings.HasPrefix(name, "bucket-") {
		t.Fatalf("BUCKET = %q, %v", name, err)
	}
	secret, err := s.RevealVariable(ctx, id, a.Name, BucketSecretKey)
	if err != nil || len(secret) < 20 {
		t.Fatalf("SECRET_ACCESS_KEY: %v", err)
	}
	for _, key := range []string{BucketEndpoint, BucketRegion, BucketKeyID} {
		if v, err := s.RevealVariable(ctx, id, a.Name, key); err != nil || v == "" {
			t.Errorf("%s = %q, %v", key, v, err)
		}
	}

	files, err := s.BucketFiles(ctx, id, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Put(ctx, "logo.png", strings.NewReader("png"), 3, "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var b Bucket
	for range 50 {
		if err := s.MeasureBucket(ctx, id, a.Name); err != nil {
			t.Fatal(err)
		}
		if b, err = s.Bucket(ctx, id, a.Name); err == nil && b.Objects == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if b.Objects != 1 || b.Bytes != 3 || b.Name != name || b.MeasuredAt.IsZero() {
		t.Fatalf("Bucket = %+v", b)
	}

	// Its variables are kept and nothing is deployed for them.
	if err := s.SetVariable(ctx, id, a.Name, VariableInput{Key: "NOTE", Value: "media"}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if _, err := s.liveOperation(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Errorf("a variable on a bucket started a deploy: %v", err)
	}
	if _, err := s.Scale(ctx, id, a.Name, 2); !errors.Is(err, ErrNoWorkload) {
		t.Errorf("Scale = %v", err)
	}
	if err := s.Redeploy(ctx, id, a.Name); !errors.Is(err, ErrNoWorkload) {
		t.Errorf("Redeploy = %v", err)
	}
	if _, err := s.GenerateDomain(ctx, id, a.Name, 0); !errors.Is(err, ErrNoWorkload) {
		t.Errorf("GenerateDomain = %v", err)
	}
	if _, err := s.AttachVolume(ctx, id, a.Name, VolumeInput{Name: "data", MountPath: "/data", SizeBytes: 1 << 30}); !errors.Is(err, ErrNoWorkload) {
		t.Errorf("AttachVolume = %v", err)
	}

	// An environment's copy is a bucket of its own.
	if _, _, err := s.CreateEnvironment(ctx, id, p.Slug, "staging", DefaultEnvironment); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT a.name, b.name FROM buckets b JOIN apps a ON a.id = b.app_id WHERE a.owner_id = $1 ORDER BY a.environment`, id)
	if err != nil {
		t.Fatal(err)
	}
	var apps, buckets []string
	for rows.Next() {
		var app, bkt string
		if err := rows.Scan(&app, &bkt); err != nil {
			t.Fatal(err)
		}
		apps, buckets = append(apps, app), append(buckets, bkt)
	}
	if len(buckets) != 2 || buckets[0] == buckets[1] {
		t.Fatalf("buckets after copying the environment = %v (%v), want two of their own", buckets, apps)
	}

	for _, app := range apps {
		if err := s.Delete(ctx, id, app); err != nil {
			t.Fatalf("Delete %s: %v", app, err)
		}
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM buckets b JOIN apps a ON a.id = b.app_id WHERE a.owner_id = $1`, id).Scan(&left); err != nil || left != 0 {
		t.Fatalf("buckets left = %d, %v", left, err)
	}
	if err := files.Put(ctx, "after.txt", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("the deleted bucket's key still writes")
	}
}

func TestNoBucketsWithoutAStore(t *testing.T) {
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	id := owner(t, s, pool, "svc-no-buckets")
	ctx := context.Background()
	p, err := s.CreateProject(ctx, id, "Media")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeployBucket(ctx, id, p.ID); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("DeployBucket with no store = %v", err)
	}
}

// A team's quota for buckets is shared among them, and kept by the store: each
// bucket may hold what the team may, less what its others hold, and a write
// past that is refused. No quota, no limit.
func TestATeamsBucketsShareItsQuota(t *testing.T) {
	g := testGarage(t)
	s, _, pool := testService(t, Options{Keeper: testKeeper(t), Buckets: g})
	id := owner(t, s, pool, "svc-bucket-quota")
	ctx := context.Background()
	p, err := s.CreateProject(ctx, id, "Media")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuota(ctx, id, Quota{BucketBytes: 30}); err != nil {
		t.Fatal(err)
	}
	first, err := s.DeployBucket(ctx, id, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.DeployBucket(ctx, id, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Delete(context.Background(), id, first.Name)
		_ = s.Delete(context.Background(), id, second.Name)
	})
	limit := func(a App) int64 {
		t.Helper()
		_, c, err := s.storedBucket(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		l, err := g.Limit(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if l := limit(second); l != 30 {
		t.Fatalf("a new bucket's limit = %d, want the team's 30", l)
	}

	files, err := s.BucketFiles(ctx, id, first.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Put(ctx, "a.txt", strings.NewReader(strings.Repeat("a", 20)), 20, ""); err != nil {
		t.Fatal(err)
	}
	// Garage counts in the background.
	for range 50 {
		if err := s.MeasureBucket(ctx, id, first.Name); err != nil {
			t.Fatal(err)
		}
		if b, _ := s.Bucket(ctx, id, first.Name); b.Bytes == 20 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if l := limit(second); l != 10 {
		t.Fatalf("with 20 of 30 in the first, the second's limit = %d, want 10", l)
	}
	b, err := s.Bucket(ctx, id, second.Name)
	if err != nil || b.TeamLimit != 30 || b.TeamBytes != 20 {
		t.Fatalf("the second's page = %+v, %v; want 20 of the team's 30", b, err)
	}
	others, err := s.BucketFiles(ctx, id, second.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := others.Put(ctx, "b.txt", strings.NewReader(strings.Repeat("b", 15)), 15, ""); !errors.Is(err, bucket.ErrFull) {
		t.Fatalf("a write past the team's quota = %v, want ErrFull", err)
	}
	if u, err := s.TeamUsage(ctx, id); err != nil || u.Usage.BucketBytes != 20 || u.Quota.BucketBytes != 30 {
		t.Fatalf("TeamUsage = %+v, %v", u, err)
	}

	// No quota, and the install's room still bounds it.
	if err := s.SetQuota(ctx, id, Quota{}); err != nil {
		t.Fatal(err)
	}
	if l := limit(second); l <= 30 {
		t.Fatalf("with no quota the limit = %d, want the install's share, past the old quota", l)
	}

	// The store's disk with 10 bytes free past its reserve: the two buckets
	// together may grow by no more than that, so neither may take 11, and
	// the page says it was the install's room and not the plan.
	s.disk = newDiskSource(func(context.Context) (bucket.Disk, error) {
		return bucket.Disk{Total: 4 << 30, Available: 2<<30 + 10}, nil
	}, s.log)
	s.limitBuckets(ctx, false)
	var grown int64
	for _, a := range []App{first, second} {
		b, err := s.Bucket(ctx, id, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		l := limit(a)
		if l < 1 {
			t.Fatalf("%s: no limit with the store nearly full", a.Name)
		}
		grown += max(l-b.Bytes, 0)
		f, err := s.BucketFiles(ctx, id, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Put(ctx, "c.txt", strings.NewReader(strings.Repeat("c", 11)), 11, ""); !errors.Is(err, bucket.ErrFull) {
			t.Fatalf("%s: a write past the install's room = %v, want ErrFull", a.Name, err)
		}
		if b.OverPlan(11) {
			t.Fatalf("%s: a team with no quota was told it is past its plan", a.Name)
		}
	}
	if grown > 10+2 {
		t.Fatalf("the two buckets may grow by %d together, with 10 bytes free", grown)
	}
	// The pass reads it again, and pages are shown what it read.
	if _, err := s.disk.get(ctx, true); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b := snap.Buckets; b == nil || !b.Known || b.Total != 4<<30 || b.Reserve != 2<<30 || b.Free != 10 ||
		b.Percent() != 99 || snap.Level == CapacityFull {
		t.Fatalf("Capacity's buckets = %+v at %s; want 10 bytes free of a 4 GiB disk keeping 2 GiB, not yet full",
			b, snap.Level)
	}

	// Room again, and the write fits: the bucket about to be written is
	// given its part of the room first.
	s.disk = newDiskSource(func(context.Context) (bucket.Disk, error) {
		return bucket.Disk{Total: 1 << 50, Available: 1 << 49}, nil
	}, s.log)
	if err := s.PrepareBucketUpload(ctx, id, second.Name); err != nil {
		t.Fatal(err)
	}
	if err := others.Put(ctx, "c.txt", strings.NewReader("cccccc"), 6, ""); err != nil {
		t.Fatalf("a write once the store has room: %v", err)
	}

	// A store that cannot be read loosens nothing: with no quota the limit
	// stays, and a quota set meanwhile tightens it.
	s.disk = newDiskSource(func(context.Context) (bucket.Disk, error) { return bucket.Disk{}, bucket.ErrUnavailable }, s.log)
	before := limit(second)
	s.limitBuckets(ctx, true)
	if l := limit(second); l != before || l == 0 {
		t.Fatalf("with no quota and no disk to read the limit went from %d to %d, want it kept", before, l)
	}
	if err := s.SetQuota(ctx, id, Quota{BucketBytes: 40}); err != nil {
		t.Fatal(err)
	}
	if l := limit(second); l > 40 {
		t.Fatalf("a quota of 40 with no disk to read left the limit at %d", l)
	}
	if snap, err := s.Capacity(ctx); err != nil || snap.Buckets == nil || snap.Buckets.Known || snap.Buckets.Reason == "" {
		t.Fatalf("Capacity with the disk unread = %+v, %v", snap.Buckets, err)
	}
}

// The store checks a limit only when an upload is completed, so the parts of
// one that has run past the bucket's limit are ended on the next pass, and the
// bucket keeps the rule that ends any upload left unfinished for a day.
func TestAPassEndsUploadsPastABucketsLimit(t *testing.T) {
	g := testGarage(t)
	s, _, pool := testService(t, Options{Keeper: testKeeper(t), Buckets: g})
	id := owner(t, s, pool, "svc-bucket-uploads")
	ctx := context.Background()
	p, err := s.CreateProject(ctx, id, "Media")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuota(ctx, id, Quota{BucketBytes: 30}); err != nil {
		t.Fatal(err)
	}
	a, err := s.DeployBucket(ctx, id, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Delete(context.Background(), id, a.Name) })
	_, c, err := s.storedBucket(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := g.Limit(ctx, c.ID); err != nil || l != 30 {
		t.Fatalf("the bucket's limit = %d, %v; want its team's 30", l, err)
	}

	// 100 bytes of parts in a bucket limited to 30: the store takes them.
	s3, err := backup.NewS3(g.S3URL, g.Region, c.Name, "", c.KeyID, c.Secret)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, u string, body string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %s %s", method, resp.Status, b)
		}
		return string(b)
	}
	u, _ := s3.PresignQuery(http.MethodPost, "big.bin", url.Values{"uploads": {""}}, time.Minute, time.Now())
	started := send(http.MethodPost, u, "")
	upload := started[strings.Index(started, "<UploadId>")+len("<UploadId>") : strings.Index(started, "</UploadId>")]
	u, _ = s3.PresignQuery(http.MethodPut, "big.bin", url.Values{"partNumber": {"1"}, "uploadId": {upload}}, time.Minute, time.Now())
	send(http.MethodPut, u, strings.Repeat("p", 100))

	for range 50 {
		if got, err := g.Usage(ctx, c.ID); err == nil && got.Uploading == 100 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.bucketPass(ctx)
	var left bucket.Usage
	for range 50 {
		if left, err = g.Usage(ctx, c.ID); err == nil && left.Uploading == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if left.Uploading != 0 {
		t.Fatalf("after a pass the bucket still has %d bytes of an upload past its limit", left.Uploading)
	}
	u, _ = s3.PresignBucket(http.MethodGet, url.Values{"lifecycle": {""}}, time.Minute, time.Now())
	if rules := send(http.MethodGet, u, ""); !strings.Contains(rules, "AbortIncompleteMultipartUpload") {
		t.Fatalf("the bucket's lifecycle = %s, want the rule that ends unfinished uploads", rules)
	}
}

// An app's upload into a bucket left with an idle bucket's small limit, and
// within its team's share, is what makes the bucket want room: the pass gives
// it that room before it looks for uploads past a limit, and so does not end
// the upload it is making room for.
func TestAPassKeepsAnUploadItGivesRoomTo(t *testing.T) {
	g := testGarage(t)
	s, _, pool := testService(t, Options{Keeper: testKeeper(t), Buckets: g})
	id := owner(t, s, pool, "svc-bucket-room")
	ctx := context.Background()
	p, err := s.CreateProject(ctx, id, "Media")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuota(ctx, id, Quota{BucketBytes: 1000}); err != nil {
		t.Fatal(err)
	}
	a, err := s.DeployBucket(ctx, id, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Delete(context.Background(), id, a.Name) })
	row, c, err := s.storedBucket(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	// As a pass left it when the store's room was shared between many.
	if err := g.SetLimit(ctx, c.ID, 30); err != nil {
		t.Fatal(err)
	}
	if err := s.q.SetBucketLimit(ctx, dbgen.SetBucketLimitParams{AppID: row.AppID, LimitBytes: 30}); err != nil {
		t.Fatal(err)
	}

	s3, err := backup.NewS3(g.S3URL, g.Region, c.Name, "", c.KeyID, c.Secret)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, u, body string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %s %s", method, resp.Status, b)
		}
		return string(b)
	}
	u, _ := s3.PresignQuery(http.MethodPost, "big.bin", url.Values{"uploads": {""}}, time.Minute, time.Now())
	started := send(http.MethodPost, u, "")
	upload := started[strings.Index(started, "<UploadId>")+len("<UploadId>") : strings.Index(started, "</UploadId>")]
	u, _ = s3.PresignQuery(http.MethodPut, "big.bin", url.Values{"partNumber": {"1"}, "uploadId": {upload}}, time.Minute, time.Now())
	send(http.MethodPut, u, strings.Repeat("p", 100))
	for range 50 {
		if got, err := g.Usage(ctx, c.ID); err == nil && got.Uploading == 100 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	s.bucketPass(ctx)
	if got, err := g.Usage(ctx, c.ID); err != nil || got.Uploading != 100 {
		t.Fatalf("after a pass the upload holds %d bytes (%v); want its 100 kept", got.Uploading, err)
	}
	if l, err := g.Limit(ctx, c.ID); err != nil || l != 1000 {
		t.Fatalf("after a pass the bucket's limit = %d, %v; want its team's 1000", l, err)
	}
}

func TestAllocateBuckets(t *testing.T) {
	const mib, gib = 1 << 20, 1 << 30
	free := func(n int64) *bucketRoom { return &bucketRoom{Free: n} }
	idle := func(size int64) bucketDemand { return bucketDemand{Size: size} }
	wants := func(size int64) bucketDemand { return bucketDemand{Size: size, Wants: true} }
	for _, c := range []struct {
		name string
		in   []bucketDemand
		room *bucketRoom
		want []int64
	}{
		{"an idle bucket keeps the floor; the one being written to takes the rest",
			[]bucketDemand{idle(0), wants(5 * gib)}, free(10 * gib), []int64{5 * gib / 2, 5*gib + 15*gib/2}},
		{"an idle bucket keeps its limit while it has room under it",
			[]bucketDemand{{Size: gib, Limit: gib + 5*gib/2}, wants(0)}, free(10 * gib), []int64{gib + 5*gib/2, 15 * gib / 2}},
		{"the floor is never more than half the free room shared",
			[]bucketDemand{idle(0), idle(0), idle(0), idle(0)}, free(8 * mib), []int64{mib, mib, mib, mib}},
		{"buckets being written to share the rest",
			[]bucketDemand{wants(0), wants(0), idle(0)}, free(12 * gib), []int64{5 * gib, 5 * gib, 2 * gib}},
		{"what a team's share leaves no room for goes to the others",
			[]bucketDemand{{Size: 20, TeamLimit: 30, Wants: true}, wants(0)}, free(10 * gib), []int64{30, 10*gib - mib}},
		{"the team's share is tighter than the floor",
			[]bucketDemand{{Size: 0, TeamLimit: 30, TeamOthers: 20}}, free(10 * gib), []int64{10}},
		{"a bucket past its team's share may take nothing",
			[]bucketDemand{{Size: 50, TeamLimit: 30}}, free(10 * gib), []int64{30}},
		{"one that used most of what it had wants more, grown or not",
			[]bucketDemand{{Size: 100 * mib, Limit: 100*mib + mib}, idle(0)}, free(10 * gib), []int64{100*mib + 15*gib/2, 5 * gib / 2}},
		{"a full store", []bucketDemand{idle(5), wants(0)}, free(0), []int64{5, 1}},
		{"no room to read keeps a limit, or the team's share when tighter",
			[]bucketDemand{{Size: 5, Limit: 50}, {Size: 5, Limit: 50, TeamLimit: 30}, {Size: 5}, {Size: 5, TeamLimit: 30}},
			nil, []int64{50, 30, 0, 30}},
		{"no room to read never loosens a limit for a bigger quota",
			[]bucketDemand{{Size: 5, Limit: 20, TeamLimit: 1000}}, nil, []int64{20}},
	} {
		if got := allocateBuckets(c.in, c.room); !slices.Equal(got, c.want) {
			t.Errorf("%s: allocateBuckets = %v, want %v", c.name, got, c.want)
		}
	}

	// However the buckets stand, together they may grow by no more than is
	// free, and a byte for each empty bucket given nothing.
	shapes := [][]bucketDemand{
		{wants(0)}, {idle(1), idle(2), wants(3)}, {wants(100), wants(0), idle(0), idle(0), {Size: 7, TeamLimit: 9}},
		{{Size: 1, TeamLimit: 1 << 40, Wants: true}, {Size: 0, TeamLimit: 1, Wants: true}, idle(0)},
	}
	for _, in := range shapes {
		for _, room := range []int64{0, 1, 97, 3 * mib, 10*gib + 12345} {
			var grow, empties int64
			for i, l := range allocateBuckets(in, free(room)) {
				grow += max(l-in[i].Size, 0)
				if in[i].Size == 0 {
					empties++
				}
			}
			if grow > room+empties {
				t.Errorf("%+v with %d free grow by %d together", in, room, grow)
			}
		}
	}
}

func TestBucketReserve(t *testing.T) {
	for total, want := range map[int64]int64{
		20 << 30: 2 << 30, 100 << 30: 10 << 30, 5 << 30: 2 << 30, 1 << 30: 512 << 20, 0: 0,
	} {
		if got := bucketReserve(total); got != want {
			t.Errorf("bucketReserve(%d) = %d, want %d", total, got, want)
		}
	}
}

func TestBucketCapacity(t *testing.T) {
	c := newBucketCapacity(bucket.Disk{Total: 20 << 30, Available: 4 << 30}, 3, 15<<30)
	if c.Reserve != 2<<30 || c.Usable != 18<<30 || c.Used != 16<<30 || c.Free != 2<<30 ||
		c.Percent() != 88 || c.level(80) != CapacityWarn || c.level(90) != CapacityOK {
		t.Fatalf("a 20 GiB disk with 4 GiB available = %+v, %d%%", c, c.Percent())
	}
	// Nearly full is not full: 99%, rounded down, while anything is free.
	c = newBucketCapacity(bucket.Disk{Total: 20 << 30, Available: 2<<30 + 1}, 3, 15<<30)
	if c.Free != 1 || c.Percent() != 99 || c.level(80) != CapacityWarn {
		t.Fatalf("a byte free = %+v, %d%%, %s; want 99%%, warned", c, c.Percent(), c.level(80))
	}
	c = newBucketCapacity(bucket.Disk{Total: 20 << 30, Available: 1 << 30}, 3, 15<<30)
	if c.Free != 0 || c.Percent() < 100 || c.level(80) != CapacityFull {
		t.Fatalf("a 20 GiB disk with 1 GiB available = %+v, %s; want nothing free, full", c, c.level(80))
	}
	if (BucketCapacity{}).level(80) != CapacityUnknown || (BucketCapacity{}).Percent() != 0 {
		t.Fatal("a disk not read is not unknown")
	}
}

// Limits read the disk afresh; pages are shown the last reading at once, and
// read it again in the background when it is old.
func TestTheDiskIsReadFreshForLimitsAndNeverWaitedOnByPages(t *testing.T) {
	var mu sync.Mutex
	reads := int64(0)
	release := make(chan struct{})
	d := newDiskSource(func(context.Context) (bucket.Disk, error) {
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n == 3 {
			<-release
		}
		return bucket.Disk{Total: n}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	d.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	ctx := context.Background()

	if _, err := d.cached(); !errors.Is(err, errDiskNotRead) {
		t.Fatalf("before any reading, a page is shown %v", err)
	}
	waitFor := func(want int64) {
		t.Helper()
		for range 100 {
			if got, err := d.cached(); err == nil && got.Total == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("pages never saw reading %d", want)
	}
	waitFor(1)
	if got, err := d.get(ctx, true); err != nil || got.Total != 2 {
		t.Fatalf("a fresh reading = %d, %v; want the second", got.Total, err)
	}
	if got, _ := d.get(ctx, false); got.Total != 2 {
		t.Fatalf("a reading within diskTTL = %d, want the second again", got.Total)
	}
	// Old, and the next reading hangs: the page is shown the last at once.
	mu.Lock()
	now = now.Add(diskTTL)
	mu.Unlock()
	start := time.Now()
	if got, _ := d.cached(); got.Total != 2 || time.Since(start) > time.Second {
		t.Fatalf("a page waited %v for reading %d", time.Since(start), got.Total)
	}
	close(release)
	waitFor(3)
}

// A stand-in for the store, so limits can be tested with no Garage: it keeps
// the limits it is given, can refuse them, and checks on every one that the
// buckets together may grow by no more than is free.
type fakeBucketStore struct {
	t     *testing.T
	mu    sync.Mutex
	disk  bucket.Disk
	err   error
	fail  map[string]bool
	limit map[string]int64
	size  map[string]int64
	calls []string
}

func (f *fakeBucketStore) Disk(context.Context) (bucket.Disk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disk, f.err
}

func (f *fakeBucketStore) SetLimit(_ context.Context, id string, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[id] {
		return errors.New("the store refused")
	}
	f.limit[id] = n
	f.calls = append(f.calls, id)
	// Checked only while the store takes every limit and its disk can be
	// read: a limit it refused to lower is one the engine cannot help.
	refusing := false
	for _, v := range f.fail {
		refusing = refusing || v
	}
	if f.err == nil && !refusing {
		if grow, most := f.grownLocked(); grow > most {
			f.t.Errorf("after setting %s to %d the buckets may grow by %d, with %d free", id, n, grow, most)
		}
	}
	return nil
}

// grownLocked is how much the buckets it has limits for may grow together,
// and the most they should: what is free, and a byte for each empty one.
func (f *fakeBucketStore) grownLocked() (grow, most int64) {
	most = newBucketCapacity(f.disk, 0, 0).Free
	for id, l := range f.limit {
		if l == 0 {
			return math.MaxInt64, most
		}
		grow += max(l-f.size[id], 0)
		if f.size[id] == 0 {
			most++
		}
	}
	return grow, most
}

func (f *fakeBucketStore) get(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limit[id]
}

// The install's buckets share the store's free room, with no Garage: what
// CI runs. Two teams, a bucket made after another was given the room, a
// limit the store refuses to lower, a disk that cannot be read, and limits
// set from many requests at once.
func TestBucketLimitsShareTheStoreAcrossTeams(t *testing.T) {
	s, _, pool := testService(t, Options{})
	ctx := context.Background()
	a, b := owner(t, s, pool, "svc-bucket-limits-a"), owner(t, s, pool, "svc-bucket-limits-b")
	const gib = 1 << 30
	// 100 GiB keeps 10 free; 20 available leaves 10 for buckets.
	store := &fakeBucketStore{t: t, disk: bucket.Disk{Total: 100 * gib, Available: 20 * gib},
		fail: map[string]bool{}, limit: map[string]int64{}, size: map[string]int64{}}
	s.limits = store
	s.disk = newDiskSource(store.Disk, s.log)

	// Every bucket on the install counts, a test's leftovers included.
	rows, err := pool.Query(ctx, `SELECT garage_id, size_bytes FROM buckets`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var size int64
		if err := rows.Scan(&id, &size); err != nil {
			t.Fatal(err)
		}
		store.size[id] = size
	}
	rows.Close()

	n := 0
	add := func(team string, size int64, wants bool) string {
		t.Helper()
		n++
		app, err := s.Create(ctx, team, CreateInput{Name: fmt.Sprintf("b%d", n), Image: "nginx:alpine", Replicas: 1})
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("garage-%s-%d", team, n)
		// Made as makeBucket makes one: one byte, until its limit is set.
		store.mu.Lock()
		store.size[id], store.limit[id] = size, 1
		store.mu.Unlock()
		if _, err := pool.Exec(ctx, `INSERT INTO buckets (app_id, owner_id, garage_id, name, key_id, sealed_secret, size_bytes, wants_room_at, limit_bytes)
			VALUES ($1, $2, $3, $4, 'GK', '\x00', $5, CASE WHEN $6 THEN now() END, 1)`,
			app.ID, team, id, fmt.Sprintf("%s-%d", team, n), size, wants); err != nil {
			t.Fatal(err)
		}
		return id
	}
	recorded := func(id string) int64 {
		t.Helper()
		var l int64
		if err := pool.QueryRow(ctx, `SELECT limit_bytes FROM buckets WHERE garage_id = $1`, id).Scan(&l); err != nil {
			t.Fatal(err)
		}
		return l
	}

	// Team A writing to one bucket: it takes most of the room.
	a1 := add(a, 0, true)
	s.limitBuckets(ctx, false)
	if l := store.get(a1); l < 5*gib {
		t.Fatalf("the one bucket being written to may hold %d of 10 GiB free", l)
	}

	// Team B's first bucket, made after A's was given the room: it has a
	// floor at once, taken from A's, which was lowered first.
	store.mu.Lock()
	store.calls = nil
	store.mu.Unlock()
	b1 := add(b, 0, false)
	s.limitBuckets(ctx, false)
	if l := store.get(b1); l < 1<<20 {
		t.Fatalf("a new bucket on an install whose room was given out may hold %d", l)
	}
	store.mu.Lock()
	calls := slices.Clone(store.calls)
	store.mu.Unlock()
	if ia, ib := slices.Index(calls, a1), slices.Index(calls, b1); ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("set %v: want A's lowered before B's raised", calls)
	}

	// The store refuses to lower A's: the record stays at what the store
	// still enforces, and the next setting lowers it.
	before := store.get(a1)
	store.mu.Lock()
	store.fail[a1] = true
	store.disk.Available = 12 * gib
	store.mu.Unlock()
	s.limitBuckets(ctx, false)
	if r := recorded(a1); r != before || store.get(a1) != before {
		t.Fatalf("a lowering the store refused: recorded %d, store %d; want both still %d", r, store.get(a1), before)
	}
	store.mu.Lock()
	store.fail[a1] = false
	store.mu.Unlock()
	s.limitBuckets(ctx, false)
	if l := store.get(a1); l >= before || recorded(a1) != l {
		t.Fatalf("once the store takes it, A's limit = %d (recorded %d), want below %d", l, recorded(a1), before)
	}

	// A disk that cannot be read: nothing loosens, a quota tightens.
	store.mu.Lock()
	store.err = bucket.ErrUnavailable
	store.disk.Available = 90 * gib
	store.mu.Unlock()
	held := store.get(a1)
	s.limitBuckets(ctx, true)
	if l := store.get(a1); l != held {
		t.Fatalf("with the disk unread A's limit went from %d to %d", held, l)
	}
	if err := s.SetQuota(ctx, a, Quota{BucketBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if l := store.get(a1); l != 1<<20 {
		t.Fatalf("a 1 MiB quota with the disk unread left A's limit at %d", l)
	}
	if err := s.SetQuota(ctx, a, Quota{}); err != nil {
		t.Fatal(err)
	}
	if l := store.get(a1); l != 1<<20 {
		t.Fatalf("lifting the quota with the disk unread loosened A's limit to %d", l)
	}

	// Readable again, and many requests at once from both teams, each
	// with buckets wanting room: the store checks every limit it is given.
	store.mu.Lock()
	store.err = nil
	store.disk.Available = 20 * gib
	store.mu.Unlock()
	var wg sync.WaitGroup
	for i := range 8 {
		team := a
		if i%2 == 1 {
			team = b
		}
		id := add(team, int64(i)<<20, i%3 == 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.Exec(ctx, `UPDATE buckets SET wants_room_at = now() WHERE garage_id = $1`, id); err != nil {
				t.Error(err)
			}
			s.limitBuckets(ctx, false)
		}()
	}
	wg.Wait()
	store.mu.Lock()
	grow, most := store.grownLocked()
	store.mu.Unlock()
	if grow > most {
		t.Fatalf("after setting limits from many requests at once the buckets may grow by %d, with %d free", grow, most)
	}
}
