package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Quotas: how much of a shared install each team may commit.
//
// What is counted is what a team has asked the cluster to hold for it, read
// from its apps rather than measured from its pods. Measured use rises and falls
// with traffic, and a limit enforced against it would refuse a deploy at noon
// that it allowed at midnight. Committed use changes only when somebody changes
// something, which is also the only moment a refusal can be acted on.
//
// Enforced here, in the service, rather than in the dashboard: an application
// wrapping the engine drives this same service, and a limit only the engine's
// own forms respected would be a suggestion.

// ErrQuotaExceeded means a change would take a team past what the install's
// operator allows it. The wrapped message says which limit, how far, and what
// to do; this is what a caller tests for.
var ErrQuotaExceeded = errors.New("app: this would take the team over its quota")

// ErrNoSuchTeam means a quota was asked about a team the install does not have.
var ErrNoSuchTeam = errors.New("app: no such team")

// Quota is how much a team may commit. Zero in any field is unlimited, and so
// is a team with no quota at all.
type Quota struct {
	Apps         int32
	CPUMillis    int64
	MemoryBytes  int64
	StorageBytes int64
}

// Unlimited reports whether nothing is limited, which is the answer for most
// installs: one team, and nobody to share with.
func (q Quota) Unlimited() bool {
	return q == Quota{}
}

// Validate refuses a quota that cannot be meant. Negative is the only such
// thing: zero already has a meaning.
func (q Quota) Validate() error {
	if q.Apps < 0 || q.CPUMillis < 0 || q.MemoryBytes < 0 || q.StorageBytes < 0 {
		return errors.New("a quota cannot be negative — leave it at zero for no limit")
	}
	return nil
}

// Usage is what a team has committed: its apps, the CPU and memory those apps'
// replicas may take, and the storage reserved for them.
type Usage struct {
	Apps         int64
	CPUMillis    int64
	MemoryBytes  int64
	StorageBytes int64
}

// TeamUsage is one team as the install's operator sees it: who it is, how many
// people are in it, what it has committed and what it may.
type TeamUsage struct {
	TeamID   string
	TeamName string

	// Members is zero on an install with no accounts, where nobody is a member
	// of anything and the one principal is the whole team.
	Members int64

	Usage Usage
	Quota Quota

	// QuotaSetAt is when the operator last saved a quota, nil when nobody ever
	// has — which is not the same as having saved one that limits nothing.
	QuotaSetAt *time.Time
}

// shape is what an app commits before it is multiplied out: the fields of an
// app the quota reads, and nothing else.
type shape struct {
	Replicas    int32
	CPULimit    string
	MemoryLimit string
}

// defaultCPUMillis and defaultMemoryBytes are what a container with no limit of
// its own is given by the namespace LimitRange.
//
// Counted at that rather than at zero. A limit left empty is not a workload
// that takes nothing — it is one that takes the default — and counting it free
// would make leaving the field blank the way around every quota.
var defaultCPUMillis, defaultMemoryBytes = func() (int64, int64) {
	d := orchestrator.ResourceLimits{}.OrDefaults()
	cpu, mem := resource.MustParse(d.DefaultCPU), resource.MustParse(d.DefaultMemory)
	return cpu.MilliValue(), mem.Value()
}()

// cost is what one app commits: its replicas times each limit.
//
// A limit the cluster would not parse is counted at the default too, rather
// than refused here. Kubernetes reads it with this same parser and refuses the
// apply, so a workload with that limit never runs and never takes anything; the
// deploy's own failure is the place that says so.
func (sh shape) cost() (cpuMillis, memoryBytes int64) {
	cpuMillis, memoryBytes = defaultCPUMillis, defaultMemoryBytes
	if q, err := resource.ParseQuantity(sh.CPULimit); err == nil && sh.CPULimit != "" {
		cpuMillis = q.MilliValue()
	}
	if q, err := resource.ParseQuantity(sh.MemoryLimit); err == nil && sh.MemoryLimit != "" {
		memoryBytes = q.Value()
	}
	n := int64(max(sh.Replicas, 0))
	return n * cpuMillis, n * memoryBytes
}

// quotaChange is what somebody is asking for, in the terms the quota counts.
type quotaChange struct {
	// Changed is an existing app, and Reshape what the request does to it.
	// A function of the app's shape rather than the shape itself, because the
	// shape it is handed is the one read under the lock: a scale must not
	// count limits somebody else changed a moment ago at their old values.
	Changed uuid.UUID
	Reshape func(shape) shape

	// Added is apps that do not exist yet.
	Added []shape

	// Storage is bytes of volume being added; negative for a volume shrinking,
	// which nothing does today.
	Storage int64
}

