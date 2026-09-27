package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Sleep: apps that scale to zero when nobody is using them, and wake on the
// next request.
//
// Most small apps are idle most of the day, and an idle app holds its CPU and
// memory all the same. One that has had no requests for a while is scaled to
// zero and its hostnames routed to the engine's waker; the next request for
// it scales it back up, and is held until it can be answered. What it gave
// back is counted as free, less a reserve kept so that the sleepers can still
// wake — see capacity.go.
//
// Off unless somebody turns it on: an app's own setting, or its team's
// default, which is off until a person or an application wrapping the engine
// changes it.

var (
	// ErrSleepUnavailable means this install has no waker the cluster can
	// reach, so an app put to sleep could never be woken again.
	ErrSleepUnavailable = errors.New("app: this install cannot wake apps, so none can sleep — " +
		"set YACHT_WAKER_ADDR to an address the cluster reaches the engine at")

	// ErrCannotSleep means this app cannot be woken by a request: it has no
	// public hostname for one to arrive at.
	ErrCannotSleep = errors.New("app: only an app with a public hostname can sleep — " +
		"a request to it is what wakes it")

	// ErrWakeNoRoom means a sleeping app could not be woken because the
	// install has no room for it right now. It stays asleep, and the next
	// request after a minute tries again.
	ErrWakeNoRoom = errors.New("app: there's no room to wake this app right now — try again in a minute")

	// ErrWakeTimeout means the app did not become ready within the wake
	// timeout. It is put back to sleep rather than left half-woken.
	ErrWakeTimeout = errors.New("app: the app did not become ready in time to wake")
)

const (
	// DefaultSleepAfter is how long an app is idle before it sleeps, where
	// nobody has said otherwise.
	DefaultSleepAfter = 30 * time.Minute

	// MinSleepAfter is the shortest idle time allowed. Shorter, and an app
	// somebody is clicking through slowly spends its day waking up.
	MinSleepAfter = 5 * time.Minute

	// MaxSleepAfter is the longest: a week, the table's own bound.
	MaxSleepAfter = 7 * 24 * time.Hour
)

// SleepMode is an app's own choice: its team's default, or on or off
// whatever the team says.
type SleepMode string

const (
	SleepInherit SleepMode = "inherit"
	SleepOn      SleepMode = "on"
	SleepOff     SleepMode = "off"
)

// SleepPolicy is whether apps sleep and after how long without a request.
type SleepPolicy struct {
	Enabled bool
	After   time.Duration
}

// DefaultSleepPolicy is every team's until it is changed: off.
func DefaultSleepPolicy() SleepPolicy {
	return SleepPolicy{After: DefaultSleepAfter}
}

// Validate refuses an idle time outside the bounds, in whole minutes.
func (p SleepPolicy) Validate() error {
	return validSleepAfter(p.After)
}

func validSleepAfter(d time.Duration) error {
	switch {
	case d%time.Minute != 0:
		return errors.New("the idle time is a whole number of minutes")
	case d < MinSleepAfter:
		return fmt.Errorf("an app sleeps after %d minutes idle at the soonest", int(MinSleepAfter.Minutes()))
	case d > MaxSleepAfter:
		return errors.New("an app sleeps after a week idle at the latest")
	}
	return nil
}

// SleepSetting is what one app asks for.
type SleepSetting struct {
	Mode SleepMode

	// After is the app's own idle time; zero takes the team's.
	After time.Duration
}

// Validate refuses a mode that is not one, and an idle time out of bounds.
func (s SleepSetting) Validate() error {
	switch s.Mode {
	case SleepInherit, SleepOn, SleepOff:
	default:
		return fmt.Errorf("app: %q is not a sleep setting", s.Mode)
	}
	if s.After == 0 {
		return nil
	}
	return validSleepAfter(s.After)
}

// Effective is the policy the app actually runs under, given its team's.
func (s SleepSetting) Effective(team SleepPolicy) SleepPolicy {
	p := team
	switch s.Mode {
	case SleepOn:
		p.Enabled = true
	case SleepOff:
		p.Enabled = false
	}
	if s.After > 0 {
		p.After = s.After
	}
	if p.After == 0 {
		p.After = DefaultSleepAfter
	}
	return p
}

// SleepState is where an app stands.
type SleepState string

const (
	SleepAwake  SleepState = "awake"
	SleepAsleep SleepState = "asleep"
	// SleepWaking is between its pods being asked for and its route coming
	// back to them.
	SleepWaking SleepState = "waking"
)

