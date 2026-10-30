package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/bucket"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Errors for buckets.
var (
	// ErrNotABucket is a bucket's page asked of a service that is not one.
	ErrNotABucket = errors.New("app: not a bucket")

	// ErrNoWorkload is something only a running service can do — deploy,
	// scale, restart, a domain, a volume, a move — asked of a bucket.
	ErrNoWorkload = errors.New("app: a bucket runs nothing to deploy, scale, restart or route to")

	// ErrNoBuckets is Add › Bucket on an install with no object store.
	ErrNoBuckets = errors.New("app: this install has no object store for buckets")
)

// The variables a bucket is given, which an app reads as ${{bucket.KEY}}:
// Railway's names, so an app moved from there finds the ones it uses.
const (
	BucketVar       = "BUCKET"
	BucketEndpoint  = "ENDPOINT"
	BucketRegion    = "REGION"
	BucketKeyID     = "ACCESS_KEY_ID"
	BucketSecretKey = "SECRET_ACCESS_KEY"
)

// bucketMade is a bucket made on the store for a create that has not yet
// committed: what to record, what to seal, and what to take back.
type bucketMade struct {
	created   bucket.Created
	sealed    []byte
	variables []VariableInput
}

func (s *Service) makeBucket(ctx context.Context, service string) (*bucketMade, error) {
	if s.buckets == nil {
		return nil, ErrNoBuckets
	}
	if !s.keeper.Configured() {
		return nil, fmt.Errorf("%w — a bucket needs one to store its key", ErrNoSecretKey)
	}
	c, err := s.buckets.Create(ctx, service)
	if err != nil {
		return nil, err
	}
	// Never without a limit, which the store would read as none: one byte
	// until limitBuckets gives it its part of the room, once it is recorded.
	if err := s.buckets.SetLimit(ctx, c.ID, 1); err != nil {
		s.unmakeBucket(c)
		return nil, err
	}
	// Before anybody has its key. Not a reason to refuse the bucket: every
	// measuring pass adds the rule where it is missing.
	if err := s.buckets.Files(c).KeepAbortRule(ctx); err != nil {
		s.log.Warn("buckets: the rule that ends unfinished uploads",
			slog.String("bucket", c.Name), slog.String("error", err.Error()))
	}
	sealed, err := s.keeper.Seal(c.Secret)
	if err != nil {
		s.unmakeBucket(c)
		return nil, err
	}
	return &bucketMade{created: c, sealed: sealed, variables: []VariableInput{
		{Key: BucketVar, Value: c.Name},
		{Key: BucketEndpoint, Value: s.buckets.Endpoint},
		{Key: BucketRegion, Value: s.buckets.Region},
		{Key: BucketKeyID, Value: c.KeyID},
		{Key: BucketSecretKey, Value: c.Secret, Secret: true},
	}}, nil
}

// unmakeBucket takes back a bucket a create made and did not keep. Empty, so
// this is a delete of the bucket and its key alone.
func (s *Service) unmakeBucket(c bucket.Created) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.buckets.Delete(ctx, c); err != nil {
		s.log.Error("take back a bucket nothing records",
			slog.String("bucket", c.Name), slog.String("error", err.Error()))
	}
}

// bucketNames is what Add › Bucket calls a bucket, as Railway does.
const bucketNames = "bucket"

// DeployBucket is Add › Bucket: a bucket in a project, named "bucket" (a
// second is bucket-2), with nothing asked.
func (s *Service) DeployBucket(ctx context.Context, ownerID string, projectID uuid.UUID) (App, error) {
	for attempt := 0; ; attempt++ {
		name, err := s.freeName(ctx, ownerID, bucketNames)
		if err != nil {
			return App{}, err
		}
		a, err := s.Create(ctx, ownerID, CreateInput{
			Source: SourceBucket, Name: name, ProjectID: projectID,
		})
		if errors.Is(err, ErrNameTaken) && attempt == 0 {
			continue
		}
		return a, err
	}
}

// Bucket is a bucket as its page shows it.
type Bucket struct {
	Name     string
	Endpoint string
	Region   string
	// Bytes and Objects are what it held when last measured, MeasuredAt
	// when that was; zero before the first measuring.
	Bytes, Objects int64
	MeasuredAt     time.Time

	// TeamLimit is the most the team's buckets may hold together, zero for
	// no limit, and TeamBytes what they all held when last measured.
	TeamLimit, TeamBytes int64
}

// OverPlan says whether writing size more bytes would take the team's
// buckets past its plan, as last measured. The store refuses a write past a
// bucket's limit the same way whichever share set it, so a refused write
// that is not past the plan was refused for the install's room.
func (b Bucket) OverPlan(size int64) bool {
	return b.TeamLimit > 0 && b.TeamBytes+size > b.TeamLimit
}

// stored is a bucket's row and the key to it, which the Files tab uses.
func (s *Service) storedBucket(ctx context.Context, a App) (dbgen.Bucket, bucket.Created, error) {
	if a.Source != SourceBucket {
		return dbgen.Bucket{}, bucket.Created{}, ErrNotABucket
	}
	if s.buckets == nil {
		return dbgen.Bucket{}, bucket.Created{}, ErrNoBuckets
	}
	row, err := s.q.GetBucket(ctx, dbgen.GetBucketParams{OwnerID: a.OwnerID, AppID: a.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbgen.Bucket{}, bucket.Created{}, ErrNotABucket
		}
		return dbgen.Bucket{}, bucket.Created{}, fmt.Errorf("app: read bucket: %w", err)
	}
	secret, err := s.keeper.Open(row.SealedSecret)
	if err != nil {
		return dbgen.Bucket{}, bucket.Created{}, err
	}
	return row, bucket.Created{ID: row.GarageID, Name: row.Name, KeyID: row.KeyID, Secret: secret}, nil
}

// Bucket is one of a team's buckets.
func (s *Service) Bucket(ctx context.Context, ownerID, name string) (Bucket, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return Bucket{}, err
	}
	row, _, err := s.storedBucket(ctx, a)
	if err != nil {
		return Bucket{}, err
	}
	b := Bucket{
		Name: row.Name, Endpoint: s.buckets.Endpoint, Region: s.buckets.Region,
		Bytes: row.SizeBytes, Objects: row.Objects,
	}
	if row.MeasuredAt.Valid {
		b.MeasuredAt = row.MeasuredAt.Time
	}
	if q, err := s.Quota(ctx, ownerID); err == nil && q.BucketBytes > 0 {
		b.TeamLimit = q.BucketBytes
		rows, err := s.q.ListTeamBuckets(ctx, ownerID)
		if err != nil {
			return Bucket{}, fmt.Errorf("app: read buckets: %w", err)
		}
		for _, r := range rows {
			b.TeamBytes += r.SizeBytes
		}
	}
	return b, nil
}

// BucketFiles is a bucket's contents, reached with its own key.
func (s *Service) BucketFiles(ctx context.Context, ownerID, name string) (*bucket.Files, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	_, c, err := s.storedBucket(ctx, a)
	if err != nil {
		return nil, err
	}
	return s.buckets.Files(c), nil
}

// deleteBucket empties a bucket and deletes it and its key from the store.
// A bucket with no row — a create that failed before recording it — has
// nothing here to delete.
func (s *Service) deleteBucket(ctx context.Context, a App) error {
	if s.buckets == nil {
		return ErrNoBuckets
	}
	row, c, err := s.storedBucket(ctx, a)
	if errors.Is(err, ErrNotABucket) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.buckets.Delete(ctx, c); err != nil {
		return fmt.Errorf("app: delete bucket %s: %w", row.Name, err)
	}
	return nil
}