// withinQuota refuses a change that would raise a team's committed use past its
// quota.
//
// q should be the caller's transaction, and the change should be written in
// that same transaction: the quota row is locked here and held until commit,
// which is what stops two creates both fitting in the space for one. Called on
// the pool instead, the lock lasts one statement and the check is advice.
func (s *Service) withinQuota(
	ctx context.Context, q *dbgen.Queries, ownerID string, c quotaChange,
) error {
	row, err := q.LockTeamQuota(ctx, ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("app: read quota: %w", err)
	}
	limit := toQuota(row)
	if limit.Unlimited() {
		return nil
	}

	now, err := s.usageWith(ctx, q, ownerID, nil)
	if err != nil {
		return err
	}
	next, err := s.usageWith(ctx, q, ownerID, &c)
	if err != nil {
		return err
	}
	return limit.admit(now, next)
}

// admit compares what a team has committed with what it would commit after a
// change.
//
// A limit is only enforced where the change raises use. Lowering a quota below
// what a team already has must not trap it: scaling down, shrinking a limit or
// deleting are how it gets back under, and each of those would otherwise be
// refused for being over — as would every change that does not touch the
// resource at all, like an image tag.
func (q Quota) admit(now, next Usage) error {
	type check struct {
		what          string
		now, next, at int64
		unit          func(int64) string
	}
	for _, c := range []check{
		{"apps", now.Apps, next.Apps, int64(q.Apps), countUnit},
		{"CPU", now.CPUMillis, next.CPUMillis, q.CPUMillis, cpuUnit},
		{"memory", now.MemoryBytes, next.MemoryBytes, q.MemoryBytes, bytesUnit},
		{"storage", now.StorageBytes, next.StorageBytes, q.StorageBytes, bytesUnit},
	} {
		if c.at == 0 || c.next <= c.now || c.next <= c.at {
			continue
		}
		return fmt.Errorf("%w — %s would be %s against a quota of %s, with %s committed now. "+
			"Free some up — fewer replicas, lower limits, or an app removed — or ask whoever "+
			"runs this install to raise it",
			ErrQuotaExceeded, c.what, c.unit(c.next), c.unit(c.at), c.unit(c.now))
	}
	return nil
}

// usageWith totals a team's committed use, as it is or with a change applied.
func (s *Service) usageWith(
	ctx context.Context, q *dbgen.Queries, ownerID string, c *quotaChange,
) (Usage, error) {
	rows, err := q.ListAppFootprints(ctx, ownerID)
	if err != nil {
		return Usage{}, fmt.Errorf("app: read committed resources: %w", err)
	}
	storage, err := q.SumVolumeBytes(ctx, ownerID)
	if err != nil {
		return Usage{}, fmt.Errorf("app: read committed storage: %w", err)
	}
	apps := make([]footprint, len(rows))
	for i, a := range rows {
		apps[i] = footprint{a.ID, shape{Replicas: a.Replicas, CPULimit: a.CpuLimit, MemoryLimit: a.MemoryLimit}}
	}
	return tally(apps, storage, c), nil
}

// footprint is one existing app's shape, with the id a change names it by.
type footprint struct {
	ID uuid.UUID
	shape
}

// tally is what a set of apps and volumes commits, with a change applied when
// there is one. One function for a team and for the whole install, so the two
// can never count an app differently.
func tally(apps []footprint, storage int64, c *quotaChange) Usage {
	u := Usage{Apps: int64(len(apps)), StorageBytes: storage}
	for _, a := range apps {
		sh := a.shape
		if c != nil && c.Reshape != nil && a.ID == c.Changed {
			sh = c.Reshape(sh)
		}
		cpu, mem := sh.cost()
		u.CPUMillis += cpu
		u.MemoryBytes += mem
	}
	if c != nil {
		for _, sh := range c.Added {
			cpu, mem := sh.cost()
			u.Apps++
			u.CPUMillis += cpu
			u.MemoryBytes += mem
		}
		u.StorageBytes += c.Storage
	}
	return u
}

// Quota returns a team's quota. A team with none is unlimited, which is the
// zero Quota rather than an error.
func (s *Service) Quota(ctx context.Context, ownerID string) (Quota, error) {
	row, err := s.q.GetTeamQuota(ctx, ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quota{}, nil
		}
		return Quota{}, fmt.Errorf("app: read quota: %w", err)
	}
	return toQuota(row), nil
}

