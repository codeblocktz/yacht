package observe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The fields every line carries, as the node collector writes them.
//
// vlagent's own names for the Kubernetes metadata it attaches, and part of the
// contract with whatever is installed beside the engine: a collector that
// names them differently stores lines no query here can find. The first three
// are the collector's default stream fields, which is what lets a namespace
// filter skip every other team's data on disk rather than read and discard it.
const (
	fieldNamespace = "kubernetes.pod_namespace"
	fieldPod       = "kubernetes.pod_name"
	fieldContainer = "kubernetes.container_name"
	fieldOwner     = "kubernetes.pod_labels.yacht/owner-id"
	fieldStream    = "output_stream"
)

// errorFilter is LogsQL for "a line that reports an error".
//
// A level field decides when the line has one: structured loggers say what
// they mean, and a line at level info that mentions an error budget is not an
// error. Pino's numeric levels are read as well, 50 being error and 60 fatal.
// Only a line with no level at all is judged by its words, which is the best
// that can be done for a print statement and is what people expect of a log
// viewer.
//
// Kept in step with isErrorEntry, which applies the same rule to the lines
// that come back so the histogram and the red rows agree.
//
// The doubled backslashes are LogsQL's string escaping: the store unquotes
// "\\b" to the regexp's word boundary, where "\b" would be a backspace.
const errorFilter = `level:~"(?i)^(error|err|fatal|crit|critical|panic|emerg|emergency|alert|50|60)$" or ` +
	`severity:~"(?i)^(error|err|fatal|crit|critical|panic|emerg|emergency|alert)$" or ` +
	`(level:="" severity:="" _msg:~"(?i)\\b(error|exception|fatal|panic|traceback)\\b")`

var (
	errorLevel = regexp.MustCompile(`(?i)^(error|err|fatal|crit|critical|panic|emerg|emergency|alert|50|60)$`)
	errorWords = regexp.MustCompile(`(?i)\b(error|exception|fatal|panic|traceback)\b`)
)

// LogStore reads lines from VictoriaLogs.
type LogStore struct {
	base   *url.URL
	client *http.Client
}

// NewLogStore returns a store for the VictoriaLogs at rawURL, or nil with no
// error when rawURL is empty — the install has no log store, which is allowed.
func NewLogStore(rawURL string, client *http.Client) (*LogStore, error) {
	u, err := storeURL("YACHT_LOGS_URL", rawURL)
	if err != nil || u == nil {
		return nil, err
	}
	if client == nil {
		client = defaultClient()
	}
	return &LogStore{base: u, client: client}, nil
}

// Entry is one line of output.
type Entry struct {
	At        time.Time `json:"at"`
	Service   string    `json:"service"`
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Stream    string    `json:"stream,omitempty"`
	Level     string    `json:"level,omitempty"`
	Message   string    `json:"message"`
	Error     bool      `json:"error,omitempty"`
}

// LogQuery is what a person asked of the log, inside a scope.
type LogQuery struct {
	// Text is matched against the message only, case-insensitively, as a
	// literal: every character in it is escaped before the store sees it.
	Text string

	// ErrorsOnly keeps the lines errorFilter matches.
	ErrorsOnly bool

	Start, End time.Time

	// Limit is the most lines Query returns, newest first from the store and
	// handed back oldest first, the way a log reads. Zero is DefaultLimit.
	Limit int
}

// DefaultLimit and MaxLimit bound one read: enough to fill a screen many times
// over, and few enough that a page of them is not megabytes of HTML.
const (
	DefaultLimit = 500
	MaxLimit     = 5000

	// MaxExport is the most lines one download holds.
	MaxExport = 100_000
)

