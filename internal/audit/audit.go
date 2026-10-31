// Package audit keeps a team's record of who did what to it, when, and from
// where — Railway's workspace Audit Logs.
//
// It is written by the dashboard and the API alike, after an action has
// succeeded, and never fails one: an action that worked and could not be
// recorded is logged loudly and left done, because undoing a deploy over its
// paperwork would be the wrong way round.
//
// What it must never hold is the reason Event is a short map of named fields
// rather than a copy of the request: a variable's key and never its value,
// that a secret was revealed and never what it was, that a console session
// began and never a keystroke of it.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Actor kinds.
const (
	ActorUser     = "user"
	ActorToken    = "token"
	ActorOperator = "operator"
	ActorSystem   = "system"
)

// Actor is who did something, and from where.
type Actor struct {
	// Kind is one of the Actor* constants. An operator acting as a team for
	// support is ActorOperator, so the team can tell their own people's
	// changes from the platform's.
	Kind string
	// ID is the user's or the token's id; Name what a page shows for them —
	// an address, or a token's name.
	ID   string
	Name string

	IP        string
	UserAgent string
}

// Event is one thing that happened.
type Event struct {
	// Action is a dotted name: deploy.redeploy, variable.set, member.remove.
	Action string
	// TargetKind and Target are what it happened to: "app" and its name,
	// "member" and an address.
	TargetKind string
	Target     string
	// Detail is a few named facts. Only what a caller puts here by name is
	// recorded — never a request body.
	Detail map[string]string
}

// Entry is an event as it was recorded.
type Entry struct {
	ID    uuid.UUID
	At    time.Time
	Actor Actor
	Event
}

// Log records events and reads them back.
type Log struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	log  *slog.Logger
}

// New wires the store into the log.
func New(pool *pgxpool.Pool, log *slog.Logger) *Log {
	if log == nil {
		log = slog.Default()
	}
	return &Log{pool: pool, q: dbgen.New(pool), log: log}
}

// Limits on what one entry holds. Generous for anything a person names, and
// short enough that a value pasted where a name belongs is cut rather than
// kept whole.
const (
	maxField      = 200
	maxDetailKeys = 12
	maxUserAgent  = 300
)

// Record writes one event. Safe on a nil Log, which is an install keeping no
// audit log — a wrapper composing the dashboard without one.
func (l *Log) Record(ctx context.Context, ownerID string, actor Actor, ev Event) {
	if l == nil || ownerID == "" || ev.Action == "" {
		return
	}
	detail := map[string]string{}
	for k, v := range ev.Detail {
		if len(detail) == maxDetailKeys {
			break
		}
		if k == "" || v == "" {
			continue
		}
		detail[clip(k, 40)] = clip(v, maxField)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		raw = []byte("{}")
	}
	kind := actor.Kind
	switch kind {
	case ActorUser, ActorToken, ActorOperator, ActorSystem:
	default:
		kind = ActorSystem
	}
	// Written with a context of its own: the request that did this may be
	// over by the time it is written, and the record must not go with it.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.q.InsertAuditEvent(wctx, dbgen.InsertAuditEventParams{
		OwnerID: ownerID, ActorKind: kind, ActorID: clip(actor.ID, maxField),
		ActorName: clip(actor.Name, maxField), Ip: clip(actor.IP, 64),
		UserAgent: clip(actor.UserAgent, maxUserAgent), Action: clip(ev.Action, 64),
		TargetKind: clip(ev.TargetKind, 40), Target: clip(ev.Target, maxField), Detail: raw,
	}); err != nil {
		l.log.Error("could not record an audit event",
			slog.String("team", ownerID), slog.String("action", ev.Action),
			slog.String("error", err.Error()))
	}
}

func clip(s string, n int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// Filter narrows a team's log.
type Filter struct {
	// Category is an action's first part — "deploy", "variable" — or a whole
	// action; empty is every action.
	Category string
	// Actor matches an actor's name or id, exactly.
	Actor string
	// Query matches a target or an actor's name, anywhere, ignoring case.
	Query string
	// Since and Until bound when, either zero for no bound.
	Since, Until time.Time
	// Before is the paging cursor: entries older than this one.
	Before time.Time
	Limit  int
}

// ErrBadFilter is a filter that cannot be applied.
var ErrBadFilter = errors.New("audit: that filter cannot be applied")

// List reads a team's log, newest first, filtered. Scoped by the owner it is
// given and by nothing a filter can widen: every clause is ANDed with it.
func (l *Log) List(ctx context.Context, ownerID string, f Filter) ([]Entry, error) {
	if l == nil {
		return nil, nil
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	where := []string{"owner_id = $1"}
	args := []any{ownerID}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if c := strings.TrimSpace(f.Category); c != "" {
		if strings.Contains(c, ".") {
			where = append(where, "action = "+arg(c))
		} else {
			where = append(where, "split_part(action, '.', 1) = "+arg(c))
		}
	}
	if a := strings.TrimSpace(f.Actor); a != "" {
		p := arg(a)
		where = append(where, "(actor_name = "+p+" OR actor_id = "+p+")")
	}
	if qy := strings.TrimSpace(f.Query); qy != "" {
		// The pattern's own wildcards are escaped: somebody searching for
		// "50%" means the characters, not "anything starting with 50".
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(qy)
		p := arg("%" + esc + "%")
		where = append(where, "(target ILIKE "+p+" OR actor_name ILIKE "+p+" OR action ILIKE "+p+")")
	}
	if !f.Since.IsZero() {
		where = append(where, "at >= "+arg(f.Since))
	}
	if !f.Until.IsZero() {
		where = append(where, "at < "+arg(f.Until))
	}
	if !f.Before.IsZero() {
		where = append(where, "at < "+arg(f.Before))
	}
	query := `SELECT id, at, actor_kind, actor_id, actor_name, ip, user_agent,
		action, target_kind, target, detail
		FROM audit_events WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY at DESC, id DESC LIMIT ` + arg(f.Limit)
	rows, err := l.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Actor.Kind, &e.Actor.ID, &e.Actor.Name,
			&e.Actor.IP, &e.Actor.UserAgent, &e.Action, &e.TargetKind, &e.Target, &detail); err != nil {
			return nil, fmt.Errorf("audit: scan: %w", err)
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Actors is everybody who appears in a team's log, for the filter's choices.
func (l *Log) Actors(ctx context.Context, ownerID string) ([]string, error) {
	if l == nil {
		return nil, nil
	}
	rows, err := l.pool.Query(ctx, `
		SELECT actor_name FROM audit_events
		WHERE owner_id = $1 AND actor_name <> ''
		GROUP BY actor_name ORDER BY max(at) DESC LIMIT 100`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("audit: actors: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Categories are the first parts of the actions the log records, in the
// order the filter offers them.
var Categories = []string{
	"deploy", "app", "variable", "domain", "storage", "bucket", "project", "environment",
	"member", "invitation", "team", "token", "webhook", "console", "secret", "github", "acting",
}
