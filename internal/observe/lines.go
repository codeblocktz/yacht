package observe

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ClassifyLine reads the level of a raw line of container output, for lines
// that did not come through the store: the live pod log a project's Logs page
// falls back to. A JSON line is read for its level as the collector would
// have read it; anything else is judged by its words.
func ClassifyLine(text string) (level string, isError bool) {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "{") {
		var fields map[string]any
		if json.Unmarshal([]byte(t), &fields) == nil {
			level = fieldString(fields["level"])
			severity := fieldString(fields["severity"])
			if level == "" {
				level = severity
			}
			if level != "" {
				return level, isErrorEntry(level, severity, "")
			}
		}
	}
	return "", isErrorEntry("", "", text)
}

func fieldString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%g", x)
	}
	return ""
}

// CountLines is Histogram for lines already in hand: the same columns, so a
// page drawn from the live pods reads the same as one drawn from the store.
func CountLines(entries []Entry, start, end time.Time) []Bucket {
	step := HistogramStep(end.Sub(start))
	first := start.UTC().Truncate(step)
	var out []Bucket
	for at := first; at.Before(end); at = at.Add(step) {
		out = append(out, Bucket{At: at, To: at.Add(step)})
	}
	for _, e := range entries {
		if e.At.Before(first) || !e.At.Before(end) {
			continue
		}
		i := int(e.At.Sub(first) / step)
		if i >= len(out) {
			continue
		}
		out[i].Total++
		if e.Error {
			out[i].Errors++
		}
	}
	return out
}

// Matches reports whether a line already in hand passes q's filters, which is
// what the store does with LogsQL for lines it holds.
func (q LogQuery) Matches(e Entry) bool {
	if !q.Start.IsZero() && e.At.Before(q.Start) {
		return false
	}
	if !q.End.IsZero() && e.At.After(q.End) {
		return false
	}
	if q.ErrorsOnly && !e.Error {
		return false
	}
	if t := strings.TrimSpace(q.Text); t != "" && !strings.Contains(strings.ToLower(e.Message), strings.ToLower(t)) {
		return false
	}
	return true
}
