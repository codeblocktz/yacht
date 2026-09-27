package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Capacity: how much of the whole install may be committed.
//
// A quota bounds one team; nothing bounded the teams together except the
// machines, and an install that sells room on machines it bought must not sell
// more than it has. The operator's policy says how much that is — the
// schedulable nodes' allocatable, oversold by a ratio, less a reserve — and
// the same admission that holds a team to its quota holds the install to it.
//
// Counted exactly as a quota counts: replicas times limits, volumes by size.
// A quota and the capacity it is carved from must agree on what "one app"
// takes, or the operator is doing arithmetic between two pages that differ.

// ErrCapacityFull means a change would take the install past what its
// operator is prepared to sell. The wrapped message says how much more the
// change needs than is free — and nothing about who else holds the rest.
var ErrCapacityFull = errors.New("app: there's no room on this install for that right now")

// CapacityPolicy is how much of the install the operator sells.
type CapacityPolicy struct {
	// Enforce refuses changes past what is sellable. Off, the install
	// behaves as it did before there was a policy: everything is still
	// counted and shown, and nothing is refused for it.
	Enforce bool

	// CPURatio and MemoryRatio are how far limits may be oversold: 2 sells
	// every allocatable core twice. A limit is a ceiling rather than a
	// reservation, and CPU past it is throttled; memory past it is not — a
	// node that runs out kills pods — so above 1 on memory is a bet.
	CPURatio    float64
	MemoryRatio float64

	// ReservePercent is kept free for a node failing, a drain, and a
	// rollout briefly running old and new pods together.
	ReservePercent int

	// WarnPercent of sellable committed is when the operator is warned.
	WarnPercent int

	// StorageBytes is the volume capacity the operator is selling. Zero is
	// not counted: nothing the engine can read says how much a storage class
	// can provision, and a node's disk is not it.
	StorageBytes int64

	// UpdatedAt is when it was last saved; zero on the defaults.
	UpdatedAt time.Time
}

// DefaultCapacityPolicy is every install's until its operator changes it:
// not enforced, nothing oversold, nothing reserved, warned at 80%.
func DefaultCapacityPolicy() CapacityPolicy {
	return CapacityPolicy{CPURatio: 1, MemoryRatio: 1, WarnPercent: 80}
}

// Validate refuses a policy that cannot be meant. The bounds are the table's
// own, said here in words.
func (p CapacityPolicy) Validate() error {
	ratio := func(what string, r float64) error {
		switch {
		case math.IsNaN(r) || math.IsInf(r, 0):
			return fmt.Errorf("the %s commit ratio must be a number", what)
		case r < 1:
			return fmt.Errorf("the %s commit ratio cannot be below 1 — use the reserve to hold room back", what)
		case r > 10:
			return fmt.Errorf("the %s commit ratio is at most 10", what)
		}
		return nil
	}
	if err := ratio("CPU", p.CPURatio); err != nil {
		return err
	}
	if err := ratio("memory", p.MemoryRatio); err != nil {
		return err
	}
	switch {
	case p.ReservePercent < 0 || p.ReservePercent > 90:
		return errors.New("the reserve is a percentage from 0 to 90 — above that there is nothing left to sell")
	case p.WarnPercent < 1 || p.WarnPercent > 100:
		return errors.New("the warning is a percentage from 1 to 100")
	case p.StorageBytes < 0:
		return errors.New("storage capacity cannot be negative — leave it at zero not to count storage")
	}
	return nil
}

// Room is what the schedulable machines offer, as last read from the cluster.
type Room struct {
	// Known is false when the cluster could not be read, or reported no
	// machines at all — which is an orchestrator with no cluster behind it,
	// not a cluster with no room. Reason says which.
	Known  bool
	Reason string

	// Nodes is every machine; Schedulable the ones new pods can land on —
	// ready and not cordoned — which are the only ones whose room counts.
	Nodes       int
	Schedulable int

	// CPUMillis and MemoryBytes are the schedulable nodes' allocatable.
	CPUMillis   int64
	MemoryBytes int64

	// Used is what those nodes report using now, known only where
	// metrics-server is installed.
	CPUUsedMillis   int64
	MemoryUsedBytes int64
	UsageKnown      bool
}

