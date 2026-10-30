package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/codeblocktz/yacht/internal/bucket"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Buckets' limits: how the bucket store's disk is shared.
//
// Garage keeps one limit per bucket, a size past which it refuses a write.
// The engine sets every bucket's, from two shares: the team's, which keeps a
// team within its plan, and the install's, which keeps every team together
// within the store's disk. Without the second, the plans together could
// promise more than the disk holds, and once it filled every team's uploads
// would fail, not only the one that filled it.
//
// What a limit cannot hold, the reserve kept free on the disk has to: Garage
// checks a single upload against the limit only once its blocks are written,
// and the parts of a multipart upload only when it is completed. Unfinished
// uploads are counted in what a bucket holds, ended after a day by a
// lifecycle rule on every bucket, and ended at once when they take a bucket
// past its limit (RunBuckets).

// bucketStore is what setting limits asks of the store. Garage in an install;
// a stand-in in a test.
type bucketStore interface {
	Disk(ctx context.Context) (bucket.Disk, error)
	SetLimit(ctx context.Context, id string, maxBytes int64) error
}

// The store's reserve: what is kept free on its disk whatever the limits
// say. For the store's own metadata where it shares the disk; for uploads in
// flight, which Garage writes before it checks them against a limit and
// refuses only afterwards, and whose blocks it frees only once it next
// collects unreferenced ones; and for what is written between two
// measurings, which limits follow.
//
// A tenth of the disk, and at least 2 GiB: the most the Files tab sends in
// one upload is 1 GiB, and a refused upload's blocks are on the disk until
// collected, so room for two of them. Never more than half the disk: a small
// disk keeps half of itself rather than all.
const (
	bucketReservePercent = 10
	bucketReserveMin     = 2 << 30
)

// bucketReserve is what is kept free of a disk of total bytes.
func bucketReserve(total int64) int64 {
	return min(max(total*bucketReservePercent/100, bucketReserveMin), total/2)
}

// bucketWantWindow is how long a bucket that grew, or was about to be written
// through the Files tab, counts as being written to: three measurings, so a
// pause between two uploads does not hand its room back.
const bucketWantWindow = 3 * bucketMeasureInterval

// bucketDemand is one bucket as its limit is decided.
type bucketDemand struct {
	// Size is what it holds; Limit the limit last set, zero for none.
	Size, Limit int64
	// TeamLimit is its team's quota for buckets, zero for none, and
	// TeamOthers what the team's other buckets hold.
	TeamLimit, TeamOthers int64
	// Wants is whether it is being written to: it grew lately, has an
	// upload unfinished, or is about to be written through the Files tab.
	Wants bool
}

// bucketRoom is the store's room as limits are decided.
type bucketRoom struct {
	// Free is what its disk may still take, past the reserve.
	Free int64
}

// allocateBuckets is the most each bucket on the install may hold, in the
// order given. Zero is no limit.
//
// With the store's room known, each bucket may grow by a grant, and the
// grants together are never more than the free room — the one thing that
// keeps buckets growing at once, between two settings of their limits, from
// filling the disk. Past that only by a byte for each empty bucket given
// nothing, which the store would otherwise read as no limit; the reserve
// covers that many times over. Every bucket is given a floor, an even part
// of half the free room, so an idle bucket can always take its next upload;
// what is left is shared between the buckets being written to, each taking
// what its team's share leaves it room for and the rest going to the others.
// So a bucket being written to can grow by at least half the free room, and
// an idle one holds no more than its part of the other half.
//
// With the room unknown, nothing is loosened: a bucket keeps its limit, or
// its team's share when that is tighter, so a store that cannot be read is
// never a reason to let a bucket write more.
func allocateBuckets(in []bucketDemand, room *bucketRoom) []int64 {
	out := make([]int64, len(in))
	if room == nil {
		for i, b := range in {
			out[i] = tighter(bucketShare(b.TeamLimit, b.TeamOthers), b.Limit)
		}
		return out
	}
	if len(in) == 0 {
		return out
	}
	free := max(room.Free, 0)
	// Every bucket's floor is an even part of half the free room, as room for
	// its next upload, so the other half is always there for the buckets
	// being written to. Not less: a single upload is refused without a trace
	// — it is not growth, so it never makes a bucket want room — and an app
	// putting one large object into a bucket that was idle could otherwise
	// never put it, whatever its plan and the disk allowed.
	floor := mebibytes(free / (2 * int64(len(in))))

	// Each bucket's room under its team's share, and its floor within it.
	team := make([]int64, len(in))
	headroom := make([]int64, len(in))
	grant := make([]int64, len(in))
	pool := free
	for i, b := range in {
		team[i] = bucketShare(b.TeamLimit, b.TeamOthers)
		headroom[i] = math.MaxInt64
		if team[i] > 0 {
			headroom[i] = max(team[i]-b.Size, 0)
		}
		grant[i] = min(floor, headroom[i])
		pool -= grant[i]
	}

	// A bucket that has used most of what it was given wants more, whether
	// or not it grew: a write it was refused does not show as growth.
	var wanting []int
	for i, b := range in {
		if b.Wants || (b.Limit > 0 && b.Limit-b.Size < floor/2) {
			wanting = append(wanting, i)
		}
	}
	// Smallest room first, so what a bucket cannot take is shared by the
	// ones after it.
	slices.SortStableFunc(wanting, func(a, b int) int {
		return cmp.Compare(headroom[a]-grant[a], headroom[b]-grant[b])
	})
	for k, i := range wanting {
		more := mebibytes(min(pool/int64(len(wanting)-k), headroom[i]-grant[i]))
		grant[i] += more
		pool -= more
	}

	for i, b := range in {
		limit := b.Size + grant[i]
		if team[i] > 0 {
			limit = min(limit, team[i])
		}
		out[i] = max(limit, 1)
	}
	return out
}

