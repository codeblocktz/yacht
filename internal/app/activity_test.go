package app

import (
	"context"
	"testing"
)

// Every day in the window is present, including the ones nobody deployed on.
//
// Postgres returns only days that have rows. Drawing those directly would put a
// handful of columns across a month and read as steady daily deploys — the axis
// would describe the data's shape rather than the calendar's.
func TestDeployActivityFillsDaysWithNoDeploys(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-activity-fill")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.DeployActivity(ctx, ownerID, 30)
	if err != nil {
		t.Fatalf("DeployActivity: %v", err)
	}

	if len(got.Days) != 30 {
		t.Errorf("got %d days, want 30 — empty days must still be columns", len(got.Days))
	}
	for i := 1; i < len(got.Days); i++ {
		want := got.Days[i-1].Day.AddDate(0, 0, 1)
		if !got.Days[i].Day.Equal(want) {
			t.Fatalf("day %d is %v, want %v — the axis has a gap",
				i, got.Days[i].Day, want)
		}
	}
}

// One team's deploys must never appear in another's chart.
func TestDeployActivityIsScopedToItsOwner(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	mine := owner(t, s, pool, "owner-activity-mine")
	theirs := owner(t, s, pool, "owner-activity-theirs")

	for _, o := range []string{mine, theirs} {
		if _, err := s.Create(ctx, o, CreateInput{
			Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
		}); err != nil {
			t.Fatalf("Create for %s: %v", o, err)
		}
	}

	got, err := s.DeployActivity(ctx, mine, 30)
	if err != nil {
		t.Fatalf("DeployActivity: %v", err)
	}
	other, err := s.DeployActivity(ctx, theirs, 30)
	if err != nil {
		t.Fatalf("DeployActivity: %v", err)
	}

	// Each created one app, so each owns exactly one deploy. Seeing two means
	// the other team's history leaked into this chart.
	if got.Total() != 1 {
		t.Errorf("owner sees %d deploys, want 1 — another team's history leaked", got.Total())
	}
	if other.Total() != 1 {
		t.Errorf("other owner sees %d deploys, want 1", other.Total())
	}
}

// A superseded deployment worked and was replaced. Counting it as a failure
// would draw a history of failures on an install where nothing ever failed.
func TestDeployActivityCountsSupersededAsSucceeded(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-activity-superseded")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A redeploy supersedes the first deployment.
	if err := s.Redeploy(ctx, ownerID, "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}

	got, err := s.DeployActivity(ctx, ownerID, 30)
	if err != nil {
		t.Fatalf("DeployActivity: %v", err)
	}

	if got.Failed != 0 {
		t.Errorf("%d deploys counted as failed, want 0 — superseded is not a failure", got.Failed)
	}
	if got.Succeeded != got.Total() {
		t.Errorf("succeeded=%d of total=%d, want all", got.Succeeded, got.Total())
	}
}

// The window is a bound, not a suggestion.
func TestDeployActivityBoundsTheWindow(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-activity-window")

	for _, days := range []int{0, -5, 5000} {
		got, err := s.DeployActivity(ctx, ownerID, days)
		if err != nil {
			t.Fatalf("DeployActivity(%d): %v", days, err)
		}
		if len(got.Days) < 1 || len(got.Days) > maxActivityDays {
			t.Errorf("asked for %d days, got %d columns — outside 1..%d",
				days, len(got.Days), maxActivityDays)
		}
	}
}

// Totals are the sum of the columns, or the panel heading contradicts the chart
// underneath it.
func TestDeployActivityTotalsMatchTheColumns(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	ownerID := owner(t, s, pool, "owner-activity-totals")

	if _, err := s.Create(ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.DeployActivity(ctx, ownerID, 30)
	if err != nil {
		t.Fatalf("DeployActivity: %v", err)
	}

	summed := 0
	for _, d := range got.Days {
		summed += d.Total()
	}
	if summed != got.Total() {
		t.Errorf("columns sum to %d but the total says %d", summed, got.Total())
	}
	if got.Total() == 0 {
		t.Fatal("creating an app recorded no deployment, so this proves nothing")
	}
}
