package orchestrator

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// ValidateSchedule reports whether s is a schedule Kubernetes will run.
//
// Parsed with robfig/cron's standard parser, which is the one the Kubernetes
// API server validates a CronJob's schedule with, so an expression this
// accepts is one the cluster accepts. A time zone in the expression is
// refused, as Kubernetes refuses it: schedules here are UTC, and saying so in
// one place is better than a schedule that means a different hour in each.
func ValidateSchedule(s string) error {
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "TZ=") || strings.HasPrefix(s, "CRON_TZ=") {
		return errors.New("schedule: a time zone cannot be set in the schedule — schedules run in UTC")
	}
	if _, err := cron.ParseStandard(s); err != nil {
		return fmt.Errorf("schedule %q is not a cron expression: %w", s, err)
	}
	return nil
}

// NextRun is when a schedule next runs after t, and false for no schedule or
// one that does not parse.
func NextRun(schedule string, after time.Time) (time.Time, bool) {
	if schedule == "" {
		return time.Time{}, false
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return time.Time{}, false
	}
	next := sched.Next(after.UTC())
	return next, !next.IsZero()
}
