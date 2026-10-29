package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
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

// MeasureBucket asks the store what a bucket holds now and records it, for
// its page to show straight after an upload or a delete.
func (s *Service) MeasureBucket(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	row, _, err := s.storedBucket(ctx, a)
	if err != nil {
		return err
	}
	if err := s.measure(ctx, row); err != nil {
		return err
	}
	s.limitBuckets(ctx, ownerID)
	return nil
}

// limitBuckets gives each of a team's buckets the most it may hold, as a
// limit the store keeps: the smaller of its share of the team's bucket
// storage and its share of the install's.
//
// The team's share is what the team may hold in buckets, less what its other
// buckets hold; with no quota there is none. The install's is what the bucket
// holds and a part of the store's free room (bucketLimits), so that every
// bucket on the install, a team's with no quota included, stops before the
// store's disk is full, and one team filling it cannot fail every other
// team's uploads.
//
// Shares follow the measured sizes, so they are as fresh as the last
// measuring: run when a quota is set, a bucket made, a file put or deleted
// through the Files tab, and on every measuring pass. An app writing to two
// buckets at once can pass the team's total by what it writes between two
// passes, and no further; it cannot pass the install's, which is split
// between the buckets rather than given to each.
//
// Only the team's own buckets are changed. Room another team frees, or
// leaves unused, reaches this team's buckets on the next measuring pass,
// which limits every team; until then the limits already set are the safe
// side of it.
func (s *Service) limitBuckets(ctx context.Context, ownerID string) {
	if s.buckets == nil {
		return
	}
	q, err := s.Quota(ctx, ownerID)
	if err != nil {
		s.log.Warn("buckets: read quota", slog.String("team", ownerID), slog.String("error", err.Error()))
		return
	}
	rows, err := s.q.ListTeamBuckets(ctx, ownerID)
	if err != nil {
		s.log.Warn("buckets: list a team's", slog.String("team", ownerID), slog.String("error", err.Error()))
		return
	}
	if len(rows) == 0 {
		return
	}
	room, err := s.installRoom(ctx, ownerID, len(rows))
	if err != nil {
		s.log.Warn("buckets: read the install's", slog.String("team", ownerID), slog.String("error", err.Error()))
		return
	}
	sizes := make([]int64, len(rows))
	for i, r := range rows {
		sizes[i] = r.SizeBytes
	}
	for i, limit := range bucketLimits(q.BucketBytes, sizes, room) {
		r := rows[i]
		// Recorded before the store is told, so that another team's buckets
		// are never given room this one has been, even for a moment: a
		// limit recorded and not set leaves room unused until the next
		// pass, and a limit set and not recorded would give it out twice.
		if err := s.q.SetBucketLimit(ctx, dbgen.SetBucketLimitParams{AppID: r.AppID, LimitBytes: limit}); err != nil {
			s.log.Warn("buckets: record limit", slog.String("bucket", r.Name), slog.String("error", err.Error()))
			continue
		}
		if err := s.buckets.SetLimit(ctx, r.GarageID, limit); err != nil && ctx.Err() == nil {
			s.log.Warn("buckets: limit", slog.String("bucket", r.Name), slog.String("error", err.Error()))
		}
	}
}