// Why a wake did not happen, as recorded on the app.
const (
	WakeFailedNoRoom  = "no-room"
	WakeFailedTimeout = "timeout"
)

// How a sleep ended, as recorded in its interval.
const (
	WokeByRequest = "request"
	WokeByManual  = "manual"
	WokeByDeploy  = "deploy"
	// WokeByInstall is the install waking every sleeper because it can no
	// longer route to a waker.
	WokeByInstall = "install"
)

// Sleep is an app's sleeping, read with the app.
type Sleep struct {
	Setting SleepSetting
	State   SleepState

	// Since is when it fell asleep, nil while awake.
	Since *time.Time

	// LastRequestAt is when a request for it was last seen — to a minute or
	// so, the interval its counter is read at. Nil when none has been.
	LastRequestAt *time.Time

	// AwakeSince is when it last woke, or was deployed, or had its setting
	// changed: the idle clock never starts earlier.
	AwakeSince time.Time

	// CountedSince is when its requests were first counted. Nil means they
	// never have been, and an app nobody can count requests for is never
	// taken to be idle.
	CountedSince *time.Time

	// WakeFailedAt and WakeFailure are the last wake that did not happen and
	// why: WakeFailedNoRoom or WakeFailedTimeout.
	WakeFailedAt *time.Time
	WakeFailure  string

	wakingSince *time.Time
}

// Asleep reports whether the app is not serving because it sleeps: asleep,
// or on its way back.
func (s Sleep) Asleep() bool { return s.State == SleepAsleep || s.State == SleepWaking }

// IdleSince is when the app's idle clock started, and false when its
// requests have never been counted.
func (s Sleep) IdleSince() (time.Time, bool) {
	if s.CountedSince == nil {
		return time.Time{}, false
	}
	t := s.AwakeSince
	for _, c := range []*time.Time{s.CountedSince, s.LastRequestAt} {
		if c != nil && c.After(t) {
			t = *c
		}
	}
	return t, true
}

// RunningReplicas is how many replicas the app runs now: none while it is
// asleep, its own count otherwise. What a meter charging for replicas reads.
func (a App) RunningReplicas() int32 {
	if a.Sleep.State == SleepAsleep {
		return 0
	}
	return a.Replicas
}

func toSleep(row dbgen.App) Sleep {
	s := Sleep{
		Setting:       SleepSetting{Mode: SleepMode(row.SleepMode)},
		State:         SleepState(row.SleepState),
		Since:         timePtr(row.SleepingSince),
		LastRequestAt: timePtr(row.LastRequestAt),
		AwakeSince:    row.AwakeSince,
		CountedSince:  timePtr(row.RequestsSeenSince),
		WakeFailedAt:  timePtr(row.WakeFailedAt),
		WakeFailure:   row.WakeFailure,
		wakingSince:   timePtr(row.WakingSince),
	}
	if row.SleepAfterMinutes != nil {
		s.Setting.After = time.Duration(*row.SleepAfterMinutes) * time.Minute
	}
	return s
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// CanSleep reports whether this install can put apps to sleep at all: it has
// a waker for their hostnames to be routed to.
func (s *Service) CanSleep() bool { return s.opts.Waker != nil }

// sleepable says why an app cannot sleep, or nil when it can.
func (s *Service) sleepable(a App) error {
	switch {
	case !s.CanSleep():
		return ErrSleepUnavailable
	case a.Port == 0 || a.Internal:
		return ErrCannotSleep
	case a.ActiveReleaseID == nil:
		return errors.New("app: an app sleeps once it has been deployed")
	}
	return nil
}

// TeamSleepDefault is a team's default policy, for every app of its left to
// inherit it. A team that never set one has DefaultSleepPolicy: off.
func (s *Service) TeamSleepDefault(ctx context.Context, ownerID string) (SleepPolicy, error) {
	row, err := s.q.GetTeamSleepDefault(ctx, ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DefaultSleepPolicy(), nil
		}
		return SleepPolicy{}, fmt.Errorf("app: read team sleep default: %w", err)
	}
	return SleepPolicy{Enabled: row.Enabled, After: time.Duration(row.AfterMinutes) * time.Minute}, nil
}

