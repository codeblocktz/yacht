package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Regions: where an app lives.
//
// A region is a node label, topology.kubernetes.io/region, written when a
// machine joins (see the cluster package). An app is placed in one when it is
// created, and its pods, its volumes and its databases stay there: every pod
// it runs carries a required node affinity on the region, and every volume
// made for it is provisioned from the region's StorageClass.
//
// Which regions exist is the nodes' to say. The regions table keeps what a
// node cannot: what a region is called, its edge — where its traffic enters —
// the StorageClass its volumes use, and whether new apps may be placed in it.
//
// An install whose nodes carry no region has none of this. No app is given a
// region, no pod an affinity, no form a field, and nothing about it changes:
// that is what every install was before regions existed, and what most still
// are.

// ErrNoSuchRegion means no node carries the region and no row names it.
var ErrNoSuchRegion = errors.New("app: no such region")

// ErrRegionClosed means a region cannot take new apps: it is closed, it has
// not been configured, or none of its machines is ready.
var ErrRegionClosed = errors.New("app: that region is not open for new apps")

// ErrNoRegionOpen means the install has regions and none can take a new app.
var ErrNoRegionOpen = errors.New("app: no region is open for new apps — an operator opens one under Admin › Regions")

// ErrNoRegions means a region was asked for on an install that has none.
var ErrNoRegions = errors.New("app: this install has no regions — no node carries " +
	orchestrator.RegionLabel)

// ErrMoveHasStorage means an app with a volume was asked to change region.
//
// A volume is data on the region's machines, and moving it is copying it —
// which nothing here does. Refused rather than attempted: a redeploy into
// another region would leave the pod unable to mount what it had, or mount a
// new empty volume where the data was.
var ErrMoveHasStorage = errors.New("app: an app with storage stays in its region — " +
	"its volumes live on that region's machines and cannot move with it. " +
	"Back the data up, deploy a new app in the other region, and restore it there")

// Region is a place an app can live in, as the operator has described it.
type Region struct {
	// Name is the topology.kubernetes.io/region label value.
	Name string

	// DisplayName is what people are shown. Empty shows Name.
	DisplayName string

	// Edge is where the region's traffic enters.
	Edge Edge

	// StorageClass is the class a volume made in this region is provisioned
	// from. Empty is the cluster's default.
	StorageClass string

	// Open is whether new apps may be placed here.
	Open bool

	UpdatedAt time.Time
}

// Label is what to call the region on a page.
func (r Region) Label() string {
	if r.DisplayName != "" {
		return r.DisplayName
	}
	return r.Name
}

// Edge is where a region's ingress answers: the address a hostname in the
// region should resolve to.
type Edge struct {
	// Host is a hostname that resolves to the region's ingress. What a custom
	// domain on an app in the region is told to CNAME to.
	Host string

	// Addresses are the IPs the region's ingress answers on, for whoever
	// writes A and AAAA records rather than CNAMEs.
	Addresses []string
}

// Set reports whether anything is known about the edge.
func (e Edge) Set() bool { return e.Host != "" || len(e.Addresses) > 0 }

