package config

import (
	"errors"
	"time"

	"github.com/codeblocktz/yacht/internal/observe"
)

// observeFaults checks the log and metrics stores' settings. Each address is
// parsed the way the engine will dial it, so one that could never be read is
// refused at startup rather than by the first page that tries.
func (c Config) observeFaults() []error {
	var errs []error
	if _, err := observe.NewLogStore(c.LogsURL, nil); err != nil {
		errs = append(errs, err)
	}
	if _, err := observe.NewMetricStore(c.MetricsURL, nil); err != nil {
		errs = append(errs, err)
	}
	if c.LogsRetention < time.Hour || c.LogsRetention > 366*24*time.Hour {
		errs = append(errs, errors.New("YACHT_LOGS_RETENTION must be between 1h and 8784h (a year), such as 168h"))
	}
	return errs
}
