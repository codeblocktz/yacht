package app

import (
	"context"
	"fmt"
	"time"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// DeployDay is one day's deploys, split by how they ended.
type DeployDay struct {
	Day time.Time

	Succeeded int
	Failed    int
	Cancelled int

	// Running counts deploys still in flight, which includes pending. They are
	// shown because a day whose deploys are all still running looks identical
	// to a day with no deploys otherwise, and those mean opposite things.
	Running int
}

// Total is every deploy started that day.
func (d DeployDay) Total() int {
	return d.Succeeded + d.Failed + d.Cancelled + d.Running
}

// DeployActivity is deploys per day over a window, for the overview chart.
type DeployActivity struct {
	// Days is one entry per day in the window, oldest first, including days
	// with no deploys.
	Days []DeployDay

	// Totals over the whole window, so the panel can say what it is showing
	// without the reader adding up bars.
	Succeeded int
	Failed    int
	Cancelled int
	Running   int
}

// Total is every deploy in the window.
func (a DeployActivity) Total() int {
	return a.Succeeded + a.Failed + a.Cancelled + a.Running
}

// maxActivityDays bounds the window a caller may ask for.
//
// The query is indexed and grouped, but the chart draws one column per day and
// a year of them is not a chart, it is a texture.
const maxActivityDays = 90

// DeployActivity counts an owner's deploys per day over the last n days.
//
// Every day in the window is returned, including the ones with no deploys.
// Postgres only reports days that have rows, and drawing those directly would
// put six columns across a month and read as steady daily deploys — the axis
// would be describing the data's shape rather than the calendar's.
func (s *Service) DeployActivity(ctx context.Context, ownerID string, days int) (DeployActivity, error) {
	if days <= 0 || days > maxActivityDays {
		days = 30
	}

	// Truncated to the day, in UTC, so the buckets line up with the ones
	// date_trunc produces. Comparing a truncated key against an untruncated
	// "now minus n days" loses the oldest day for everyone not on UTC midnight.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	since := today.AddDate(0, 0, -(days - 1))

	rows, err := dbgen.New(s.pool).DeployActivity(ctx, dbgen.DeployActivityParams{
		OwnerID:   ownerID,
		StartedAt: since,
	})
	if err != nil {
		return DeployActivity{}, fmt.Errorf("app: deploy activity: %w", err)
	}

	// Indexed by day so the zero-fill below is a lookup rather than a scan per
	// day over every row.
	byDay := make(map[time.Time]*DeployDay, days)
	out := DeployActivity{Days: make([]DeployDay, 0, days)}
	for d := since; !d.After(today); d = d.AddDate(0, 0, 1) {
		out.Days = append(out.Days, DeployDay{Day: d})
	}
	for i := range out.Days {
		byDay[out.Days[i].Day] = &out.Days[i]
	}

	for _, row := range rows {
		day, ok := byDay[row.Day.UTC().Truncate(24*time.Hour)]
		if !ok {
			// A row outside the window. Not reachable through the query's own
			// predicate, but a clock change between the two is not worth a
			// panic or a silently miscounted total.
			continue
		}
		n := int(row.Total)
		switch row.Status {
		// A superseded deployment worked and was later replaced, so it counts
		// as a success. Charting it as anything else would draw a history of
		// failures on an install where nothing ever failed — which is the
		// specific mistake the status vocabulary was designed to avoid.
		//
		// "succeeded" and "cancelled" are the older vocabulary. The constraint
		// still permits them and rows written before 00015 still carry them,
		// so they are counted rather than falling through to Running and
		// showing a month of deploys as permanently in flight.
		case DeployActive, DeploySuperseded, "succeeded":
			day.Succeeded += n
			out.Succeeded += n
		case DeployFailed:
			day.Failed += n
			out.Failed += n
		case "cancelled":
			day.Cancelled += n
			out.Cancelled += n
		default:
			// running and pending both mean "not finished yet".
			day.Running += n
			out.Running += n
		}
	}
	return out, nil
}