// Equal reports whether two edges are the same, addresses in any order.
func (e Edge) Equal(o Edge) bool {
	if e.Host != o.Host || len(e.Addresses) != len(o.Addresses) {
		return false
	}
	a, b := slices.Clone(e.Addresses), slices.Clone(o.Addresses)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// A dotted DNS name, the shape a CNAME target has to have.
var edgeHostRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

// A StorageClass name: a lowercase RFC 1123 subdomain.
var storageClassRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// Validate checks a region before it is stored. Every field ends up somewhere
// strict — a node affinity, a StorageClass reference, a DNS record — so each
// is held to that shape here, where the message can say what was wrong.
func (r Region) Validate() error {
	if r.Name == "" {
		return errors.New("a region needs a name")
	}
	if err := orchestrator.ValidateRegion(r.Name); err != nil {
		return err
	}
	if len(r.DisplayName) > 80 {
		return errors.New("a region's display name is at most 80 characters")
	}
	if strings.ContainsAny(r.DisplayName, "\n\r\t") {
		return errors.New("a region's display name is one line")
	}
	if r.Edge.Host != "" && (len(r.Edge.Host) > 253 || !edgeHostRE.MatchString(r.Edge.Host)) {
		return fmt.Errorf("the edge %q is not a hostname — a name like edge-fsn.example.test, "+
			"with no scheme, port or path", r.Edge.Host)
	}
	for _, a := range r.Edge.Addresses {
		addr, err := netip.ParseAddr(a)
		if err != nil || addr.Zone() != "" || addr.String() != a {
			return fmt.Errorf("the edge address %q is not an IP address", a)
		}
	}
	if r.StorageClass != "" && (len(r.StorageClass) > 253 || !storageClassRE.MatchString(r.StorageClass)) {
		return fmt.Errorf("%q is not a StorageClass name", r.StorageClass)
	}
	return nil
}

// RegionInfo is a region as the operator sees it: what is configured for it
// and what the cluster says is there.
type RegionInfo struct {
	Region

	// Configured is whether the region has a row. A region the nodes carry
	// and nobody has configured is shown, and cannot take new apps until it
	// is: nothing says where its traffic enters.
	Configured bool

	// Nodes is the machines carrying the region; Ready those that are ready
	// and not cordoned, which are the ones new pods can land on.
	Nodes int
	Ready int

	// Sites are the distinct sites among the region's machines.
	Sites []string

	// Apps is how many apps live here, across every team, counting the
	// unrecorded ones where they will be recorded.
	Apps int64

	// Default is whether new apps go here when nobody chooses.
	Default bool

	// Choosable is whether a new app may be placed here now: configured,
	// open, and with a ready machine.
	Choosable bool
}

// RegionChoice is what a form offering a region needs.
type RegionChoice struct {
	// InUse is whether the install has regions at all. False draws no field.
	InUse bool

	// Options are the regions a new app may be placed in, by name.
	Options []RegionInfo

	// Default is the one selected when nobody chooses.
	Default string
}

// RegionChangeKind says what changed about routing.
type RouteChangeKind string

const (
	// RouteChangeApp is one app's hostnames, or the region it lives in.
	RouteChangeApp RouteChangeKind = "app"

	// RouteChangeRegion is a region's edge.
	RouteChangeRegion RouteChangeKind = "region"
)

// RouteChange says which routes may have changed. A hint, not a diff: an
// observer reads Routes for the truth.
type RouteChange struct {
	Kind RouteChangeKind

	// OwnerID, AppID and App are the app, for an app change.
	OwnerID string
	AppID   uuid.UUID
	App     string

	// Region is the region, for a region change.
	Region string
}

// RouteObserver is told when routes may have changed: an app's hostnames or
// region, or a region's edge. For an application that writes DNS, so the
// traffic for an app enters at its region's edge.
//
// Called after the change is committed, from a goroutine of its own, with a
// context that ends after a minute — never on the path of the request that
// made the change, so a slow DNS provider slows nobody's deploy. It is a hint
// rather than a ledger: a change made by another replica, a change made while
// the observer was failing, or an app domain changed in the configuration is
// not announced, so an observer also reconciles from Routes on a timer of its
// own.
type RouteObserver interface {
	RoutesChanged(ctx context.Context, change RouteChange)
}

// RouteObserverFunc adapts a function to RouteObserver.
type RouteObserverFunc func(ctx context.Context, change RouteChange)

// RoutesChanged calls f.
func (f RouteObserverFunc) RoutesChanged(ctx context.Context, change RouteChange) {
	f(ctx, change)
}

// Route is one hostname the install answers, and where its traffic should
// enter.
type Route struct {
	Host string

	// Custom is a domain a team brought, as against the address the install
	// issued its app. A custom domain's DNS is the team's; what its CNAME
	// should point at is Edge.Host.
	Custom bool

	// Routable is whether traffic for it reaches the app: an issued address
	// always, a custom domain once it is proven.
	Routable bool

	OwnerID string
	AppID   uuid.UUID
	App     string

	// Region is where the app lives, empty on an install with no regions or
	// for an app not placed in one yet. Edge is that region's edge, zero
	// when none is configured — the install's own entry point, as before.
	Region string
	Edge   Edge
}

// regionView is the regions as one decision sees them: the machines, the
// rows, and the install's default, read once and used for the whole decision
// so it cannot change underneath itself.
type regionView struct {
	// nodesKnown is whether the cluster could be read. Unknown, the rows are
	// all there is to go on.
	nodesKnown bool
	nodes      []orchestrator.NodeInfo
	rows       map[string]dbgen.Region

	// configured is YACHT_DEFAULT_REGION.
	configured string
}

// regionView reads the regions on q.
func (s *Service) regionView(ctx context.Context, q *dbgen.Queries) (regionView, error) {
	room, nodes := s.room.snapshot(ctx)
	return s.regionViewOf(ctx, q, room, nodes)
}

// regionViewOf is regionView on a reading of the machines the caller already
// has, so a decision about room and one about regions are the same machines.
func (s *Service) regionViewOf(
	ctx context.Context, q *dbgen.Queries, room Room, nodes []orchestrator.NodeInfo,
) (regionView, error) {
	rows, err := q.ListRegions(ctx)
	if err != nil {
		return regionView{}, fmt.Errorf("app: read regions: %w", err)
	}
	v := regionView{
		nodesKnown: room.Known, nodes: nodes,
		rows: make(map[string]dbgen.Region, len(rows)), configured: s.opts.DefaultRegion,
	}
	for _, r := range rows {
		v.rows[r.Name] = r
	}
	return v, nil
}

// inUse is whether the install has regions: some machine carries one. With
// the cluster unreadable, a configured region stands in for the machines, so
// that a moment's API outage does not create apps with no region on an
// install that has them.
func (v regionView) inUse() bool {
	if !v.nodesKnown {
		return len(v.rows) > 0
	}
	for _, n := range v.nodes {
		if n.Region != "" {
			return true
		}
	}
	return false
}

// names is every region the machines carry or a row names, sorted.
func (v regionView) names() []string {
	seen := map[string]bool{}
	for _, n := range v.nodes {
		if n.Region != "" {
			seen[n.Region] = true
		}
	}
	for name := range v.rows {
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// ready is how many of a region's machines new pods can land on.
func (v regionView) ready(region string) int {
	n := 0
	for _, node := range v.nodes {
		if node.Region == region && node.Ready && !node.Unschedulable {
			n++
		}
	}
	return n
}

// choosable is whether a new app may be placed in region now: it has a row
// that says it is open, and a machine ready to run it. With the cluster
// unreadable the row alone decides — refusing every create while the API
// server restarts would be worse than an app that waits for a machine.
func (v regionView) choosable(region string) bool {
	row, ok := v.rows[region]
	if !ok || !row.Open {
		return false
	}
	return !v.nodesKnown || v.ready(region) > 0
}

// choices is every region a new app may be placed in, by name.
func (v regionView) choices() []string {
	var out []string
	for _, name := range v.names() {
		if v.choosable(name) {
			out = append(out, name)
		}
	}
	return out
}

// newAppDefault is where a new app goes when nobody chooses: the install's
// default when it can take apps, or else the first region that can. Empty
// when none can.
func (v regionView) newAppDefault() string {
	if v.configured != "" && v.choosable(v.configured) {
		return v.configured
	}
	if c := v.choices(); len(c) > 0 {
		return c[0]
	}
	return ""
}

// recordDefault is the region an app that was never placed is taken to be
// in: the install's default, or the only region there is. Empty when that is
// not certain — several regions and no default named — because recording an
// app in a guess would move it the next time it is applied.
func (v regionView) recordDefault() string {
	if v.configured != "" {
		return v.configured
	}
	if c := v.choices(); len(c) == 1 {
		return c[0]
	}
	if names := v.names(); len(names) == 1 {
		return names[0]
	}
	return ""
}

// effective is the region an app counts as being in: the one recorded, or
// for one not recorded yet, where it will be.
func (v regionView) effective(recorded *string) string {
	if recorded != nil && *recorded != "" {
		return *recorded
	}
	if !v.inUse() {
		return ""
	}
	return v.recordDefault()
}

// roomIn is the room on one region's machines. Known whenever the cluster
// is: a region with no machine ready has no room, which is an answer, not an
// unknown.
func (v regionView) roomIn(region string) Room {
	var in []orchestrator.NodeInfo
	for _, n := range v.nodes {
		if n.Region == region {
			in = append(in, n)
		}
	}
	r := RoomOf(in)
	r.Known, r.Reason = v.nodesKnown, ""
	return r
}

// region returns what is known about one region, row or not.
func (v regionView) region(name string) Region {
	r := Region{Name: name}
	if row, ok := v.rows[name]; ok {
		r = toRegion(row)
	}
	return r
}

// resolveNew decides the region a new app is created in, from what was asked
// for. Empty on an install with no regions, where asking for one is refused.
func (v regionView) resolveNew(asked string) (string, error) {
	asked = strings.TrimSpace(asked)
	if !v.inUse() {
		if asked != "" {
			return "", ErrNoRegions
		}
		return "", nil
	}
	if asked == "" {
		if d := v.newAppDefault(); d != "" {
			return d, nil
		}
		return "", ErrNoRegionOpen
	}
	if err := orchestrator.ValidateRegion(asked); err != nil {
		return "", err
	}
	if !v.choosable(asked) {
		if !slices.Contains(v.names(), asked) {
			return "", fmt.Errorf("%w: %s", ErrNoSuchRegion, asked)
		}
		return "", fmt.Errorf("%w: %s", ErrRegionClosed, asked)
	}
	return asked, nil
}

func toRegion(row dbgen.Region) Region {
	return Region{
		Name: row.Name, DisplayName: row.DisplayName,
		Edge:         Edge{Host: row.EdgeHost, Addresses: slices.Clone(row.EdgeAddresses)},
		StorageClass: row.StorageClass, Open: row.Open, UpdatedAt: row.UpdatedAt,
	}
}

// Regions lists every region the machines carry or the operator has
// configured, with what is in each. For the operator, and for an application
// wrapping the engine; it counts every team's apps.
func (s *Service) Regions(ctx context.Context) ([]RegionInfo, error) {
	v, err := s.regionView(ctx, s.q)
	if err != nil {
		return nil, err
	}
	counts, err := s.q.CountAppsByRegion(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: count apps by region: %w", err)
	}
	apps := map[string]int64{}
	for _, c := range counts {
		apps[v.effective(c.Region)] += c.Apps
	}
	def := v.newAppDefault()

	out := make([]RegionInfo, 0)
	for _, name := range v.names() {
		_, configured := v.rows[name]
		info := RegionInfo{
			Region: v.region(name), Configured: configured, Apps: apps[name],
			Default: name == def, Choosable: v.choosable(name),
		}
		sites := map[string]bool{}
		for _, n := range v.nodes {
			if n.Region != name {
				continue
			}
			info.Nodes++
			if n.Ready && !n.Unschedulable {
				info.Ready++
			}
			if n.Site != "" && !sites[n.Site] {
				sites[n.Site] = true
				info.Sites = append(info.Sites, n.Site)
			}
		}
		slices.Sort(info.Sites)
		out = append(out, info)
	}
	return out, nil
}

// Region returns one region, configured or only carried by machines.
func (s *Service) Region(ctx context.Context, name string) (RegionInfo, error) {
	all, err := s.Regions(ctx)
	if err != nil {
		return RegionInfo{}, err
	}
	for _, r := range all {
		if r.Name == name {
			return r, nil
		}
	}
	return RegionInfo{}, fmt.Errorf("%w: %s", ErrNoSuchRegion, name)
}

// RegionChoice is what a form offering a region draws: whether to draw one at
// all, what may be chosen, and what is chosen already.
func (s *Service) RegionChoice(ctx context.Context) (RegionChoice, error) {
	v, err := s.regionView(ctx, s.q)
	if err != nil {
		return RegionChoice{}, err
	}
	if !v.inUse() {
		return RegionChoice{}, nil
	}
	choice := RegionChoice{InUse: true, Default: v.newAppDefault()}
	for _, name := range v.choices() {
		info := RegionInfo{
			Region: v.region(name), Configured: true, Ready: v.ready(name),
			Default: name == choice.Default, Choosable: true,
		}
		choice.Options = append(choice.Options, info)
	}
	return choice, nil
}

// SetRegion configures a region: what it is called, its edge, its volumes'
// StorageClass, and whether new apps may be placed in it.
//
// A region nobody has joined a machine to yet may be configured, so an
// installer can describe a region before its machines arrive; it cannot take
// an app until one does.
//
// Nothing already running is moved. Closing a region stops new apps arriving;
// a new StorageClass is used for volumes made from now on, and never for an
// existing claim, whose class Kubernetes does not let change. A new edge is
// announced to the route observer, which is what moves the region's traffic.
func (s *Service) SetRegion(ctx context.Context, r Region) error {
	r.Name = strings.TrimSpace(r.Name)
	r.DisplayName = strings.TrimSpace(r.DisplayName)
	r.Edge.Host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.Edge.Host)), ".")
	r.StorageClass = strings.TrimSpace(r.StorageClass)
	addrs := make([]string, 0, len(r.Edge.Addresses))
	for _, a := range r.Edge.Addresses {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	r.Edge.Addresses = addrs
	if err := r.Validate(); err != nil {
		return err
	}

	var before Edge
	if row, err := s.q.GetRegion(ctx, r.Name); err == nil {
		before = toRegion(row).Edge
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("app: read region: %w", err)
	}

	if _, err := s.q.UpsertRegion(ctx, dbgen.UpsertRegionParams{
		Name: r.Name, DisplayName: r.DisplayName, EdgeHost: r.Edge.Host,
		EdgeAddresses: r.Edge.Addresses, StorageClass: r.StorageClass, Open: r.Open,
	}); err != nil {
		return fmt.Errorf("app: set region: %w", err)
	}
	s.log.Info("region set", slog.String("region", r.Name), slog.Bool("open", r.Open),
		slog.String("edge", r.Edge.Host), slog.String("storage_class", r.StorageClass))

	if !before.Equal(r.Edge) {
		// Every custom domain on an app in this region renders its record
		// from the edge, and an app publishing a CNAME through ExternalDNS
		// carries it in an annotation: versioning them is what reaches the
		// cluster, the way a changed install target does.
		if err := s.versionRegionApps(ctx, r.Name); err != nil {
			return err
		}
		s.notifyRoutes(RouteChange{Kind: RouteChangeRegion, Region: r.Name})
	}
	return nil
}

// versionRegionApps re-applies the apps in a region that publish a CNAME, so
// the annotation follows a changed edge. Only those: nothing else in an app's
// workload reads the edge.
func (s *Service) versionRegionApps(ctx context.Context, region string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE apps SET config_version = config_version + 1, updated_at = now()
		WHERE region = $1 AND cname_only`, region); err != nil {
		return fmt.Errorf("app: version the region's apps: %w", err)
	}
	return nil
}

// AppRegion is the region an app lives in: the one recorded, or for an app
// made before regions existed and not recorded yet, the one it will be. Empty
// on an install with no regions.
func (s *Service) AppRegion(ctx context.Context, ownerID, name string) (string, error) {
	row, err := s.q.GetApp(ctx, dbgen.GetAppParams{OwnerID: ownerID, Name: name})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("app: read app: %w", err)
	}
	v, err := s.regionView(ctx, s.q)
	if err != nil {
		return "", err
	}
	return v.effective(row.Region), nil
}

// MoveApp places an app in another region: it is redeployed there, the way a
// team's pool change rolls its apps — a replacement ready in the new region
// before the old pod stops.
//
// Only an app without storage moves. An app with a volume, which every
// database has, is refused with ErrMoveHasStorage: its data is on the old
// region's machines. The target has to be open to new apps, and the app is
// admitted against the room in it as if it were new there; the team's quota
// does not change, because the team holds exactly what it held.
func (s *Service) MoveApp(ctx context.Context, ownerID, name, region string) error {
	region = strings.TrimSpace(region)
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if !a.Source.Workload() {
		return ErrNoWorkload
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin region move: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed
	q := s.q.WithTx(tx)

	v, err := s.regionView(ctx, q)
	if err != nil {
		return err
	}
	if !v.inUse() {
		return ErrNoRegions
	}
	current := v.effective(regionPtr(a.Region))
	if region == current && a.Region != "" {
		return nil
	}
	vols, err := s.volumesFor(ctx, q, a.ID)
	if err != nil {
		return err
	}
	if len(vols) > 0 {
		if region == current {
			// Recording where it already is moves nothing, storage or not.
			return s.recordRegion(ctx, q, tx, a, region)
		}
		return ErrMoveHasStorage
	}
	if _, err := v.resolveNew(region); err != nil {
		return err
	}

	// Admitted as if it were new in the target: the region's room against
	// every app already there and this one. Not against the team's quota,
	// which counts the same app either way.
	if region != current {
		c := quotaChange{Region: region, Added: []shape{{
			Replicas: a.Replicas, CPULimit: a.CPULimit, MemoryLimit: a.MemoryLimit,
		}}}
		if err := s.withinCapacity(ctx, q, ownerID, c); err != nil {
			return err
		}
	}

	if _, err := q.SetAppRegion(ctx, dbgen.SetAppRegionParams{
		OwnerID: ownerID, ID: a.ID, Region: &region,
	}); err != nil {
		return fmt.Errorf("app: move region: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit region move: %w", err)
	}
	s.log.Info("app moved region", slog.String("owner", ownerID), slog.String("app", name),
		slog.String("from", current), slog.String("to", region))
	s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: ownerID, AppID: a.ID, App: a.Name})
	return nil
}

// recordRegion records the region an app is already in and commits.
func (s *Service) recordRegion(ctx context.Context, q *dbgen.Queries, tx pgx.Tx, a App, region string) error {
	if _, err := q.RecordAppRegion(ctx, dbgen.RecordAppRegionParams{
		OwnerID: a.OwnerID, ID: a.ID, Region: &region,
	}); err != nil {
		return fmt.Errorf("app: record region: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit region: %w", err)
	}
	s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: a.OwnerID, AppID: a.ID, App: a.Name})
	return nil
}

// regionStorageClass is the class a new volume in region is provisioned
// from, empty for the cluster's default.
func regionStorageClass(ctx context.Context, q *dbgen.Queries, region string) (string, error) {
	if region == "" {
		return "", nil
	}
	row, err := q.GetRegion(ctx, region)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("app: read region: %w", err)
	}
	return row.StorageClass, nil
}

// regionEdge is a region's edge, zero when it has none.
func (s *Service) regionEdge(ctx context.Context, q *dbgen.Queries, region string) Edge {
	if region == "" {
		return Edge{}
	}
	row, err := q.GetRegion(ctx, region)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Warn("read region edge", slog.String("region", region),
				slog.String("error", err.Error()))
		}
		return Edge{}
	}
	return toRegion(row).Edge
}

// Routes is every hostname the install answers, with the region of the app
// it belongs to and that region's edge: what an application writing DNS
// needs, so that the traffic for an app enters at its region's edge.
//
// Custom domains are included whether or not they are proven yet, marked
// Routable when they are, so an application fronting them — issuing them
// certificates at its own edge, say — can prepare one before it goes live.
func (s *Service) Routes(ctx context.Context) ([]Route, error) {
	v, err := s.regionView(ctx, s.q)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListRoutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list routes: %w", err)
	}
	out := make([]Route, 0, len(rows))
	for _, r := range rows {
		region := v.effective(r.Region)
		out = append(out, Route{
			Host: r.Host, Custom: !r.Managed, Routable: r.Managed || r.Verified,
			OwnerID: r.OwnerID, AppID: r.AppID, App: r.AppName,
			Region: region, Edge: v.region(region).Edge,
		})
	}
	return out, nil
}

// notifyRoutes tells the route observer, if there is one, that routes may
// have changed. In a goroutine of its own and after the change is committed,
// for the reasons RouteObserver gives.
func (s *Service) notifyRoutes(change RouteChange) {
	if s.opts.Routes == nil {
		return
	}
	s.routesWG.Add(1)
	go func() {
		defer s.routesWG.Done()
		defer func() {
			// An observer is the wrapper's code. One that panics must not
			// take the engine with it.
			if p := recover(); p != nil {
				s.log.Error("route observer panicked", slog.Any("panic", p))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.opts.Routes.RoutesChanged(ctx, change)
	}()
}

// regionPtr is a region as the database holds it: nil for none.
func regionPtr(region string) *string {
	if region == "" {
		return nil
	}
	return &region
}

// regionBackfillInterval is how often apps not yet placed in a region are
// looked for. Long, because the work is done once per app and an install that
// has finished has nothing to look at but one cheap query.
const regionBackfillInterval = 5 * time.Minute

// RunRegionBackfill records, for every app made before regions existed, the
// region it lives in, until there are none left; see BackfillRegions.
func (s *Service) RunRegionBackfill(ctx context.Context) {
	tick := time.NewTicker(regionBackfillInterval)
	defer tick.Stop()
	for {
		if _, err := s.BackfillRegions(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("could not record apps' regions", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// BackfillRegions records the region of each app made before regions existed,
// and reports how many it recorded.
//
// Recording is where the app already is, never a move, so it is decided by
// what the cluster says before anything else:
//
//   - An app with pods running in exactly one region is recorded there.
//   - An app with no pods running and no storage is recorded in the default
//     region — YACHT_DEFAULT_REGION, or the only region there is — when that
//     region has a ready machine.
//   - Anything else waits for a later pass: an app with storage that is not
//     running, whose volume may be on any region's machines; an app spread
//     across several regions with no default to settle it; an install with
//     several regions and no default named.
//
// Nothing is restarted. A recorded region reaches an app's pods at its next
// apply — a deploy, a setting changed — as the affinity it then carries, and
// until then the app runs where it ran. An app never recorded is placed with
// no region affinity at all, which is what it had before.
//
// The default region is given a row, open, so an install labelled with one
// region and YACHT_DEFAULT_REGION set can place new apps with nothing more
// configured.
func (s *Service) BackfillRegions(ctx context.Context) (int, error) {
	if s.opts.DefaultRegion != "" {
		if err := s.q.EnsureRegion(ctx, s.opts.DefaultRegion); err != nil {
			return 0, fmt.Errorf("app: ensure the default region: %w", err)
		}
	}
	unplaced, err := s.q.ListUnplacedApps(ctx)
	if err != nil || len(unplaced) == 0 {
		if err != nil {
			err = fmt.Errorf("app: list apps with no region: %w", err)
		}
		return 0, err
	}
	v, err := s.regionView(ctx, s.q)
	if err != nil {
		return 0, err
	}
	if !v.nodesKnown || !v.inUse() {
		// Nothing to record into, or no way to see where anything is.
		return 0, nil
	}
	pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{ManagedOnly: true})
	if err != nil {
		return 0, fmt.Errorf("app: read where apps run: %w", err)
	}
	nodeRegion := make(map[string]string, len(v.nodes))
	for _, n := range v.nodes {
		nodeRegion[n.Name] = n.Region
	}
	type key struct{ ns, app string }
	running := map[key]map[string]bool{}
	for _, p := range pods {
		if p.Node == "" || p.Phase != "Running" {
			continue
		}
		k := key{p.Namespace, p.App}
		if running[k] == nil {
			running[k] = map[string]bool{}
		}
		running[k][nodeRegion[p.Node]] = true
	}

	def := v.recordDefault()
	if def != "" && v.ready(def) == 0 {
		def = ""
	}
	recorded := 0
	for _, a := range unplaced {
		var region string
		switch where := running[key{a.Namespace, a.Name}]; {
		case len(where) == 1:
			for r := range where {
				region = r
			}
		case len(where) == 0 && !a.HasVolumes:
			region = def
		case len(where) > 1 && !a.HasVolumes && where[def]:
			region = def
		}
		if region == "" {
			continue
		}
		n, err := s.q.RecordAppRegion(ctx, dbgen.RecordAppRegionParams{
			OwnerID: a.OwnerID, ID: a.ID, Region: &region,
		})
		if err != nil {
			return recorded, fmt.Errorf("app: record region of %s: %w", a.Name, err)
		}
		if n == 0 {
			continue
		}
		recorded++
		s.log.Info("app region recorded", slog.String("owner", a.OwnerID),
			slog.String("app", a.Name), slog.String("region", region))
		s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: a.OwnerID, AppID: a.ID, App: a.Name})
	}
	return recorded, nil
}