// filter is the query's own LogsQL, without the scope.
func (q LogQuery) filter() string {
	var parts []string
	if t := strings.TrimSpace(q.Text); t != "" {
		// A regexp rather than a phrase, because a phrase in LogsQL matches
		// whole words and people search for fragments of paths and ids. The
		// text is quoted for the regexp and then for LogsQL, so it is a
		// literal twice over and nothing in it is syntax to either.
		parts = append(parts, "_msg:~"+strconv.Quote("(?i)"+regexp.QuoteMeta(t)))
	}
	if q.ErrorsOnly {
		parts = append(parts, "("+errorFilter+")")
	}
	if len(parts) == 0 {
		return "*"
	}
	return strings.Join(parts, " ")
}

func (q LogQuery) limit(max int) int {
	switch {
	case q.Limit <= 0:
		return DefaultLimit
	case q.Limit > max:
		return max
	}
	return q.Limit
}

// Query returns the newest lines matching q, oldest first.
func (l *LogStore) Query(ctx context.Context, scope Scope, q LogQuery) ([]Entry, error) {
	if l == nil {
		return nil, ErrNotConfigured
	}
	query := q.filter() + " | sort by (_time desc) | limit " + strconv.Itoa(q.limit(MaxLimit))
	var out []Entry
	err := l.stream(ctx, "/select/logsql/query", scope, query, q.Start, q.End, func(e Entry) bool {
		out = append(out, e)
		return true
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b Entry) int { return a.At.Compare(b.At) })
	return out, nil
}

// Export writes every line matching q, up to MaxExport, oldest first, as plain
// text: one line each, time, service and message. Streamed from the store to
// w, so a large download is never held in memory whole.
func (l *LogStore) Export(ctx context.Context, scope Scope, q LogQuery, w io.Writer) error {
	if l == nil {
		return ErrNotConfigured
	}
	query := q.filter() + " | sort by (_time) | limit " + strconv.Itoa(MaxExport)
	var werr error
	err := l.stream(ctx, "/select/logsql/query", scope, query, q.Start, q.End, func(e Entry) bool {
		_, werr = fmt.Fprintf(w, "%s %s %s\n", e.At.UTC().Format(time.RFC3339Nano), e.Service, e.Message)
		return werr == nil
	})
	if werr != nil {
		return werr
	}
	return err
}

// Bucket is one column of the log volume histogram.
type Bucket struct {
	At     time.Time `json:"at"`
	To     time.Time `json:"to"`
	Total  int64     `json:"total"`
	Errors int64     `json:"errors"`
}

// Histogram counts the lines matching q, and the errors among them, in equal
// columns over q's range.
//
// The columns are the store's, aligned to whole multiples of the step, and
// every column in the range is returned whether or not anything was written
// in it: a gap in a log is information, and a chart that closed it up would
// hide exactly the outage somebody is looking for.
func (l *LogStore) Histogram(ctx context.Context, scope Scope, q LogQuery) ([]Bucket, error) {
	if l == nil {
		return nil, ErrNotConfigured
	}
	step := HistogramStep(q.End.Sub(q.Start))
	query := q.filter() + " | stats by (_time:" + promDuration(step) + ") count() total, count() if (" +
		errorFilter + ") errors"

	counts := map[int64]Bucket{}
	err := l.rows(ctx, "/select/logsql/query", scope, query, q.Start, q.End, func(row map[string]string) bool {
		at, err := time.Parse(time.RFC3339Nano, row["_time"])
		if err != nil {
			return true
		}
		total, _ := strconv.ParseInt(row["total"], 10, 64)
		errs, _ := strconv.ParseInt(row["errors"], 10, 64)
		counts[at.Unix()] = Bucket{At: at, Total: total, Errors: errs}
		return true
	})
	if err != nil {
		return nil, err
	}

	first := q.Start.UTC().Truncate(step)
	var out []Bucket
	for at := first; at.Before(q.End); at = at.Add(step) {
		b := counts[at.Unix()]
		b.At, b.To = at, at.Add(step)
		out = append(out, b)
	}
	return out, nil
}

// HistogramStep is how wide one column is for a range: around sixty columns,
// at a width a person would name.
func HistogramStep(span time.Duration) time.Duration {
	for _, s := range []time.Duration{
		10 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour,
		2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
	} {
		if span/s <= 60 {
			return s
		}
	}
	return 24 * time.Hour
}

