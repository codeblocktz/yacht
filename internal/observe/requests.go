package observe

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// An app's requests, from the ingress controller's access log as the log
// store keeps it.
//
// Read from the store rather than from the controller's pod, because the pod
// log is only the controller's current file: the kubelet rotates it every
// 10 MiB, ten thousand requests or so across every tenant, and what was
// before is gone from the API (QA OBSERVE-1). The node collector reads the
// rotated files as it reads every container's, so the store has them all.

// ingressFilter is LogsQL for the controller's own lines, and nothing else:
// Traefik's container, in the namespaces an installer puts it in. Tenants'
// apps run in namespaces of their own, so an app called traefik that prints
// lines shaped like an access log cannot pass for the controller.
var ingressFilter = `{` + fieldNamespace + ` in ("kube-system","traefik"), ` +
	fieldContainer + `="traefik"}`

// Requests returns the newest requests to any of hosts between start and end,
// at most limit of them, oldest first, and whether there were more than
// limit — then the oldest returned is where the answer starts being whole.
//
// The hosts are the whole tenancy boundary, as they are for the pod log
// (orchestrator.HTTPLogOptions): one controller logs every tenant's traffic,
// and the caller resolves which hosts are the app's from an owner-scoped
// read. They travel in the store's extra filter, with the controller's, so
// nothing else in the query can widen them. No hosts is no requests, never
// every tenant's.
func (l *LogStore) Requests(
	ctx context.Context, hosts []string, start, end time.Time, limit int,
) ([]orchestrator.HTTPLogLine, bool, error) {
	if l == nil {
		return nil, false, ErrNotConfigured
	}
	if len(hosts) == 0 {
		return nil, false, nil
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	want := make(map[string]bool, len(hosts))
	quoted := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(h)
		want[h] = true
		quoted = append(quoted, regexp.QuoteMeta(h))
	}
	// A host is matched whole and in any case, as the pod log reader does:
	// browsers send it lowercase, but nothing makes them.
	filter := ingressFilter + ` RequestHost:~` + strconv.Quote("(?i)^("+strings.Join(quoted, "|")+")$")

	body, err := post(ctx, l.client, l.base, "/select/logsql/query", url.Values{
		// One more than asked, to know whether there were more.
		"query":         {"* | sort by (_time desc) | limit " + strconv.Itoa(limit+1)},
		"extra_filters": {filter},
		"start":         {start.UTC().Format(time.RFC3339Nano)},
		"end":           {end.UTC().Format(time.RFC3339Nano)},
	})
	if err != nil {
		return nil, false, err
	}
	defer body.Close()

	var out []orchestrator.HTTPLogLine
	err = decodeRows(body, func(row map[string]string) bool {
		line, ok := accessRow(row)
		// The store's filter means a line for another host never comes back;
		// this is the check that it did not.
		if ok && want[strings.ToLower(line.Host)] {
			out = append(out, line)
		}
		return true
	})
	if err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	slices.SortStableFunc(out, func(a, b orchestrator.HTTPLogLine) int { return a.At.Compare(b.At) })
	if more {
		out = out[len(out)-limit:]
	}
	return out, more, nil
}

// accessRow reads one of Traefik's JSON access lines as the store keeps it:
// each of its fields a field of the row, which is what the node collector
// makes of a JSON line. One whose message is still the JSON — a collector
// that keeps lines whole — is read from the message instead.
func accessRow(row map[string]string) (orchestrator.HTTPLogLine, bool) {
	if row["RequestHost"] == "" {
		var whole map[string]any
		if err := json.Unmarshal([]byte(row["_msg"]), &whole); err != nil {
			return orchestrator.HTTPLogLine{}, false
		}
		row = map[string]string{"_time": row["_time"]}
		for k, v := range whole {
			switch v := v.(type) {
			case string:
				row[k] = v
			case float64:
				row[k] = strconv.FormatFloat(v, 'f', -1, 64)
			}
		}
	}
	if row["RequestHost"] == "" || row["RequestMethod"] == "" {
		return orchestrator.HTTPLogLine{}, false
	}
	status, _ := strconv.Atoi(row["DownstreamStatus"])
	size, _ := strconv.ParseFloat(row["DownstreamContentSize"], 64)
	ns, _ := strconv.ParseFloat(row["Duration"], 64)
	line := orchestrator.HTTPLogLine{
		Host: row["RequestHost"], Method: row["RequestMethod"], Path: row["RequestPath"],
		Protocol: row["RequestProtocol"], Status: status, Bytes: int64(size),
		Client: row["ClientHost"], Duration: time.Duration(ns),
	}
	// When the request began, as the pod log reader shows it; the line's own
	// time, when it was logged, for one without.
	if t, err := time.Parse(time.RFC3339Nano, row["StartUTC"]); err == nil {
		line.At = t
	} else if t, err := time.Parse(time.RFC3339Nano, row["_time"]); err == nil {
		line.At = t
	}
	return line, true
}