// SetQuota replaces a team's quota.
//
// Taking effect on the next change the team makes, never on what it already
// has: nothing running is stopped or scaled down to fit. An operator lowering a
// quota is saying "no more", and reading it as "fewer, now" would take a team's
// workloads down on the strength of a form.
func (s *Service) SetQuota(ctx context.Context, ownerID string, q Quota) error {
	if err := q.Validate(); err != nil {
		return err
	}
	_, err := s.q.UpsertTeamQuota(ctx, dbgen.UpsertTeamQuotaParams{
		OwnerID: ownerID, MaxApps: q.Apps, CpuMillis: q.CPUMillis,
		MemoryBytes: q.MemoryBytes, StorageBytes: q.StorageBytes,
	})
	if err != nil {
		if isForeignKeyViolation(err) {
			return ErrNoSuchTeam
		}
		return fmt.Errorf("app: set quota: %w", err)
	}
	s.log.Info("team quota set", slog.String("team", ownerID),
		slog.Int("apps", int(q.Apps)), slog.Int64("cpu_millis", q.CPUMillis),
		slog.Int64("memory_bytes", q.MemoryBytes), slog.Int64("storage_bytes", q.StorageBytes))
	return nil
}

// TeamUsages is every team on the install with what it has committed and what
// it may. For the install's operator; nothing here is scoped to one team.
func (s *Service) TeamUsages(ctx context.Context) ([]TeamUsage, error) {
	rows, err := s.q.ListTeamQuotas(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list teams: %w", err)
	}
	apps, err := s.q.ListAllAppFootprints(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: read committed resources: %w", err)
	}

	byTeam := make(map[string]*Usage, len(rows))
	out := make([]TeamUsage, len(rows))
	for i, r := range rows {
		out[i] = teamUsage(dbgen.GetTeamQuotaSummaryRow(r))
		byTeam[r.ID] = &out[i].Usage
	}
	for _, a := range apps {
		u, ok := byTeam[a.OwnerID]
		if !ok {
			continue
		}
		cpu, mem := shape{Replicas: a.Replicas, CPULimit: a.CpuLimit, MemoryLimit: a.MemoryLimit}.cost()
		u.Apps++
		u.CPUMillis += cpu
		u.MemoryBytes += mem
	}
	return out, nil
}

// TeamUsage is one team's, for the page that edits its quota.
func (s *Service) TeamUsage(ctx context.Context, ownerID string) (TeamUsage, error) {
	row, err := s.q.GetTeamQuotaSummary(ctx, ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TeamUsage{}, ErrNoSuchTeam
		}
		return TeamUsage{}, fmt.Errorf("app: read team: %w", err)
	}
	t := teamUsage(row)
	u, err := s.usageWith(ctx, s.q, ownerID, nil)
	if err != nil {
		return TeamUsage{}, err
	}
	t.Usage = u
	return t, nil
}

func teamUsage(r dbgen.GetTeamQuotaSummaryRow) TeamUsage {
	t := TeamUsage{
		TeamID: r.ID, TeamName: r.DisplayName, Members: r.Members,
		Usage: Usage{StorageBytes: r.StorageBytes},
		Quota: Quota{
			Apps: r.QuotaApps, CPUMillis: r.QuotaCpuMillis,
			MemoryBytes: r.QuotaMemoryBytes, StorageBytes: r.QuotaStorageBytes,
		},
	}
	if r.QuotaUpdatedAt.Valid {
		at := r.QuotaUpdatedAt.Time
		t.QuotaSetAt = &at
	}
	return t
}

func toQuota(row dbgen.TeamQuota) Quota {
	return Quota{
		Apps: row.MaxApps, CPUMillis: row.CpuMillis,
		MemoryBytes: row.MemoryBytes, StorageBytes: row.StorageBytes,
	}
}

// isForeignKeyViolation reports SQLSTATE 23503: here, a quota for a team id
// that is not a team.
func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

// The units a refusal is written in: what somebody would type into the quota
// form, rather than what the cluster is given.

func countUnit(n int64) string {
	if n == 1 {
		return "1 app"
	}
	return strconv.FormatInt(n, 10) + " apps"
}

func cpuUnit(millis int64) string {
	return strconv.FormatFloat(float64(millis)/1000, 'f', -1, 64) + " vCPU"
}

// bytesUnit says GiB from one upward and MiB below it. A default memory limit
// is 128 MiB, and "0.13 GiB" is a number nobody recognises as that.
func bytesUnit(b int64) string {
	if b < 1<<30 {
		return strconv.FormatInt(int64(math.Round(float64(b)/(1<<20))), 10) + " MiB"
	}
	return strconv.FormatFloat(math.Round(float64(b)/(1<<30)*100)/100, 'f', -1, 64) + " GiB"
}