// RoomOf totals the room on the machines new pods can be placed on.
//
// A cordoned or not-ready node is left out of the room but still counted as a
// node: its pods are running and its capacity is real, but nothing new will be
// placed there, and "is there room for the next change" is the question.
func RoomOf(nodes []orchestrator.NodeInfo) Room {
	r := Room{Known: len(nodes) > 0}
	if !r.Known {
		r.Reason = "the cluster reports no nodes"
	}
	for _, n := range nodes {
		r.Nodes++
		if !n.Ready || n.Unschedulable {
			continue
		}
		r.Schedulable++
		r.CPUMillis += cmp.Or(n.CPUAllocatableMillis, n.CPUCapacityMillis)
		r.MemoryBytes += cmp.Or(n.MemAllocatableBytes, n.MemCapacityBytes)
		if n.UsageKnown {
			r.UsageKnown = true
			r.CPUUsedMillis += n.CPUUsedMillis
			r.MemoryUsedBytes += n.MemUsedBytes
		}
	}
	return r
}

// roomTTL is how long a reading of the cluster's machines is trusted. Nodes
// join and leave in minutes, and admission asks on every change that would
// raise what the install commits — which must not be a call to the
// Kubernetes API each time.
const roomTTL = 30 * time.Second

// roomReadTimeout bounds one reading, so a cluster that has stopped answering
// makes a change wait seconds rather than forever before it is let through.
const roomReadTimeout = 5 * time.Second

// roomSource is the cluster's room, read at most once per roomTTL and shared
// by admission and the pages that show it.
type roomSource struct {
	read func(context.Context) ([]orchestrator.NodeInfo, error)
	log  *slog.Logger
	now  func() time.Time

	// mu is held across a reading, so that a burst of changes arriving on a
	// stale cache reads the cluster once rather than once each.
	mu   sync.Mutex
	room Room
	at   time.Time

	// unknown is whether the last reading failed, so that a cluster that
	// stays unreadable is logged when it becomes so and not every 30 seconds.
	unknown bool
}

func newRoomSource(orch orchestrator.Orchestrator, log *slog.Logger) *roomSource {
	read := func(context.Context) ([]orchestrator.NodeInfo, error) {
		return nil, errors.New("no orchestrator")
	}
	if orch != nil {
		read = orch.Nodes
	}
	return &roomSource{read: read, log: log, now: time.Now}
}

// get returns the room, reading the cluster when the last reading is stale.
func (r *roomSource) get(ctx context.Context) Room {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.at.IsZero() && r.now().Sub(r.at) < roomTTL {
		return r.room
	}

	readCtx, cancel := context.WithTimeout(ctx, roomReadTimeout)
	defer cancel()
	nodes, err := r.read(readCtx)
	room := RoomOf(nodes)
	if err != nil {
		room = Room{Reason: err.Error()}
	}

	// A reading cut short by the caller going away says nothing about the
	// cluster, and caching it would answer "unknown" to everyone for 30s.
	if ctx.Err() != nil {
		return room
	}
	r.room, r.at = room, r.now()

	switch {
	case !room.Known && !r.unknown:
		r.unknown = true
		r.log.Warn("cluster capacity is unknown — nothing is refused for capacity "+
			"until the cluster can be read", slog.String("reason", room.Reason))
	case room.Known && r.unknown:
		r.unknown = false
		r.log.Info("cluster capacity can be read again")
	}
	return room
}

