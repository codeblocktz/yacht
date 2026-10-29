package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/bucket"
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
	// split them, so neither may take 6 more, and the page says it was the
	// install's room and not the plan.
	s.disk.read = func(context.Context) (bucket.Disk, error) {
		return bucket.Disk{Total: 4 << 30, Available: 1<<30 + 10}, nil
	}
	s.limitBuckets(ctx, id)
	for _, a := range []App{first, second} {
		b, err := s.Bucket(ctx, id, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		if l := limit(a); l < 1 || l > max(b.Bytes+5, 1) {
			t.Fatalf("%s holding %d, with 10 bytes free between two buckets: limit %d", a.Name, b.Bytes, l)
		}
		f, err := s.BucketFiles(ctx, id, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Put(ctx, "c.txt", strings.NewReader("cccccc"), 6, ""); !errors.Is(err, bucket.ErrFull) {
			t.Fatalf("%s: a write past its part of the install's room = %v, want ErrFull", a.Name, err)
		}
		if b.OverPlan(6) {
			t.Fatalf("%s: a team with no quota was told it is past its plan", a.Name)
		}
	}
	snap, err := s.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b := snap.Buckets; b == nil || !b.Known || b.Total != 4<<30 || b.Reserve != 1<<30 || b.Free != 10 ||
		b.Percent() != 100 || snap.Level != CapacityFull {
		t.Fatalf("Capacity's buckets = %+v at %s; want 10 bytes free of a 4 GiB disk keeping 1 GiB, full", b, snap.Level)
	}

	// Room again, and the write fits.
	s.disk.read = func(context.Context) (bucket.Disk, error) {
		return bucket.Disk{Total: 1 << 50, Available: 1 << 49}, nil
	}
	s.limitBuckets(ctx, id)
	if err := others.Put(ctx, "c.txt", strings.NewReader("cccccc"), 6, ""); err != nil {
		t.Fatalf("a write once the store has room: %v", err)
	}

	// A store that cannot be read holds buckets to their quotas alone.
	s.disk.read = func(context.Context) (bucket.Disk, error) { return bucket.Disk{}, bucket.ErrUnavailable }
	s.limitBuckets(ctx, id)
	if l := limit(second); l != 0 {
		t.Fatalf("with no quota and no disk to read the limit = %d, want none", l)
	}
	if snap, err := s.Capacity(ctx); err != nil || snap.Buckets == nil || snap.Buckets.Known || snap.Buckets.Reason == "" {
		t.Fatalf("Capacity with the disk unread = %+v, %v", snap.Buckets, err)
	}
}

func TestBucketShare(t *testing.T) {
	for _, c := range []struct{ limit, others, want int64 }{
		{0, 100, 0}, {30, 0, 30}, {30, 20, 10}, {30, 30, 1}, {30, 50, 1},
	} {
		if got := bucketShare(c.limit, c.others); got != c.want {
			t.Errorf("bucketShare(%d, %d) = %d, want %d", c.limit, c.others, got, c.want)
		}
	}
}

func TestBucketLimits(t *testing.T) {
	free := func(free, buckets, granted int64) *installRoom {
		return &installRoom{Free: free, Buckets: buckets, Granted: granted}
	}
	for _, c := range []struct {
		name  string
		team  int64
		sizes []int64
		room  *installRoom
		want  []int64
	}{
		{"no room to read, no quota", 0, []int64{5, 0}, nil, []int64{0, 0}},
		{"no room to read, the team's share", 30, []int64{20, 0}, nil, []int64{30, 10}},
		{"the install's room split between every bucket", 0, []int64{10, 0}, free(100, 4, 0), []int64{35, 25}},
		{"room other teams were given is not given again", 0, []int64{10, 0}, free(100, 4, 90), []int64{15, 5}},
		{"other teams given more than is free", 0, []int64{10, 0}, free(100, 4, 500), []int64{10, 1}},
		{"the team's share is smaller", 30, []int64{20, 0}, free(1000, 2, 0), []int64{30, 10}},
		{"the install's is smaller", 1000, []int64{0}, free(10, 1, 0), []int64{10}},
		{"a full store", 0, []int64{5, 0}, free(0, 2, 0), []int64{5, 1}},
		{"never fewer buckets than the team's", 0, []int64{0, 0}, free(10, 0, 0), []int64{5, 5}},
	} {
		got := bucketLimits(c.team, c.sizes, c.room)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: bucketLimits(%d, %v, %+v) = %v, want %v", c.name, c.team, c.sizes, c.room, got, c.want)
		}
	}

	// However the room is split, the team's buckets growing together to
	// their limits take no more than is free and not already given out.
	for _, sizes := range [][]int64{{0}, {1, 2, 3}, {100, 0, 0, 0, 0}} {
		for _, room := range []*installRoom{free(97, 9, 0), free(97, 5, 40), free(1<<30, 7, 1<<29)} {
			var grow int64
			for i, l := range bucketLimits(0, sizes, room) {
				grow += l - sizes[i]
			}
			if grow > max(room.Free-room.Granted, 0) {
				t.Errorf("sizes %v in %+v grow by %d together, past what is free", sizes, room, grow)
			}
		}
	}
}

func TestBucketReserve(t *testing.T) {
	for total, want := range map[int64]int64{
		20 << 30: 2 << 30, 100 << 30: 10 << 30, 5 << 30: 1 << 30, 1 << 30: 512 << 20, 0: 0,
	} {
		if got := bucketReserve(total); got != want {
			t.Errorf("bucketReserve(%d) = %d, want %d", total, got, want)
		}
	}
}

func TestBucketCapacity(t *testing.T) {
	c := newBucketCapacity(bucket.Disk{Total: 20 << 30, Available: 4 << 30}, 3, 15<<30)
	if c.Reserve != 2<<30 || c.Usable != 18<<30 || c.Used != 16<<30 || c.Free != 2<<30 ||
		c.Percent() != 89 || c.level(80) != CapacityWarn || c.level(90) != CapacityOK {
		t.Fatalf("a 20 GiB disk with 4 GiB available = %+v, %d%%", c, c.Percent())
	}
	c = newBucketCapacity(bucket.Disk{Total: 20 << 30, Available: 1 << 30}, 3, 15<<30)
	if c.Free != 0 || c.level(80) != CapacityFull {
		t.Fatalf("a 20 GiB disk with 1 GiB available = %+v, %s; want nothing free, full", c, c.level(80))
	}
	if (BucketCapacity{}).level(80) != CapacityUnknown || (BucketCapacity{}).Percent() != 0 {
		t.Fatal("a disk not read is not unknown")
	}
}

// The pages read the disk at most every diskTTL; limits are set from a
// reading of their own.
func TestTheDiskIsReadFreshForLimits(t *testing.T) {
	reads := 0
	now := time.Now()
	d := &diskSource{
		read: func(context.Context) (bucket.Disk, error) {
			reads++
			return bucket.Disk{Total: int64(reads)}, nil
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: func() time.Time { return now },
	}
	ctx := context.Background()
	for _, step := range []struct {
		fresh bool
		later time.Duration
		want  int64
	}{{false, 0, 1}, {false, 0, 1}, {true, 0, 2}, {false, 0, 2}, {false, diskTTL, 3}} {
		now = now.Add(step.later)
		if got, err := d.get(ctx, step.fresh); err != nil || got.Total != step.want {
			t.Fatalf("get(fresh %v) after %v = %d, %v; want reading %d", step.fresh, step.later, got.Total, err, step.want)
		}
	}
}