// SetTeamSleepDefault replaces a team's default. It changes nothing already
// asleep, and an app idle for longer than the new time sleeps on the next
// pass — a hosting business turning sleep on for a plan means it.
func (s *Service) SetTeamSleepDefault(ctx context.Context, ownerID string, p SleepPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := s.q.UpsertTeamSleepDefault(ctx, dbgen.UpsertTeamSleepDefaultParams{
		OwnerID: ownerID, Enabled: p.Enabled, AfterMinutes: int32(p.After / time.Minute),
	}); err != nil {
		if isForeignKeyViolation(err) {
			return ErrNoSuchTeam
		}
		return fmt.Errorf("app: set team sleep default: %w", err)
	}
	s.log.Info("team sleep default set", slog.String("team", ownerID),
		slog.Bool("enabled", p.Enabled), slog.Duration("after", p.After))
	return nil
}

// SetSleepSetting replaces one app's setting. The idle clock restarts, so an
// app that has been quiet for a week is not put to sleep a minute after
// somebody turns sleeping on; an app already asleep stays asleep until it is
// next asked for.
func (s *Service) SetSleepSetting(ctx context.Context, ownerID, name string, in SleepSetting) (App, error) {
	if err := in.Validate(); err != nil {
		return App{}, err
	}
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return App{}, err
	}
	var after *int32
	if in.After > 0 {
		m := int32(in.After / time.Minute)
		after = &m
	}
	row, err := s.q.SetAppSleepSetting(ctx, dbgen.SetAppSleepSettingParams{
		OwnerID: ownerID, ID: a.ID, SleepMode: string(in.Mode), SleepAfterMinutes: after,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, fmt.Errorf("app: set sleep setting: %w", err)
	}
	s.log.Info("app sleep setting set", slog.String("app", name),
		slog.String("mode", string(in.Mode)), slog.Duration("after", in.After))
	return toApp(row), nil
}

// SleepStatus is everything about one app's sleeping: what it asked for, what
// that comes to with its team's default, where it stands and what happened.
type SleepStatus struct {
	Setting   SleepSetting
	Team      SleepPolicy
	Effective SleepPolicy

	Sleep Sleep

	// Unavailable is why this app cannot sleep, nil when it can.
	Unavailable error

	// History is its most recent sleeps, newest first.
	History []SleepInterval
}

// SleepStatus reads one app's.
func (s *Service) SleepStatus(ctx context.Context, ownerID, name string) (SleepStatus, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return SleepStatus{}, err
	}
	team, err := s.TeamSleepDefault(ctx, ownerID)
	if err != nil {
		return SleepStatus{}, err
	}
	rows, err := s.q.ListAppSleeps(ctx, dbgen.ListAppSleepsParams{
		OwnerID: ownerID, AppID: a.ID, MaxRows: 10,
	})
	if err != nil {
		return SleepStatus{}, fmt.Errorf("app: list sleeps: %w", err)
	}
	st := SleepStatus{
		Setting: a.Sleep.Setting, Team: team, Effective: a.Sleep.Setting.Effective(team),
		Sleep: a.Sleep, Unavailable: s.sleepable(a),
	}
	for _, r := range rows {
		st.History = append(st.History, toSleepInterval(r.ID, r.AppID, a.Name, r.SleptAt, r.IdleMinutes, r.WokeAt, r.WokeBy))
	}
	return st, nil
}

// SleepInterval is one stretch an app spent asleep.
type SleepInterval struct {
	ID      uuid.UUID
	AppID   uuid.UUID
	AppName string

	From time.Time
	// To is when it woke — when its pods were asked for again. Nil while it
	// is still asleep.
	To *time.Time

	// IdleMinutes is the idle time it slept after; nil is somebody putting it
	// to sleep by hand. WokeBy is one of the WokeBy constants, empty while
	// still asleep.
	IdleMinutes *int32
	WokeBy      string
}

// Describe says what happened in a sentence, for an app's history.
func (i SleepInterval) Describe() string {
	out := "Put to sleep by hand"
	if i.IdleMinutes != nil {
		out = "Slept after " + strconv.Itoa(int(*i.IdleMinutes)) + " min without requests"
	}
	return out
}

// DescribeWake says how it ended, empty while it has not.
func (i SleepInterval) DescribeWake() string {
	switch i.WokeBy {
	case WokeByRequest:
		return "Woke on a request"
	case WokeByManual:
		return "Woken by hand"
	case WokeByDeploy:
		return "Woke for a deploy"
	case WokeByInstall:
		return "Woken because the install can no longer route to its waker"
	}
	return ""
}

func toSleepInterval(
	id, appID uuid.UUID, name string, from time.Time, idle *int32, to pgtype.Timestamptz, by *string,
) SleepInterval {
	i := SleepInterval{ID: id, AppID: appID, AppName: name, From: from, To: timePtr(to), IdleMinutes: idle}
	if by != nil {
		i.WokeBy = *by
	}
	return i
}