// sellable is what the policy sells of a room: allocatable, times the ratio,
// less the reserve. Storage is the operator's figure as it stands — the ratio
// and the reserve are about machines — and zero where it is not counted.
func (p CapacityPolicy) sellable(room Room) (cpuMillis, memoryBytes, storageBytes int64) {
	keep := 1 - float64(p.ReservePercent)/100
	return int64(math.Floor(float64(room.CPUMillis) * p.CPURatio * keep)),
		int64(math.Floor(float64(room.MemoryBytes) * p.MemoryRatio * keep)),
		p.StorageBytes
}

// shortfall is one resource a change needs more of than is sellable.
type shortfall struct {
	Resource string // cpu, memory or storage — the refusal record's names
	Amount   int64
}

// capacityAdmit compares what the install has committed with what it would
// commit after a change, against what is sellable.
//
// The rule Quota.admit keeps, for the same reason: only a rise is refused.
// An operator who lowers the ratio or cordons a node must not trap every team
// on the install — scaling down, lowering a limit and deleting are how the
// install gets back under, and a change that touches no resource is none of
// this check's business.
func capacityAdmit(cpu, memory, storage int64, now, next Usage) (shortfall, bool) {
	for _, c := range []struct {
		resource      string
		now, next, at int64
		counted       bool
	}{
		{"cpu", now.CPUMillis, next.CPUMillis, cpu, true},
		{"memory", now.MemoryBytes, next.MemoryBytes, memory, true},
		{"storage", now.StorageBytes, next.StorageBytes, storage, storage > 0},
	} {
		if !c.counted || c.next <= c.now || c.next <= c.at {
			continue
		}
		// From whichever is higher: already past sellable, the whole rise is
		// short; under it, only what does not fit.
		return shortfall{c.resource, c.next - max(c.at, c.now)}, true
	}
	return shortfall{}, false
}

// refusal is the sentence a customer is refused with. It says how much more
// was needed and what to try; it cannot say how much anybody else holds, which
// is not the customer's to know.
func (f shortfall) refusal() error {
	amount, try := cpuUnit(f.Amount)+" more CPU", "Try fewer replicas or smaller limits"
	switch f.Resource {
	case "memory":
		amount = bytesUnit(f.Amount) + " more memory"
	case "storage":
		amount, try = bytesUnit(f.Amount)+" more storage", "Try a smaller volume"
	}
	return fmt.Errorf("%w — it needs %s than is free. The people who run it have been told. "+
		"%s, or try again later", ErrCapacityFull, amount, try)
}

// admit holds a change to the team's quota and then to the install's
// capacity, in that order, in the caller's transaction.
//
// Always that order, from every caller: the team's row and then the policy
// row, so two changes can never each hold the lock the other wants.
func (s *Service) admit(ctx context.Context, q *dbgen.Queries, ownerID string, c quotaChange) error {
	if err := s.withinQuota(ctx, q, ownerID, c); err != nil {
		return err
	}
	return s.withinCapacity(ctx, q, ownerID, c)
}

// withinCapacity refuses a change that would raise the install's committed use
// past what its operator sells.
//
// Like withinQuota, q should be the transaction the change is written in: the
// policy row is locked until it commits, which is what stops two creates both
// taking the last of the room.
//
// The cluster is read before the lock is taken, not under it. The reading is
// usually cached; when it is not, it is a call to the Kubernetes API, and
// holding every other change on the install behind that call would make one
// slow API server everybody's problem.
func (s *Service) withinCapacity(
	ctx context.Context, q *dbgen.Queries, ownerID string, c quotaChange,
) error {
	p, err := capacityPolicy(ctx, q.GetCapacityPolicy)
	if err != nil || !p.Enforce {
		return err
	}
	room := s.room.get(ctx)
	if !room.Known {
		// Open rather than closed. An install whose API server is briefly
		// unreachable must not stop every customer deploying; the source has
		// already logged it, and the operator's banner says so.
		return nil
	}

	p, err = capacityPolicy(ctx, q.LockCapacityPolicy)
	if err != nil || !p.Enforce {
		return err
	}
	now, err := s.installUsageWith(ctx, q, nil)
	if err != nil {
		return err
	}
	next, err := s.installUsageWith(ctx, q, &c)
	if err != nil {
		return err
	}
	cpu, memory, storage := p.sellable(room)
	short, refused := capacityAdmit(cpu, memory, storage, now, next)
	if !refused {
		return nil
	}
	s.recordRefusal(ownerID, short)
	return short.refusal()
}

