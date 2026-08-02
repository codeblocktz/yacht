package web

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func rows(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "row-" + strconv.Itoa(i)
	}
	return out
}

func contains(row, needle string) bool {
	return strings.Contains(strings.ToLower(row), needle)
}

// A page is a window, and the numbers under it have to describe that window.
//
// "51–100 of 412" is the one thing on the control that somebody reads to know
// where they are; an off-by-one here is a page that lies about itself.
func TestAPageDescribesItself(t *testing.T) {
	for _, tc := range []struct {
		total, want         int
		number              int
		from, to, pageTotal int
	}{
		{total: 0, number: 1, from: 0, to: 0, pageTotal: 1, want: 0},
		{total: 10, number: 1, from: 1, to: 10, pageTotal: 1, want: 10},
		{total: 50, number: 1, from: 1, to: 50, pageTotal: 1, want: 50},
		{total: 51, number: 1, from: 1, to: 50, pageTotal: 2, want: 50},
		{total: 51, number: 2, from: 51, to: 51, pageTotal: 2, want: 1},
		{total: 412, number: 2, from: 51, to: 100, pageTotal: 9, want: 50},
	} {
		got, p := paginate(rows(tc.total), "", tc.number, "/x", nil, contains)

		if len(got) != tc.want {
			t.Errorf("%d rows page %d: got %d rows, want %d",
				tc.total, tc.number, len(got), tc.want)
		}
		if p.From != tc.from || p.To != tc.to {
			t.Errorf("%d rows page %d: showing %d-%d, want %d-%d",
				tc.total, tc.number, p.From, p.To, tc.from, tc.to)
		}
		if p.Total != tc.pageTotal {
			t.Errorf("%d rows: %d pages, want %d", tc.total, p.Total, tc.pageTotal)
		}
	}
}

// A page past the end lands on the last one rather than on nothing.
//
// Rows move: a list of live events reorders between one request and the next,
// so somebody on page 9 of 9 who reloads after events expire is asking for a
// page that no longer exists. That is time passing, not an error.
func TestAPagePastTheEndIsClamped(t *testing.T) {
	got, p := paginate(rows(10), "", 99, "/x", nil, contains)

	if len(got) == 0 {
		t.Error("a page past the end showed nothing at all")
	}
	if p.Number != 1 {
		t.Errorf("page %d of %d — not clamped", p.Number, p.Total)
	}
	if p.HasNext() {
		t.Error("the last page offers a next page")
	}
}

// Searching narrows before paging, not after.
//
// The other order pages the whole list and then filters the page, which shows
// three matches on page one and claims there are no more — the bug that makes
// somebody trust a search that is lying.
func TestSearchNarrowsBeforePaging(t *testing.T) {
	// 120 rows, of which 12 contain "row-1" followed by nothing else... use a
	// needle that matches a known count.
	all := rows(120)
	got, p := paginate(all, "row-11", 1, "/x", nil, contains)

	// row-11, row-110..row-119 — eleven of them.
	if p.Matched != 11 {
		t.Fatalf("matched %d, want 11", p.Matched)
	}
	if len(got) != 11 {
		t.Errorf("page holds %d, want all 11 on one page", len(got))
	}
	if p.Total != 1 {
		t.Errorf("%d pages for 11 matches", p.Total)
	}
}

// Paging keeps the search, and everything else the surface carries.
//
// A next-page link that dropped the view would move somebody from the HTTP log
// to the deploy log without saying so; one that dropped the search would show
// them page two of a list they were not looking at.
func TestPagingKeepsTheSearchAndTheView(t *testing.T) {
	_, p := paginate(rows(200), "row-1", 2, "/apps/web/logs",
		url.Values{"view": {"http"}}, contains)

	next := p.NextHref()
	for _, want := range []string{"q=row-1", "view=http", "page=3"} {
		if !strings.Contains(next, want) {
			t.Errorf("next link %q lost %q", next, want)
		}
	}

	// And page one is spelled without a page parameter, so the first page has
	// one URL rather than two that differ invisibly.
	if strings.Contains(p.Href(1), "page=") {
		t.Errorf("page one carries a page parameter: %s", p.Href(1))
	}
}

