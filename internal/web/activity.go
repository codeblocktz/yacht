package web

import (
	"strconv"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
)

// The deploy activity chart is drawn from ordinary elements rather than a
// client-side chart library.
//
// The dashboard renders every visual state to HTML from a gallery test, with no
// browser and no cluster, because those are the states that rot unseen. A chart
// mounted by JavaScript is an empty box there — so the states worth checking,
// like a month with no deploys or a single day that dwarfs the rest, would be
// the ones nothing checks. Thirty stacked columns do not need a library.

// deployPlotHeight is the height of the plot area in pixels.
//
// Fixed rather than proportional so a row of columns does not change height
// between an install with one deploy and one with a thousand.
const deployPlotHeight = 96

// deployColumnStyle sizes one day's column against the busiest day.
//
// Scaled to the maximum rather than to a fixed ceiling: an install that deploys
// twice a week and one that deploys forty times a day both want to see shape,
// and a fixed ceiling gives one of them a flat line.
func deployColumnStyle(d app.DeployDay, max int) string {
	if max <= 0 || d.Total() == 0 {
		return "height:0"
	}
	// A floor, so a day with one deploy against a max of two hundred is still
	// visible. A column that rounds to nothing reads as a day with no deploys,
	// which is a different fact.
	pct := float64(d.Total()) / float64(max) * 100
	if pct < 2 {
		pct = 2
	}
	return "height:" + strconv.FormatFloat(pct, 'f', 2, 64) + "%"
}

// deploySegmentStyle sizes one outcome's share within a day's column.
func deploySegmentStyle(count, total int) string {
	if total <= 0 || count <= 0 {
		return "display:none"
	}
	pct := float64(count) / float64(total) * 100
	return "height:" + strconv.FormatFloat(pct, 'f', 2, 64) + "%"
}

// deployMax is the busiest day in the window, and the scale everything else is
// drawn against.
func deployMax(a app.DeployActivity) int {
	max := 0
	for _, d := range a.Days {
		if t := d.Total(); t > max {
			max = t
		}
	}
	return max
}

// deployDayTitle is the hover text for one column.
//
// The chart carries no per-column labels — thirty of them would be unreadable —
// so this is where the exact numbers live.
func deployDayTitle(d app.DeployDay) string {
	day := d.Day.Format("Mon 2 Jan")
	if d.Total() == 0 {
		return day + " — no deploys"
	}

	out := day + " — " + plural(d.Total(), "deploy")
	for _, part := range []struct {
		n     int
		label string
	}{
		{d.Succeeded, "succeeded"},
		{d.Failed, "failed"},
		{d.Cancelled, "cancelled"},
		{d.Running, "in flight"},
	} {
		if part.n > 0 {
			out += ", " + strconv.Itoa(part.n) + " " + part.label
		}
	}
	return out
}

// deployAxisLabel formats the two dates under the chart.
func deployAxisLabel(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2 Jan")
}

// deployWindowLabel names the span the chart covers.
func deployWindowLabel(a app.DeployActivity) string {
	return plural(len(a.Days), "day")
}

// deployChartLabel describes the chart for a screen reader.
//
// The columns carry their detail in title attributes, which assistive
// technology does not reliably announce; without this the chart is an
// unlabelled box. One sentence with the totals is what a sighted reader takes
// from the shape.
func deployChartLabel(a app.DeployActivity) string {
	if a.Total() == 0 {
		return "Deploy activity: no deploys in the last " + deployWindowLabel(a)
	}
	out := "Deploy activity over " + deployWindowLabel(a) + ": " +
		plural(a.Total(), "deploy") + " in total"
	if a.Failed > 0 {
		out += ", " + strconv.Itoa(a.Failed) + " failed"
	}
	return out
}

// deployWindowDays is how far back the overview chart looks.
//
// Thirty days is long enough to show a rhythm and short enough that one column
// per day stays readable at dashboard width.
const deployWindowDays = 30