// SleepIntervals is every stretch a team's apps spent asleep that overlaps
// [from, to), clipped to it. What an application wrapping the engine reads to
// stop charging for an app while it sleeps: an app's awake time in the window
// is the window less these — see AwakeWithin.
func (s *Service) SleepIntervals(ctx context.Context, ownerID string, from, to time.Time) ([]SleepInterval, error) {
	rows, err := s.q.ListSleepIntervals(ctx, dbgen.ListSleepIntervalsParams{
		OwnerID: ownerID, Since: from, Until: to,
	})
	if err != nil {
		return nil, fmt.Errorf("app: list sleep intervals: %w", err)
	}
	out := make([]SleepInterval, 0, len(rows))
	for _, r := range rows {
		i := toSleepInterval(r.ID, r.AppID, r.AppName, r.SleptAt, r.IdleMinutes, r.WokeAt, r.WokeBy)
		if i.From.Before(from) {
			i.From = from
		}
		if i.To != nil && i.To.After(to) {
			end := to
			i.To = &end
		}
		out = append(out, i)
	}
	return out, nil
}

// AwakeWithin is how long an app was awake in [from, to), given the sleep
// intervals SleepIntervals returned for that window. An interval still open is
// asleep until to.
func AwakeWithin(intervals []SleepInterval, appID uuid.UUID, from, to time.Time) time.Duration {
	awake := to.Sub(from)
	for _, i := range intervals {
		if i.AppID != appID {
			continue
		}
		start, end := i.From, to
		if i.To != nil {
			end = *i.To
		}
		start, end = maxTime(start, from), minTime(end, to)
		if end.After(start) {
			awake -= end.Sub(start)
		}
	}
	return max(awake, 0)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// SleepNow puts an app to sleep by hand, whatever its setting — somebody who
// knows nobody will use it tonight need not wait for the idle clock. It wakes
// the same way as any sleeper, on its next request.
func (s *Service) SleepNow(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	return s.sleep(ctx, a, nil)
}

// sleep scales an app to zero and routes its hostnames to the waker.
//
// idle is the idle time it slept after, nil by hand. still, when given, is
// asked again once the app has been re-read under the convergence lock — the
// sleeper decided from a reading that a request may since have overtaken.
func (s *Service) sleep(ctx context.Context, a App, idle *int32, still ...func(App) bool) error {
	if err := s.sleepable(a); err != nil {
		return err
	}
	return s.withAppConvergenceLock(ctx, a.ID, func() error {
		row, err := s.q.GetAppByID(ctx, dbgen.GetAppByIDParams{OwnerID: a.OwnerID, ID: a.ID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("app: re-read app to sleep: %w", err)
		}
		a = toApp(row)
		if a.Sleep.State != SleepAwake {
			return nil
		}
		for _, ok := range still {
			if !ok(a) {
				return nil
			}
		}
		// Never mid-deploy. The operation is applying a release, and sleeping
		// underneath it would leave its rollout waiting for pods that were
		// scaled away.
		if _, err := s.liveOperation(ctx, a); err == nil {
			return ErrOperationInFlight
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		hosts, err := domain.RoutableHosts(ctx, s.q, a.ID)
		if err != nil {
			return err
		}
		if len(hosts) == 0 {
			return ErrCannotSleep
		}
		slept, err := s.markAsleep(ctx, a, idle)
		if err != nil {
			return err
		}
		s.log.Info("app asleep", slog.String("app", a.Name), slog.String("owner", a.OwnerID),
			slog.Any("idle_minutes", idle))
		// A failed apply leaves the new config version unconverged, and the
		// app reconciler applies it again; the database already says asleep.
		return s.apply(ctx, s.q, slept)
	})
}

// markAsleep records the app asleep and opens its interval, together.
func (s *Service) markAsleep(ctx context.Context, a App, idle *int32) (App, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return App{}, fmt.Errorf("app: begin sleep: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	row, err := q.MarkAppAsleep(ctx, dbgen.MarkAppAsleepParams{OwnerID: a.OwnerID, ID: a.ID})
	if err != nil {
		return App{}, fmt.Errorf("app: mark asleep: %w", err)
	}
	if err := q.OpenAppSleep(ctx, dbgen.OpenAppSleepParams{
		OwnerID: a.OwnerID, AppID: a.ID, SleptAt: row.SleepingSince.Time, IdleMinutes: idle,
	}); err != nil {
		return App{}, fmt.Errorf("app: record sleep: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return App{}, fmt.Errorf("app: commit sleep: %w", err)
	}
	return toApp(row), nil
}