// recordRefusal notes demand turned away, for the operator.
//
// Written after the caller's transaction is gone rather than in it: the
// refusal rolls that transaction back, and with it anything written there. Not
// written on the pool while the transaction is open either — every change
// waiting on the policy lock holds a connection, and one that needed a second
// connection to finish refusing could wait for a pool the waiters had emptied.
func (s *Service) recordRefusal(ownerID string, f shortfall) {
	s.log.Warn("change refused: no room on this install",
		slog.String("team", ownerID), slog.String("resource", f.Resource),
		slog.Int64("shortfall", f.Amount))
	s.refusals.Add(1)
	go func() {
		defer s.refusals.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.q.RecordCapacityRefusal(ctx, dbgen.RecordCapacityRefusalParams{
			OwnerID: ownerID, Resource: f.Resource, Shortfall: f.Amount,
		}); err != nil {
			s.log.Error("record capacity refusal", slog.String("error", err.Error()))
		}
	}()
}

// capacityPolicy reads the policy with read, which either locks it or does
// not. No row is the default policy: a database somebody edited by hand is
// not a reason to refuse every change on the install.
func capacityPolicy(
	ctx context.Context, read func(context.Context) (dbgen.CapacityPolicy, error),
) (CapacityPolicy, error) {
	row, err := read(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DefaultCapacityPolicy(), nil
		}
		return CapacityPolicy{}, fmt.Errorf("app: read capacity policy: %w", err)
	}
	return CapacityPolicy{
		Enforce: row.Enforce, CPURatio: row.CpuCommitRatio, MemoryRatio: row.MemoryCommitRatio,
		ReservePercent: int(row.ReservePercent), WarnPercent: int(row.WarnPercent),
		StorageBytes: row.StorageBytes, UpdatedAt: row.UpdatedAt,
	}, nil
}

// installUsageWith totals every team's committed use together, as it is or
// with a change applied — usageWith for the whole install.
func (s *Service) installUsageWith(ctx context.Context, q *dbgen.Queries, c *quotaChange) (Usage, error) {
	rows, err := q.ListInstallFootprints(ctx)
	if err != nil {
		return Usage{}, fmt.Errorf("app: read the install's committed resources: %w", err)
	}
	storage, err := q.SumInstallVolumeBytes(ctx)
	if err != nil {
		return Usage{}, fmt.Errorf("app: read the install's committed storage: %w", err)
	}
	apps := make([]footprint, len(rows))
	for i, a := range rows {
		apps[i] = footprint{a.ID, shape{Replicas: a.Replicas, CPULimit: a.CpuLimit, MemoryLimit: a.MemoryLimit}}
	}
	return tally(apps, storage, c), nil
}

// CapacityPolicy returns the install's policy.
func (s *Service) CapacityPolicy(ctx context.Context) (CapacityPolicy, error) {
	return capacityPolicy(ctx, s.q.GetCapacityPolicy)
}