// Tail follows new lines matching q until ctx ends.
//
// The store holds a line back a few seconds before sending it, so that one
// arriving late from a slow node still comes out in order; the stream is that
// much behind the pod, and never out of order.
func (l *LogStore) Tail(ctx context.Context, scope Scope, q LogQuery) (iter.Seq2[Entry, error], error) {
	if l == nil {
		return nil, ErrNotConfigured
	}
	body, err := l.open(ctx, "/select/logsql/tail", scope, q.filter(), time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	return func(yield func(Entry, error) bool) {
		defer body.Close()
		err := decodeRows(body, func(row map[string]string) bool {
			e, ok := toEntry(scope, row)
			if !ok {
				return true
			}
			return yield(e, nil)
		})
		if err != nil && ctx.Err() == nil {
			yield(Entry{}, err)
		}
	}, nil
}

// stream runs a query and hands each line that belongs to the scope to fn.
func (l *LogStore) stream(
	ctx context.Context, path string, scope Scope, query string, start, end time.Time,
	fn func(Entry) bool,
) error {
	return l.rows(ctx, path, scope, query, start, end, func(row map[string]string) bool {
		e, ok := toEntry(scope, row)
		if !ok {
			return true
		}
		return fn(e)
	})
}

func (l *LogStore) rows(
	ctx context.Context, path string, scope Scope, query string, start, end time.Time,
	fn func(map[string]string) bool,
) error {
	body, err := l.open(ctx, path, scope, query, start, end)
	if err != nil {
		return err
	}
	defer body.Close()
	return decodeRows(body, fn)
}

// open sends a query with the scope attached as the store's extra filter.
func (l *LogStore) open(
	ctx context.Context, path string, scope Scope, query string, start, end time.Time,
) (io.ReadCloser, error) {
	scopeFilter, err := scope.logFilter()
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"query": {query},
		// Applied by the store to the whole query, whatever the query
		// says; see Scope.logFilter.
		"extra_filters": {scopeFilter},
	}
	if !start.IsZero() {
		form.Set("start", start.UTC().Format(time.RFC3339Nano))
	}
	if !end.IsZero() {
		form.Set("end", end.UTC().Format(time.RFC3339Nano))
	}
	return post(ctx, l.client, l.base, path, form)
}

// decodeRows reads the store's answer: one JSON object per line, every value
// a string.
func decodeRows(r io.Reader, fn func(map[string]string) bool) error {
	sc := bufio.NewScanner(r)
	// A line is one log line and its fields; a stack trace logged as one
	// line can run long, and a line the scanner cannot hold ends the read.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row map[string]string
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("observe: read a log line: %w", err)
		}
		if !fn(row) {
			return nil
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("observe: read the log store: %w", err)
	}
	return nil
}

// toEntry labels a row with its service, and drops it when its namespace is
// not the scope's. The store's extra filter means that never happens; this is
// the check that it did not.
func toEntry(scope Scope, row map[string]string) (Entry, bool) {
	service, ok := scope.ServiceFor(row[fieldNamespace])
	if !ok {
		return Entry{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, row["_time"])
	e := Entry{
		At:        at,
		Service:   service,
		Pod:       row[fieldPod],
		Container: row[fieldContainer],
		Stream:    row[fieldStream],
		Level:     firstOf(row, "level", "severity"),
		Message:   row["_msg"],
	}
	e.Error = isErrorEntry(e.Level, row["severity"], e.Message)
	return e, true
}

// isErrorEntry is errorFilter, for one line already read.
func isErrorEntry(level, severity, msg string) bool {
	if level != "" || severity != "" {
		return errorLevel.MatchString(level) || errorLevel.MatchString(severity)
	}
	return errorWords.MatchString(msg)
}

func firstOf(row map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := row[k]; v != "" {
			return v
		}
	}
	return ""
}