// The request reader tolerates whatever arrives in the query string.
func TestAPageNumberFromTheQueryIsNotTrusted(t *testing.T) {
	for _, raw := range []string{"", "0", "-3", "abc", "9999999999999999999999"} {
		r := httptest.NewRequest("GET", "/x?page="+url.QueryEscape(raw), nil)
		_, number := pageRequest(r)
		if number < 1 {
			t.Errorf("page=%q produced %d", raw, number)
		}
	}

	r := httptest.NewRequest("GET", "/x?q=%20%20spaced%20%20&page=3", nil)
	q, number := pageRequest(r)
	if q != "spaced" {
		t.Errorf("query = %q, want trimmed", q)
	}
	if number != 3 {
		t.Errorf("page = %d, want 3", number)
	}
}

// An empty list and an empty search look identical and mean opposite things.
func TestAnEmptySearchIsDistinguishedFromAnEmptyList(t *testing.T) {
	_, quiet := paginate(rows(0), "", 1, "/x", nil, contains)
	if quiet.Filtered() {
		t.Error("an unfiltered empty list reports itself as a search")
	}

	_, missed := paginate(rows(10), "nothing-matches-this", 1, "/x", nil, contains)
	if !missed.Filtered() || !missed.Empty() {
		t.Errorf("a search that found nothing: filtered=%v empty=%v",
			missed.Filtered(), missed.Empty())
	}
}

// Filtering must not scribble on the caller's slice.
//
// The filter reuses the input's backing array to avoid an allocation, which is
// only safe because it allocates a fresh one. Getting that wrong would corrupt
// the very list the chart above is computed from.
func TestFilteringDoesNotDisturbTheOriginal(t *testing.T) {
	all := rows(20)
	before := append([]string(nil), all...)

	paginate(all, "row-1", 1, "/x", nil, contains)

	for i := range all {
		if all[i] != before[i] {
			t.Fatalf("row %d changed from %q to %q", i, before[i], all[i])
		}
	}
}

// Highlighting cuts a line into spans rather than building markup.
//
// The template escapes each span, which is the whole point: a log line is
// arbitrary output from somebody's container, and a version that built a
// string with <mark> in it and trusted it would turn a crash message into
// script running in the dashboard.
func TestHighlightSplitsWithoutBuildingMarkup(t *testing.T) {
	spans := splitMatches("connect failed: CONNECT refused", "connect")
	if len(spans) != 4 {
		t.Fatalf("got %d spans, want 4: %+v", len(spans), spans)
	}
	// Case-insensitive, but each span keeps the log's own casing — showing
	// "connect" where the log said "CONNECT" would be editing the output.
	if spans[0].Text != "connect" || !spans[0].Match {
		t.Errorf("first span = %+v", spans[0])
	}
	if spans[2].Text != "CONNECT" || !spans[2].Match {
		t.Errorf("third span = %+v, want the log's own casing", spans[2])
	}

	// Rejoining the spans gives the line back, unchanged.
	var joined string
	for _, s := range spans {
		joined += s.Text
	}
	if joined != "connect failed: CONNECT refused" {
		t.Errorf("rejoined to %q", joined)
	}
}

// An empty search leaves the line whole rather than exploding it.
func TestHighlightWithNoSearchIsOneSpan(t *testing.T) {
	spans := splitMatches("a line", "")
	if len(spans) != 1 || spans[0].Match {
		t.Errorf("got %+v", spans)
	}
}

// The method selector offers only verbs the data actually contains.
//
// A fixed list would offer PATCH on an app that has never seen one, and a
// filter that can only return nothing is worse than not offering it.
func TestTheMethodListComesFromTheData(t *testing.T) {
	got := methodsIn([]orchestrator.HTTPLogLine{
		{Method: "GET"}, {Method: "POST"}, {Method: "GET"}, {Method: ""},
	})
	if len(got) != 2 || got[0] != "GET" || got[1] != "POST" {
		t.Errorf("methods = %v, want [GET POST]", got)
	}
}