// SetCapacityPolicy replaces the install's policy.
//
// Like a quota, it takes effect on the next change and never on what is
// already running: turning enforcement on over an install that has sold past
// its machines stops nothing, and refuses only what would sell more.
func (s *Service) SetCapacityPolicy(ctx context.Context, p CapacityPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := s.q.SetCapacityPolicy(ctx, dbgen.SetCapacityPolicyParams{
		Enforce: p.Enforce, CpuCommitRatio: p.CPURatio, MemoryCommitRatio: p.MemoryRatio,
		ReservePercent: int32(p.ReservePercent), WarnPercent: int32(p.WarnPercent),
		StorageBytes: p.StorageBytes,
	}); err != nil {
		return fmt.Errorf("app: set capacity policy: %w", err)
	}
	s.log.Info("capacity policy set", slog.Bool("enforce", p.Enforce),
		slog.Float64("cpu_ratio", p.CPURatio), slog.Float64("memory_ratio", p.MemoryRatio),
		slog.Int("reserve_percent", p.ReservePercent), slog.Int("warn_percent", p.WarnPercent),
		slog.Int64("storage_bytes", p.StorageBytes))
	return nil
}

// CapacityResource is one resource across the whole install. CPU is in
// millicores; memory and storage in bytes.
type CapacityResource struct {
	// Counted is whether the resource is held to anything. CPU and memory
	// are whenever the cluster can be read; storage only when the operator
	// has said how much there is.
	Counted bool

	// Allocatable is what the schedulable nodes offer — for storage, the
	// operator's figure. Sellable is that times the commit ratio, less the
	// reserve. Committed is every team's replicas times limits, or volume
	// sizes. Free is Sellable less Committed, and never below zero.
	Allocatable int64
	Sellable    int64
	Committed   int64
	Free        int64

	// Used is what the nodes report using now; zero unless the snapshot's
	// UsageKnown. Never storage.
	Used int64
}

// Percent is how much of what is sellable is committed. A counted resource
// with nothing sellable — every node cordoned — is full, however little is
// committed.
func (r CapacityResource) Percent() int {
	if r.Sellable <= 0 {
		if r.Counted {
			return 100
		}
		return 0
	}
	return int(math.Round(float64(r.Committed) * 100 / float64(r.Sellable)))
}

// CapacityLevel says in a word how full the install is.
type CapacityLevel string

const (
	// CapacityOK is committed under the warning line.
	CapacityOK CapacityLevel = "ok"
	// CapacityWarn is at or past the warning line and under what is sellable.
	CapacityWarn CapacityLevel = "warn"
	// CapacityFull is at or past what is sellable on any counted resource.
	CapacityFull CapacityLevel = "full"
	// CapacityUnknown is a cluster that could not be read.
	CapacityUnknown CapacityLevel = "unknown"
)

// CapacitySnapshot is the whole install's room at one moment: what the policy
// sells of the machines, what the teams have committed, and what is left.
//
// For the operator and for an application wrapping the engine. It holds
// totals across every team, so nothing in it is for a customer to see beyond
// RoomShort.
type CapacitySnapshot struct {
	// Known is whether the cluster could be read. Without it CPU and memory
	// have no Allocatable or Sellable, and nothing is refused for them.
	Known bool
	// Reason is why not, when not Known.
	Reason string

	// Enforced is whether changes past what is sellable are refused.
	Enforced bool
	Policy   CapacityPolicy

	Nodes       int
	Schedulable int
	UsageKnown  bool

	CPU     CapacityResource
	Memory  CapacityResource
	Storage CapacityResource

	// Apps is how many apps the install has, across every team.
	Apps int64

	WarnPercent int
	Level       CapacityLevel

	// RefusedLast24h is how many changes were refused for capacity in the
	// last day: demand the install turned away.
	RefusedLast24h int

	// RoomShort is whether the install is enforcing and nearly out of room:
	// less free than one default container, or than a twentieth of what is
	// sellable. The one thing here a customer may be told, as a notice that
	// a change may be refused — never as a number.
	RoomShort bool
}