// mebibytes rounds a grant down to whole MiB, past one, so that limits do not
// change with every byte the free room does — each change is a call to the
// store.
func mebibytes(n int64) int64 {
	if n < 1<<20 {
		return n
	}
	return n &^ (1<<20 - 1)
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

// tighter is the smaller of two limits, where zero is none.
func tighter(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// looser is the larger of two limits, where zero is none.
func looser(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	return max(a, b)
}

// bucketLimitLock serialises setting buckets' limits across replicas: the
// limits are decided together from one reading of the store's disk, and two
// replicas deciding at once from two readings could each give out the same
// free room.
const bucketLimitLock int64 = 0x796163687462

// limitBuckets sets every bucket's limit on the install, as allocateBuckets
// decides them from the store's disk read now, under bucketLimitLock.
//
// Run whenever what a bucket may hold changes: a quota set, a bucket made, a
// file put or deleted through the Files tab, and on every measuring pass,
// which also heals: it sets every limit, where the others set only those that
// changed. A team's share follows measured sizes, so an app writing to two of
// a team's buckets at once can pass the team's total by what it writes
// between two passes, and no further; it cannot pass the install's.
func (s *Service) limitBuckets(ctx context.Context, heal bool) {
	if s.limits == nil {
		return
	}
	err := s.withSessionLock(ctx, bucketLimitLock, "bucket limits", func() error {
		rows, err := s.q.ListBucketLimits(ctx)
		if err != nil {
			return fmt.Errorf("list buckets: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		// Unread, the room is unknown and limits only tighten; the source
		// has logged why.
		var room *bucketRoom
		if d, err := s.disk.get(ctx, true); err == nil {
			room = &bucketRoom{Free: newBucketCapacity(d, 0, 0).Free}
		}
		teams := map[string]int64{}
		for _, r := range rows {
			teams[r.OwnerID] += r.SizeBytes
		}
		now := time.Now()
		in := make([]bucketDemand, len(rows))
		for i, r := range rows {
			in[i] = bucketDemand{
				Size: r.SizeBytes, Limit: r.LimitBytes,
				TeamLimit: r.TeamLimit, TeamOthers: teams[r.OwnerID] - r.SizeBytes,
				Wants: r.UploadingBytes > 0 ||
					(r.WantsRoomAt.Valid && now.Sub(r.WantsRoomAt.Time) < bucketWantWindow),
			}
		}
		limits := allocateBuckets(in, room)

		// Lowered before any is raised, so that the room a bucket is given
		// has already been taken from the ones it came from.
		order := make([]int, len(rows))
		for i := range order {
			order[i] = i
		}
		slices.SortStableFunc(order, func(a, b int) int {
			return cmp.Compare(lowers(limits[b], rows[b].LimitBytes), lowers(limits[a], rows[a].LimitBytes))
		})
		for _, i := range order {
			if limits[i] != rows[i].LimitBytes || heal {
				s.setBucketLimit(ctx, rows[i], limits[i])
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.log.Warn("buckets: limits", slog.String("error", err.Error()))
	}
}

// lowers is 1 when a limit is lower than the one it replaces, and 0 when it
// is not.
func lowers(next, prev int64) int {
	if looser(next, prev) == prev && next != prev {
		return 1
	}
	return 0
}

// setBucketLimit sets one bucket's limit on the store and records it.
//
// The record is never less than the store enforces: the looser of the old
// and the new limit is recorded first, then the store is told, then the new
// limit is recorded. A limit raised and not set, or lowered and not
// recorded, leaves the record above the store, which is the side the record
// is read on — a store that cannot be read only tightens from it, and a
// bucket measured past it was written past what the store allowed.
func (s *Service) setBucketLimit(ctx context.Context, r dbgen.ListBucketLimitsRow, limit int64) {
	record := func(n int64) bool {
		err := s.q.SetBucketLimit(ctx, dbgen.SetBucketLimitParams{AppID: r.AppID, LimitBytes: n})
		if err != nil && ctx.Err() == nil {
			s.log.Warn("buckets: record limit", slog.String("bucket", r.Name), slog.String("error", err.Error()))
		}
		return err == nil
	}
	first := looser(r.LimitBytes, limit)
	if first != r.LimitBytes && !record(first) {
		return
	}
	if err := s.limits.SetLimit(ctx, r.GarageID, limit); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("buckets: limit", slog.String("bucket", r.Name), slog.String("error", err.Error()))
		}
		return
	}
	if limit != first {
		record(limit)
	}
}

// MeasureBucket asks the store what a bucket holds now and records it, for
// its page to show straight after an upload or a delete, and sets the
// limits that follow from it.
func (s *Service) MeasureBucket(ctx context.Context, ownerID, name string) error {
	row, err := s.bucketRow(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if _, err := s.measure(ctx, row); err != nil {
		return err
	}
	s.limitBuckets(ctx, false)
	return nil
}

// PrepareBucketUpload gives a bucket about to be written through the Files
// tab its part of the store's free room, as a bucket being written to,
// before the upload rather than after a refusal.
func (s *Service) PrepareBucketUpload(ctx context.Context, ownerID, name string) error {
	row, err := s.bucketRow(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if _, err := s.measure(ctx, row); err != nil {
		return err
	}
	if err := s.q.BucketWantsRoom(ctx, row.AppID); err != nil {
		return fmt.Errorf("app: record a bucket wanting room: %w", err)
	}
	s.limitBuckets(ctx, false)
	return nil
}

func (s *Service) bucketRow(ctx context.Context, ownerID, name string) (dbgen.Bucket, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return dbgen.Bucket{}, err
	}
	row, _, err := s.storedBucket(ctx, a)
	return row, err
}

func (s *Service) measure(ctx context.Context, row dbgen.Bucket) (bucket.Usage, error) {
	u, err := s.buckets.Usage(ctx, row.GarageID)
	if err != nil {
		return u, err
	}
	return u, s.q.SetBucketUsage(ctx, dbgen.SetBucketUsageParams{
		AppID: row.AppID, SizeBytes: u.Bytes, Objects: u.Objects, UploadingBytes: u.Uploading,
	})
}

// bucketMeasureInterval is how often every bucket is measured: what usage
// and billing read, which change by the hour rather than the second.
const bucketMeasureInterval = 5 * time.Minute

// RunBuckets gives the store's node its place in the layout, which a new
// Garage needs before it stores anything, then measures every bucket every
// few minutes and sets every limit, until ctx ends. Every replica may run it:
// both are the same whoever does them.
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
		s.bucketPass(ctx)
		t.Reset(bucketMeasureInterval)
	}
}

// bucketPass measures every bucket, sets every limit from what it measured,
// and only then ends the unfinished uploads those limits cannot hold.
//
// In that order because an unfinished upload is what makes a bucket want
// room: measured before its limit is set, an app's large multipart upload
// into a bucket that was idle would be ended for passing the small limit an
// idle bucket keeps, when the pass was about to give it room for the upload.
// What is ended is only what passes the bucket's team's share or the store's
// free room.
func (s *Service) bucketPass(ctx context.Context) {
	rows, err := s.q.ListBuckets(ctx)
	if err != nil {
		s.log.Error("buckets: list", slog.String("error", err.Error()))
		return
	}
	for _, row := range rows {
		s.measureAndKeep(ctx, row)
	}
	s.limitBuckets(ctx, true)
	if rows, err = s.q.ListBuckets(ctx); err != nil {
		s.log.Error("buckets: list", slog.String("error", err.Error()))
		return
	}
	for _, row := range rows {
		s.endUploadsPastLimit(ctx, row)
	}
}

// measureAndKeep measures one bucket on a pass and keeps the lifecycle rule
// that ends its unfinished uploads after a day.
func (s *Service) measureAndKeep(ctx context.Context, row dbgen.Bucket) {
	warn := func(what string, err error) {
		if ctx.Err() == nil {
			s.log.Warn("buckets: "+what, slog.String("bucket", row.Name), slog.String("error", err.Error()))
		}
	}
	if _, err := s.measure(ctx, row); err != nil {
		warn("measure", err)
		return
	}
	files, err := s.bucketFiles(row)
	if err != nil {
		warn("open its key", err)
		return
	}
	if err := files.KeepAbortRule(ctx); err != nil {
		warn("the rule that ends unfinished uploads", err)
	}
}

// endUploadsPastLimit ends a bucket's unfinished uploads when, as measured,
// they have taken it past its limit: the store checks the limit only when an
// upload is completed, and one that cannot be completed within it is only
// filling the disk.
func (s *Service) endUploadsPastLimit(ctx context.Context, row dbgen.Bucket) {
	if row.UploadingBytes == 0 || row.LimitBytes == 0 || row.SizeBytes <= row.LimitBytes {
		return
	}
	warn := func(what string, err error) {
		if ctx.Err() == nil {
			s.log.Warn("buckets: "+what, slog.String("bucket", row.Name), slog.String("error", err.Error()))
		}
	}
	files, err := s.bucketFiles(row)
	if err != nil {
		warn("open its key", err)
		return
	}
	n, err := files.AbortUploads(ctx)
	if err != nil {
		warn("end unfinished uploads past its limit", err)
		return
	}
	s.log.Warn("buckets: ended unfinished uploads past the bucket's limit",
		slog.String("bucket", row.Name), slog.Int("uploads", n),
		slog.Int64("limit", row.LimitBytes), slog.Int64("bytes", row.SizeBytes))
	if _, err := s.measure(ctx, row); err != nil {
		warn("measure", err)
	}
}

// bucketFiles is a bucket's objects, through its own key.
func (s *Service) bucketFiles(row dbgen.Bucket) (*bucket.Files, error) {
	secret, err := s.keeper.Open(row.SealedSecret)
	if err != nil {
		return nil, err
	}
	return s.buckets.Files(bucket.Created{ID: row.GarageID, Name: row.Name, KeyID: row.KeyID, Secret: secret}), nil
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
	// below but Buckets and Stored is filled in, and buckets' limits are
	// only ever tightened. Reason is why not.
	Known  bool
	Reason string

	// Total is the store's disk. Reserve is kept free on it, and Usable is
	// the rest: what buckets may fill, and what a plan's bucket storage can
	// be checked against.
	Total   int64
	Reserve int64
	Usable  int64

	// Used is what is on the disk now: every bucket's objects and
	// unfinished uploads as stored, and the store's own metadata where it
	// shares the disk. Free is Usable less Used, never below zero: what
	// buckets may still write.
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

// Percent is how much of Usable is used, rounded down: 100 only once nothing
// is free, which is a store refusing every upload.
func (c BucketCapacity) Percent() int {
	switch {
	case !c.Known:
		return 0
	case c.Usable <= 0:
		return 100
	case c.Free == 0:
		return max(100, int(c.Used*100/c.Usable))
	}
	return int(c.Used * 100 / c.Usable)
}

// level is the store's fullness in a word, warned at warn percent.
func (c BucketCapacity) level(warn int) CapacityLevel {
	switch {
	case !c.Known:
		return CapacityUnknown
	case c.Free == 0:
		return CapacityFull
	case c.Percent() >= warn:
		return CapacityWarn
	}
	return CapacityOK
}

// bucketCapacity is the install's room for buckets now, with the disk as
// last read: a page never waits on the store (diskSource.cached).
func (s *Service) bucketCapacity(ctx context.Context) (BucketCapacity, error) {
	sum, err := s.q.SumBuckets(ctx)
	if err != nil {
		return BucketCapacity{}, fmt.Errorf("app: sum buckets: %w", err)
	}
	d, err := s.disk.cached()
	if err != nil {
		return BucketCapacity{Reason: err.Error(), Buckets: sum.Buckets, Stored: sum.Stored}, nil
	}
	return newBucketCapacity(d, sum.Buckets, sum.Stored), nil
}

// diskTTL is how long a reading of the store's disk is shown before it is
// read again. Limits are set from a reading of their own.
const diskTTL = 30 * time.Second

// diskReadTimeout bounds one reading.
const diskReadTimeout = 5 * time.Second

// errDiskNotRead is the disk before it has been read at all.
var errDiskNotRead = fmt.Errorf("%w: its disk has not been read yet", bucket.ErrUnavailable)

// diskSource is the store's disk: read afresh for limits, and kept for the
// pages, which are shown the last reading and never wait for the next.
type diskSource struct {
	read func(context.Context) (bucket.Disk, error)
	log  *slog.Logger
	now  func() time.Time

	// reading is held across a reading, so readings are one at a time.
	reading sync.Mutex

	// mu guards the last reading, which pages are shown.
	mu         sync.Mutex
	disk       bucket.Disk
	err        error
	at         time.Time
	refreshing bool

	// unknown is whether the last reading failed, so that a store that stays
	// unreadable is logged when it becomes so and not at every reading.
	unknown bool
}

func newDiskSource(read func(context.Context) (bucket.Disk, error), log *slog.Logger) *diskSource {
	if read == nil {
		read = func(context.Context) (bucket.Disk, error) { return bucket.Disk{}, ErrNoBuckets }
	}
	return &diskSource{read: read, log: log, now: time.Now, err: errDiskNotRead}
}

// get reads the disk now, or returns the last reading when it is younger
// than diskTTL and fresh is not asked for.
func (d *diskSource) get(ctx context.Context, fresh bool) (bucket.Disk, error) {
	if !fresh {
		d.mu.Lock()
		if !d.at.IsZero() && d.now().Sub(d.at) < diskTTL {
			defer d.mu.Unlock()
			return d.disk, d.err
		}
		d.mu.Unlock()
	}
	d.reading.Lock()
	defer d.reading.Unlock()
	readCtx, cancel := context.WithTimeout(ctx, diskReadTimeout)
	defer cancel()
	disk, err := d.read(readCtx)
	// A reading cut short by the caller going away says nothing about the
	// store.
	if ctx.Err() != nil {
		return disk, err
	}
	d.keep(disk, err)
	return disk, err
}

// cached is the last reading, at once. When it is older than diskTTL, or
// there is none, the disk is read again in the background, for the next
// page: a page an operator or a customer opens never waits on the store.
func (d *diskSource) cached() (bucket.Disk, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if (d.at.IsZero() || d.now().Sub(d.at) >= diskTTL) && !d.refreshing {
		d.refreshing = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), diskReadTimeout)
			defer cancel()
			_, _ = d.get(ctx, true)
			d.mu.Lock()
			d.refreshing = false
			d.mu.Unlock()
		}()
	}
	return d.disk, d.err
}

func (d *diskSource) keep(disk bucket.Disk, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disk, d.err, d.at = disk, err, d.now()
	switch {
	case err != nil && !d.unknown:
		d.unknown = true
		d.log.Warn("the bucket store's disk is unknown — buckets' limits are only tightened "+
			"until it can be read", slog.String("reason", err.Error()))
	case err == nil && d.unknown:
		d.unknown = false
		d.log.Info("the bucket store's disk can be read again")
	}
}

// withSessionLock runs fn holding a Postgres advisory lock on a connection of
// its own, for as long as fn takes: longer than a transaction lock could be
// held, since what fn does includes calls to other services.
func (s *Service) withSessionLock(ctx context.Context, key int64, what string, fn func() error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("app: acquire a connection for the %s lock: %w", what, err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		conn.Release()
		return fmt.Errorf("app: acquire the %s lock: %w", what, err)
	}
	defer func() {
		unlock, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlock, "SELECT pg_advisory_unlock($1)", key); err != nil {
			s.log.Error("could not release a lock", slog.String("lock", what), slog.String("error", err.Error()))
			// A session lock survives returning a connection to the pool.
			// Remove the broken session entirely rather than poisoning a
			// future caller.
			raw := conn.Hijack()
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			_ = raw.Close(closeCtx)
			return
		}
		conn.Release()
	}()
	return fn()
}
