package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// PageSize is how many rows a list shows at once.
//
// Chosen to fill a screen and stop, rather than to be generous. The failure
// this exists to prevent is a page that renders every row a cluster can
// produce: the response gets large, the browser lays out thousands of nodes,
// and the one row somebody wanted is no easier to find than it was.
const PageSize = 50

// Page is one window onto a list, and what a template needs to draw the
// controls around it.
//
// Computed from the whole result set rather than from a database cursor,
// because both surfaces using this read from Kubernetes, which returns what it
// returns. Slicing here is honest about that: it makes the page small, and it
// does not pretend the read was.
type Page struct {
	// Query is the search somebody typed, echoed back so the box keeps it.
	Query string

	// Number is the 1-based page being shown, and Total is how many there are.
	Number int
	Total  int

	// Matched is how many rows the search found, before paging.
	Matched int

	// From and To are the 1-based bounds of this page, for "51–100 of 412".
	From int
	To   int

	// Base is the path the controls link to, without a query string.
	Base string

	// Extra carries any other query parameters the surface needs kept across
	// paging — a log view, a pod name — so a page link does not silently
	// change what is being looked at.
	Extra url.Values
}

// HasPrev and HasNext gate the controls, so a page never offers a link to
// somewhere that does not exist.
func (p Page) HasPrev() bool { return p.Number > 1 }
func (p Page) HasNext() bool { return p.Number < p.Total }

// Empty reports whether there is nothing to show at all.
func (p Page) Empty() bool { return p.Matched == 0 }

// Filtered reports whether a search narrowed the list, which is the difference
// between "nothing here" and "nothing matched".
func (p Page) Filtered() bool { return p.Query != "" }

// Href builds the link to another page, keeping the search and anything else
// the surface asked to carry.
func (p Page) Href(number int) string {
	q := url.Values{}
	for k, vs := range p.Extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	if p.Query != "" {
		q.Set("q", p.Query)
	}
	if number > 1 {
		q.Set("page", strconv.Itoa(number))
	}
	if len(q) == 0 {
		return p.Base
	}
	return p.Base + "?" + q.Encode()
}

func (p Page) PrevHref() string { return p.Href(p.Number - 1) }
func (p Page) NextHref() string { return p.Href(p.Number + 1) }

// pageRequest reads the search and page number from a request.
//
// A page beyond the end is clamped rather than refused. Rows move — a list of
// live events reorders between one request and the next — so an out-of-range
// page is an ordinary consequence of time passing, not something to show an
// error for.
func pageRequest(r *http.Request) (query string, number int) {
	query = strings.TrimSpace(r.URL.Query().Get("q"))
	number, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || number < 1 {
		number = 1
	}
	return query, number
}

// paginate slices one page out of a filtered list and describes it.
//
// Generic over the row type because the two callers share nothing else: an
// ingress request and a Kubernetes event have no common shape, only a common
// need to be searched and cut into pages.
func paginate[T any](
	rows []T, query string, number int, base string, extra url.Values,
	matches func(T, string) bool,
) ([]T, Page) {
	if query != "" {
		needle := strings.ToLower(query)
		kept := rows[:0:0]
		for _, row := range rows {
			if matches(row, needle) {
				kept = append(kept, row)
			}
		}
		rows = kept
	}

	p := Page{
		Query: query, Number: number, Matched: len(rows),
		Base: base, Extra: extra,
	}
	p.Total = (len(rows) + PageSize - 1) / PageSize
	if p.Total < 1 {
		p.Total = 1
	}
	if p.Number > p.Total {
		p.Number = p.Total
	}

	start := (p.Number - 1) * PageSize
	if start > len(rows) {
		start = len(rows)
	}
	end := min(start+PageSize, len(rows))

	p.From, p.To = start+1, end
	if len(rows) == 0 {
		p.From = 0
	}
	return rows[start:end], p
}
