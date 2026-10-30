package engine

import (
	"log/slog"

	"github.com/codeblocktz/yacht/internal/config"
	"github.com/codeblocktz/yacht/internal/observe"
	"github.com/codeblocktz/yacht/internal/web"
)

// attachObserve gives the dashboard the install's log and metrics stores,
// where it has them, and returns the metrics store for Egress: nil without
// one.
//
// Set only when configured: a nil *observe.LogStore in the interface would
// be a store that is there and fails every read, and the pages tell "no
// store" from "a store that failed" to say the right thing.
func attachObserve(opts *web.Options, cfg config.Config, log *slog.Logger) (*observe.MetricStore, error) {
	logs, err := observe.NewLogStore(cfg.LogsURL, nil)
	if err != nil {
		return nil, err
	}
	metrics, err := observe.NewMetricStore(cfg.MetricsURL, nil)
	if err != nil {
		return nil, err
	}
	if logs != nil {
		opts.LogHistory = logs
		opts.LogRetention = cfg.LogsRetention
		log.Info("log history is read from a log store", slog.String("retention", cfg.LogsRetention.String()))
	} else {
		log.Info("no log store — a project's Logs page shows each service's running pod; " +
			"set YACHT_LOGS_URL to keep history")
	}
	if metrics != nil {
		opts.MetricHistory = metrics
		log.Info("metrics are read from a metrics store")
	} else {
		log.Info("no metrics store — Observability shows metrics-server's current reading; " +
			"set YACHT_METRICS_URL for requests, latency and history")
	}
	return metrics, nil
}
