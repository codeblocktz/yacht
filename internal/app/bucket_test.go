package app

import (
	"context"
	"errors"
	"os"
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

	if err := s.SetQuota(ctx, id, Quota{}); err != nil {
		t.Fatal(err)
	}
	if l := limit(second); l != 0 {
		t.Fatalf("with no quota the limit = %d, want none", l)
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