// installRoom is the store's room as a team with n buckets is limited in it,
// read afresh. Nil when the store's disk cannot be read: like the capacity
// policy with a cluster it cannot see, buckets are then held to their teams'
// quotas alone rather than refused everything — and a store that cannot be
// read is refusing writes itself.
func (s *Service) installRoom(ctx context.Context, ownerID string, n int) (*installRoom, error) {
	d, err := s.disk.get(ctx, true)
	if err != nil {
		return nil, nil
	}
	others, err := s.q.OtherBuckets(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	return &installRoom{
		Free:    newBucketCapacity(d, 0, 0).Free,
		Buckets: others.Buckets + int64(n),
		Granted: others.Granted,
	}, nil
}

// installRoom is the store's room, as the buckets of one team are limited in
// it.
type installRoom struct {
	// Free is what the store's disk may still take, past its reserve.
	Free int64
	// Buckets is every bucket on the install, the team's own among them.
	Buckets int64
	// Granted is what other teams' buckets may still write under the limits
	// set on them: room already given out.
	Granted int64
}

// bucketLimits is the most each of a team's buckets may hold, given the
// team's quota for buckets and each bucket's measured size, in the same
// order. Zero is no limit, which only a bucket with no quota and no room to
// read is given.
//
// Each bucket may grow by an equal part of the store's free room: the free
// room split between every bucket on the install, so that buckets growing at
// once between two measurings cannot together write more than is free.
// Never more than is left of the free room once other teams' buckets have
// what they were given, split between this team's: the free room is read
// now, and their limits were set from what was free then.
//
// The team's share, when it has a quota, bounds a bucket as before; the
// install's bounds it always. Never below one byte, which the store would
// read as no limit.
func bucketLimits(teamLimit int64, sizes []int64, room *installRoom) []int64 {
	var total int64
	for _, size := range sizes {
		total += size
	}
	var grow int64
	if room != nil && len(sizes) > 0 {
		n := int64(len(sizes))
		fair := room.Free / max(room.Buckets, n)
		left := max(room.Free-room.Granted, 0) / n
		grow = min(fair, left)
	}
	out := make([]int64, len(sizes))
	for i, size := range sizes {
		limit := bucketShare(teamLimit, total-size)
		if room != nil {
			if install := max(size+grow, 1); limit == 0 || install < limit {
				limit = install
			}
		}
		out[i] = limit
	}
	return out
}

// bucketShare is the most one bucket may hold when the team may hold limit
// and its other buckets hold others: zero for no limit, and never below one
// byte, which the store would read as none.
func bucketShare(limit, others int64) int64 {
	if limit <= 0 {
		return 0
	}
	return max(limit-others, 1)
}

// The store's reserve: what is kept free on its disk whatever the buckets'
// limits say. For the store's own metadata where it shares the disk, for
// blocks a delete has not yet let go of, and for what the limits miss —
// sizes are measured every few minutes, and the disk fills with what is
// written in between.
//
// A tenth of the disk, and at least a GiB, but never more than half of it: a
// small disk keeps half of itself rather than all.
const (
	bucketReservePercent = 10
	bucketReserveMin     = 1 << 30
)

// bucketReserve is what is kept free of a disk of total bytes.
func bucketReserve(total int64) int64 {
	return min(max(total*bucketReservePercent/100, bucketReserveMin), total/2)
}

// BucketCapacity is the install's room for buckets: the bucket store's disk,
// less what is kept free on it, against what is on it now. Bytes throughout.
//
// The disk is what Garage reports of its data partition, so it grows with
// the volume under it. Every bucket's limit is held under Usable together
// (limitBuckets), and past it every upload to every bucket is refused until
// there is room; Stored is what the teams are billed for.
type BucketCapacity struct {
	// Known is whether the store's disk could be read. Without it nothing
	// below but Buckets and Stored is filled in, and buckets are held to
	// their teams' quotas alone. Reason is why not.
	Known  bool
	Reason string

	// Total is the store's disk. Reserve is kept free on it, and Usable is
	// the rest: what buckets may fill, and what a plan's bucket storage can
	// be checked against.
	Total   int64
	Reserve int64
	Usable  int64

	// Used is what is on the disk now: every bucket's objects as stored,
	// and the store's own metadata where it shares the disk. Free is Usable
	// less Used, never below zero: what buckets may still write.
	Used int64
	Free int64

	// Buckets is how many there are, across every team, and Stored what
	// they held when last measured.
	Buckets int64
	Stored  int64
}

func newBucketCapacity(d bucket.Disk, buckets, stored int64) BucketCapacity {
	c := BucketCapacity{Known: true, Total: d.Total, Reserve: bucketReserve(d.Total), Buckets: buckets, Stored: stored}
	c.Usable = c.Total - c.Reserve
	c.Used = max(d.Total-d.Available, 0)
	c.Free = max(c.Usable-c.Used, 0)
	return c
}

// Percent is how much of Usable is used: 100 and past is a store refusing
// every upload.
func (c BucketCapacity) Percent() int {
	if c.Usable <= 0 {
		if c.Known {
			return 100
		}
		return 0
	}
	return int(math.Round(float64(c.Used) * 100 / float64(c.Usable)))
}

// level is the store's fullness in a word, warned at warn percent.
func (c BucketCapacity) level(warn int) CapacityLevel {
	switch {
	case !c.Known:
		return CapacityUnknown
	case c.Percent() >= 100:
		return CapacityFull
	case c.Percent() >= warn:
		return CapacityWarn
	}
	return CapacityOK
}

// bucketCapacity is the install's room for buckets now, the disk as last
// read within diskTTL.
func (s *Service) bucketCapacity(ctx context.Context) (BucketCapacity, error) {
	sum, err := s.q.SumBuckets(ctx)
	if err != nil {
		return BucketCapacity{}, fmt.Errorf("app: sum buckets: %w", err)
	}
	d, err := s.disk.get(ctx, false)
	if err != nil {
		return BucketCapacity{Reason: err.Error(), Buckets: sum.Buckets, Stored: sum.Stored}, nil
	}
	return newBucketCapacity(d, sum.Buckets, sum.Stored), nil
}

// diskTTL is how long a reading of the store's disk is trusted by the pages
// that show it, which ask on every page an operator opens. Limits are set
// from a fresh reading instead.
const diskTTL = 30 * time.Second

// diskReadTimeout bounds one reading, so a store that has stopped answering
// slows a page by seconds rather than stopping it.
const diskReadTimeout = 5 * time.Second

// diskSource is the store's disk, read when asked and kept for diskTTL.
type diskSource struct {
	read func(context.Context) (bucket.Disk, error)
	log  *slog.Logger
	now  func() time.Time

	// mu is held across a reading, so a burst of pages on a stale reading
	// reads the store once rather than once each.
	mu   sync.Mutex
	disk bucket.Disk
	err  error
	at   time.Time

	// unknown is whether the last reading failed, so that a store that stays
	// unreadable is logged when it becomes so and not at every reading.
	unknown bool
}

func newDiskSource(g *bucket.Garage, log *slog.Logger) *diskSource {
	read := func(context.Context) (bucket.Disk, error) {
		return bucket.Disk{}, ErrNoBuckets
	}
	if g != nil {
		read = g.Disk
	}
	return &diskSource{read: read, log: log, now: time.Now}
}

// get returns the disk, read again when fresh is asked for or the last
// reading is older than diskTTL.
func (d *diskSource) get(ctx context.Context, fresh bool) (bucket.Disk, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !fresh && !d.at.IsZero() && d.now().Sub(d.at) < diskTTL {
		return d.disk, d.err
	}
	readCtx, cancel := context.WithTimeout(ctx, diskReadTimeout)
	defer cancel()
	disk, err := d.read(readCtx)
	// A reading cut short by the caller going away says nothing about the
	// store.
	if ctx.Err() != nil {
		return disk, err
	}
	d.disk, d.err, d.at = disk, err, d.now()
	switch {
	case err != nil && !d.unknown:
		d.unknown = true
		d.log.Warn("the bucket store's disk is unknown — buckets are held to their teams' quotas "+
			"alone until it can be read", slog.String("reason", err.Error()))
	case err == nil && d.unknown:
		d.unknown = false
		d.log.Info("the bucket store's disk can be read again")
	}
	return disk, err
}

func (s *Service) measure(ctx context.Context, row dbgen.Bucket) error {
	u, err := s.buckets.Usage(ctx, row.GarageID)
	if err != nil {
		return err
	}
	return s.q.SetBucketUsage(ctx, dbgen.SetBucketUsageParams{
		AppID: row.AppID, SizeBytes: u.Bytes, Objects: u.Objects,
	})
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

// bucketMeasureInterval is how often every bucket is measured: what usage
// and billing read, which change by the hour rather than the second.
const bucketMeasureInterval = 5 * time.Minute

// RunBuckets gives the store's node its place in the layout, which a new
// Garage needs before it stores anything, then measures every bucket every
// few minutes, until ctx ends. Every replica may run it: both are the same
// whoever does them.
func (s *Service) RunBuckets(ctx context.Context, zone string) {
	if s.buckets == nil {
		return
	}
	laidOut := false
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !laidOut {
			if err := s.buckets.EnsureLayout(ctx, zone); err != nil {
				s.log.Warn("buckets: the store's layout", slog.String("error", err.Error()))
				t.Reset(30 * time.Second)
				continue
			}
			laidOut = true
		}
		rows, err := s.q.ListBuckets(ctx)
		if err != nil {
			s.log.Error("buckets: list", slog.String("error", err.Error()))
		}
		teams := map[string]bool{}
		for _, row := range rows {
			if err := s.measure(ctx, row); err != nil && ctx.Err() == nil {
				s.log.Warn("buckets: measure", slog.String("bucket", row.Name), slog.String("error", err.Error()))
			}
			teams[row.OwnerID] = true
		}
		for owner := range teams {
			s.limitBuckets(ctx, owner)
		}
		t.Reset(bucketMeasureInterval)
	}
}