// NewCapacitySnapshot puts a policy, a room and the install's commitments
// together. Service.Capacity is what reads them; this is the arithmetic alone,
// for drawing a state without a cluster or a database behind it.
func NewCapacitySnapshot(p CapacityPolicy, room Room, used Usage, refused int) CapacitySnapshot {
	cpu, memory, storage := p.sellable(room)
	resource := func(counted bool, allocatable, sellable, committed, inUse int64) CapacityResource {
		r := CapacityResource{Counted: counted, Committed: committed}
		if counted {
			r.Allocatable, r.Sellable = allocatable, sellable
			r.Free = max(sellable-committed, 0)
			r.Used = inUse
		}
		return r
	}
	snap := CapacitySnapshot{
		Known: room.Known, Reason: room.Reason, Enforced: p.Enforce, Policy: p,
		Nodes: room.Nodes, Schedulable: room.Schedulable, UsageKnown: room.UsageKnown,
		CPU:            resource(room.Known, room.CPUMillis, cpu, used.CPUMillis, room.CPUUsedMillis),
		Memory:         resource(room.Known, room.MemoryBytes, memory, used.MemoryBytes, room.MemoryUsedBytes),
		Storage:        resource(storage > 0, storage, storage, used.StorageBytes, 0),
		Apps:           used.Apps,
		WarnPercent:    p.WarnPercent,
		RefusedLast24h: refused,
	}

	// Unknown outranks everything else: with the cluster unread nothing is
	// refused, storage included, and "full" would say otherwise.
	snap.Level = CapacityOK
	if !room.Known {
		snap.Level = CapacityUnknown
	}
	for _, r := range []CapacityResource{snap.CPU, snap.Memory, snap.Storage} {
		switch {
		case !r.Counted || !room.Known:
		case r.Percent() >= 100:
			snap.Level = CapacityFull
		case r.Percent() >= p.WarnPercent && snap.Level == CapacityOK:
			snap.Level = CapacityWarn
		}
	}

	short := func(r CapacityResource, container int64) bool {
		return r.Counted && r.Free < max(container, r.Sellable/20)
	}
	snap.RoomShort = p.Enforce && room.Known &&
		(short(snap.CPU, defaultCPUMillis) || short(snap.Memory, defaultMemoryBytes) ||
			short(snap.Storage, 0))
	return snap
}

// Capacity is the install's room now. The cluster's side is at most roomTTL
// old; the teams' side is read as it stands.
func (s *Service) Capacity(ctx context.Context) (CapacitySnapshot, error) {
	p, err := s.CapacityPolicy(ctx)
	if err != nil {
		return CapacitySnapshot{}, err
	}
	used, err := s.installUsageWith(ctx, s.q, nil)
	if err != nil {
		return CapacitySnapshot{}, err
	}
	refused, err := s.q.CountCapacityRefusalsSince(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		return CapacitySnapshot{}, fmt.Errorf("app: count capacity refusals: %w", err)
	}
	return NewCapacitySnapshot(p, s.room.get(ctx), used, int(refused)), nil
}

// CapacityRefusal is one change refused for want of room.
type CapacityRefusal struct {
	ID       uuid.UUID
	TeamID   string
	TeamName string

	// Resource is cpu, memory or storage; Shortfall how much more of it the
	// change needed than was free, in millicores or bytes.
	Resource  string
	Shortfall int64
	At        time.Time
}

// CapacityRefusals lists changes refused since a moment, newest first, at most
// limit of them. For the operator; it names every team.
func (s *Service) CapacityRefusals(ctx context.Context, since time.Time, limit int32) ([]CapacityRefusal, error) {
	rows, err := s.q.ListCapacityRefusalsSince(ctx, dbgen.ListCapacityRefusalsSinceParams{
		Since: since, MaxRows: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("app: list capacity refusals: %w", err)
	}
	out := make([]CapacityRefusal, len(rows))
	for i, r := range rows {
		out[i] = CapacityRefusal{
			ID: r.ID, TeamID: r.OwnerID, TeamName: r.DisplayName,
			Resource: r.Resource, Shortfall: r.Shortfall, At: r.At,
		}
	}
	return out, nil
}
