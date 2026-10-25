package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// recorder is a store that answers with fixed rows and remembers the form it
// was sent, so a test can prove what was asked rather than only what came back.
type recorder struct {
	forms []url.Values
	body  string
}

func (rec *recorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		f := url.Values{}
		for k, v := range r.PostForm {
			f[k] = v
		}
		f.Set("path", r.URL.Path)
		rec.forms = append(rec.forms, f)
		io.WriteString(w, rec.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func teamScope(t *testing.T) Scope {
	t.Helper()
	s, err := NewScope("team-a", []Service{
		{Name: "web", Namespace: "yacht-aaaaaaaaaaaaaaaa"},
		{Name: "postgres", Namespace: "yacht-bbbbbbbbbbbbbbbb"},
	})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return s
}

// The scope travels as the store's extra filter on every read, so a query
// cannot reach past it whatever its own text turns out to say.
func TestEveryLogReadCarriesTheTeamsScopeAsAnExtraFilter(t *testing.T) {
	rec := &recorder{}
	ls, err := NewLogStore(rec.server(t).URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	q := LogQuery{Text: "timeout", Start: time.Now().Add(-time.Hour), End: time.Now()}
	if _, err := ls.Query(ctx, teamScope(t), q); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Histogram(ctx, teamScope(t), q); err != nil {
		t.Fatal(err)
	}
	if err := ls.Export(ctx, teamScope(t), q, io.Discard); err != nil {
		t.Fatal(err)
	}
	tail, err := ls.Tail(ctx, teamScope(t), q)
	if err != nil {
		t.Fatal(err)
	}
	for range tail {
	}

	// Sorted by service name: postgres before web.
	want := `{kubernetes.pod_namespace in ("yacht-bbbbbbbbbbbbbbbb","yacht-aaaaaaaaaaaaaaaa")} ` +
		`"kubernetes.pod_labels.yacht/owner-id":="team-a"`
	if len(rec.forms) != 4 {
		t.Fatalf("%d requests, want 4", len(rec.forms))
	}
	for _, f := range rec.forms {
		if got := f.Get("extra_filters"); got != want {
			t.Errorf("%s: extra_filters = %s\nwant %s", f.Get("path"), got, want)
		}
	}
}

// What a person types is a literal inside the message filter: LogsQL syntax
// in it is text to find, not a way out of the filter.
func TestSearchTextIsEscapedIntoTheMessageFilter(t *testing.T) {
	for _, text := range []string{
		`") or * | ("`,
		`a.b*c`,
		`kubernetes.pod_namespace:="yacht-zzzzzzzzzzzzzzzz"`,
		"line\nbreak",
	} {
		f := LogQuery{Text: text}.filter()
		if !strings.HasPrefix(f, `_msg:~"`) {
			t.Errorf("%q: filter %s is not a message filter", text, f)
			continue
		}
		// The quoted regexp is the whole filter: nothing follows it.
		if !strings.HasSuffix(f, `"`) || strings.Count(f, `_msg:~`) != 1 {
			t.Errorf("%q: filter %s carries more than the message filter", text, f)
		}
	}
	if f := (LogQuery{}).filter(); f != "*" {
		t.Errorf("an empty query is %q, want *", f)
	}
}

func TestAnEmptyScopeIsRefusedRatherThanReadAsEverything(t *testing.T) {
	rec := &recorder{}
	ls, _ := NewLogStore(rec.server(t).URL, nil)
	ms, _ := NewMetricStore(rec.server(t).URL, nil)
	empty, err := NewScope("team-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Query(context.Background(), empty, LogQuery{}); !errors.Is(err, ErrEmptyScope) {
		t.Errorf("query: %v, want ErrEmptyScope", err)
	}
	if _, err := ms.Dashboard(context.Background(), empty, WindowFor(time.Now(), time.Hour)); !errors.Is(err, ErrEmptyScope) {
		t.Errorf("dashboard: %v, want ErrEmptyScope", err)
	}
	if len(rec.forms) != 0 {
		t.Errorf("an empty scope reached the store %d times", len(rec.forms))
	}
}

func TestAScopeIsBuiltOnlyFromNamesThatAreSafeInAQuery(t *testing.T) {
	for _, ns := range []string{`yacht-a") or *`, "Yacht-A", "", `a|b`} {
		if _, err := NewScope("team-a", []Service{{Name: "web", Namespace: ns}}); err == nil {
			t.Errorf("namespace %q was accepted", ns)
		}
	}
	if _, err := NewScope(" ", []Service{{Name: "web", Namespace: "yacht-a"}}); err == nil {
		t.Error("a scope with no owner was accepted")
	}
}

// Narrowing picks among the scope's services. A name from outside it — a
// service in somebody else's team, typed into the filter — narrows to
// nothing, never to that service.
func TestNarrowingCanOnlyMakeAScopeSmaller(t *testing.T) {
	s := teamScope(t)
	if got := s.Narrow([]string{"web"}).Services(); len(got) != 1 || got[0].Name != "web" {
		t.Errorf("narrow to web = %v", got)
	}
	if got := s.Narrow([]string{"someone-elses-app"}); !got.Empty() {
		t.Errorf("narrowing to a stranger's app left %v", got.Services())
	}
	if got := s.Narrow(nil).Services(); len(got) != 2 {
		t.Errorf("no names narrowed to %v", got)
	}
}

// A row the store returns from outside the scope is dropped. The store's own
// filter means it cannot happen; this proves the second check exists.
func TestARowFromAnotherNamespaceIsNeverShown(t *testing.T) {
	rec := &recorder{body: `{"_time":"2026-10-25T10:00:00Z","_msg":"mine","kubernetes.pod_namespace":"yacht-aaaaaaaaaaaaaaaa"}
{"_time":"2026-10-25T10:00:01Z","_msg":"theirs","kubernetes.pod_namespace":"yacht-zzzzzzzzzzzzzzzz"}
`}
	ls, _ := NewLogStore(rec.server(t).URL, nil)
	got, err := ls.Query(context.Background(), teamScope(t), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Message != "mine" || got[0].Service != "web" {
		t.Errorf("got %+v, want only the team's own line, labelled web", got)
	}
}

func TestErrorLinesAreJudgedByTheirLevelFirst(t *testing.T) {
	for _, c := range []struct {
		level, severity, msg string
		want                 bool
	}{
		{"error", "", "anything", true},
		{"ERROR", "", "", true},
		{"50", "", "pino's error", true},
		{"info", "", "error budget is fine", false},
		{"", "critical", "", true},
		{"", "", "Traceback (most recent call last):", true},
		{"", "", "panic: runtime error", true},
		{"", "", "0 errors", false},
		{"", "", "GET /orders 200", false},
	} {
		if got := isErrorEntry(c.level, c.severity, c.msg); got != c.want {
			t.Errorf("level=%q severity=%q msg=%q: error=%v, want %v", c.level, c.severity, c.msg, got, c.want)
		}
	}
}

func TestTheFilterBoxReadsRailwaysFilters(t *testing.T) {
	s := ParseSearch("@service:Web  timeout @level:error  /orders @team:x")
	if s.Text != "timeout /orders @team:x" || !s.ErrorsOnly || len(s.Services) != 1 || s.Services[0] != "web" {
		t.Errorf("parsed %+v", s)
	}
}

// Every column of the range comes back, filled or not, so a gap in the log
// draws as a gap.
func TestTheHistogramHasAColumnForEveryStepOfTheRange(t *testing.T) {
	end := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	start := end.Add(-15 * time.Minute)
	rec := &recorder{body: `{"_time":"2026-10-25T11:50:00Z","total":"12","errors":"3"}` + "\n"}
	ls, _ := NewLogStore(rec.server(t).URL, nil)
	got, err := ls.Histogram(context.Background(), teamScope(t), LogQuery{Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	step := HistogramStep(15 * time.Minute)
	if want := int(15 * time.Minute / step); len(got) != want {
		t.Fatalf("%d columns, want %d", len(got), want)
	}
	var total, errs int64
	for _, b := range got {
		total += b.Total
		errs += b.Errors
	}
	if total != 12 || errs != 3 {
		t.Errorf("counted %d lines and %d errors, want 12 and 3", total, errs)
	}
	if !strings.Contains(rec.forms[0].Get("query"), "stats by (_time:"+promDuration(step)+")") {
		t.Errorf("query %s does not bucket by the step", rec.forms[0].Get("query"))
	}
}

// Metrics are confined the same way: every query names the scope's
// namespaces or its Traefik services, and nothing else.
func TestEveryMetricQueryIsConfinedToTheScope(t *testing.T) {
	rec := &recorder{body: `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"namespace":"yacht-aaaaaaaaaaaaaaaa"},"values":[[1792900000,"0.25"],[1792900030,"NaN"]]},
		{"metric":{"namespace":"yacht-zzzzzzzzzzzzzzzz"},"values":[[1792900000,"9"]]}
	]}}`}
	ms, _ := NewMetricStore(rec.server(t).URL, nil)
	panels, err := ms.Dashboard(context.Background(), teamScope(t), WindowFor(time.Now(), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rec.forms {
		q := f.Get("query")
		ns := strings.Contains(q, `namespace=~"yacht-bbbbbbbbbbbbbbbb|yacht-aaaaaaaaaaaaaaaa"`)
		svc := strings.Contains(q, `service=~"yacht-bbbbbbbbbbbbbbbb-postgres-80@kubernetes|yacht-aaaaaaaaaaaaaaaa-web-80@kubernetes"`)
		if !ns && !svc {
			t.Errorf("query is not confined to the scope: %s", q)
		}
	}
	cpu := panels[0]
	if cpu.Key != PanelCPU || len(cpu.Series) != 1 || cpu.Series[0].Name != "web" || len(cpu.Series[0].Points) != 1 {
		t.Errorf("cpu panel = %+v; want only web, with NaN dropped", cpu)
	}
}

func TestAStoreAddressMustBeAnHTTPURL(t *testing.T) {
	for _, bad := range []string{"victoria-logs:9428", "ftp://x", "http://", "http://x/?a=b"} {
		if _, err := NewLogStore(bad, nil); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if ls, err := NewLogStore("", nil); ls != nil || err != nil {
		t.Errorf("an empty address is no store and no error, got %v, %v", ls, err)
	}
}

// Against a real VictoriaLogs, when one is given: the scope is enforced by
// the store itself, not only assembled correctly here.
//
//	docker run -p 9428:9428 victoriametrics/victoria-logs:v1.53.0
//	YACHT_TEST_VICTORIALOGS_URL=http://127.0.0.1:9428 go test ./internal/observe
func TestAgainstVictoriaLogs(t *testing.T) {
	base := os.Getenv("YACHT_TEST_VICTORIALOGS_URL")
	if base == "" {
		t.Skip("set YACHT_TEST_VICTORIALOGS_URL to test against a running VictoriaLogs")
	}
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	now := time.Now().UTC()
	var body strings.Builder
	line := func(ns, owner, level, msg string) {
		fmt.Fprintf(&body, `{"_time":%q,"_msg":%q,"level":%q,"run":%q,`+
			`"kubernetes.pod_namespace":%q,"kubernetes.pod_name":"p","kubernetes.container_name":"c",`+
			`"kubernetes.pod_labels.yacht/owner-id":%q}`+"\n",
			now.Format(time.RFC3339Nano), msg, level, run, ns, owner)
	}
	line("yacht-aaaaaaaaaaaaaaaa", "team-a", "info", "ours one")
	line("yacht-aaaaaaaaaaaaaaaa", "team-a", "error", "ours two (failed)")
	line("yacht-zzzzzzzzzzzzzzzz", "team-b", "info", "theirs")
	// Our namespace with somebody else's owner label: shown to nobody here.
	line("yacht-aaaaaaaaaaaaaaaa", "team-b", "info", "relabelled")
	resp, err := http.Post(base+"/insert/jsonline?_stream_fields=kubernetes.pod_namespace,kubernetes.pod_name,kubernetes.container_name",
		"application/stream+json", strings.NewReader(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ls, _ := NewLogStore(base, nil)
	scope := teamScope(t)
	q := LogQuery{Start: now.Add(-time.Minute), End: now.Add(time.Minute)}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := ls.Query(context.Background(), scope, LogQuery{Text: "", Start: q.Start, End: q.End, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var mine []Entry
		for _, e := range got {
			if strings.HasPrefix(e.Message, "ours") || e.Message == "theirs" || e.Message == "relabelled" {
				mine = append(mine, e)
			}
		}
		if len(mine) >= 2 || time.Now().After(deadline) {
			for _, e := range mine {
				if !strings.HasPrefix(e.Message, "ours") {
					t.Errorf("read %q, which is not the team's", e.Message)
				}
			}
			if len(mine) != 2 {
				t.Fatalf("read %d of the team's lines, want 2", len(mine))
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	got, err := ls.Query(context.Background(), scope, LogQuery{Text: `(failed)`, ErrorsOnly: true, Start: q.Start, End: q.End})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[len(got)-1].Message != "ours two (failed)" || !got[len(got)-1].Error {
		t.Errorf("search for an error = %+v", got)
	}
}
