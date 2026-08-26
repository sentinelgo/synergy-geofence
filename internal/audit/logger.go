package audit

import (
	activitylogger "github.com/sentinelgo/synergy-common/pkg/activity-logger"
	"github.com/sentinelgo/synergy-common/pkg/log"
)

// serviceName identifies this service on every emitted ActivityLogRequest
// (see activitylogger.WithServiceName), matching the "synergy-<name>"
// convention other services (e.g. synergy-sidecar) use.
const serviceName = "synergy-geofence"

// NewLogger builds the activity-logger client geofence audit events are
// recorded through. It mirrors synergy-sidecar's newActivityLogger:
// deliberately silent-by-default. An unconfigured Address (the zero value,
// and today's default in every environment — this integration is new) and
// an explicitly Disabled config both fall back to a no-op logger rather
// than failing startup, so environments that haven't wired up the
// happening/activity-log service yet keep running, just without a geofence
// audit trail. The unset-Address fallback is logged at Warn on every boot
// specifically so it doesn't go unnoticed; Disabled is logged at Info since
// it's a deliberate operator choice, not a misconfiguration.
func NewLogger(cfg Config, l log.Interface) activitylogger.Logger {
	if cfg.Disabled {
		l.Info("activity-log explicitly disabled via common.activity-log.disabled — geofence audit events will be discarded")
		return activitylogger.NoOp()
	}
	if cfg.Address == "" {
		l.Warn("activity-log address not configured — geofence audit events will be discarded until common.activity-log.address is set")
		return activitylogger.NoOp()
	}

	opts := []activitylogger.Option{
		activitylogger.WithAddress(cfg.Address),
		activitylogger.WithServiceName(serviceName),
	}
	if cfg.Workers > 0 {
		opts = append(opts, activitylogger.WithWorkers(cfg.Workers))
	}
	if cfg.QueueSize > 0 {
		opts = append(opts, activitylogger.WithQueueSize(cfg.QueueSize))
	}
	if cfg.Retry {
		opts = append(opts, activitylogger.WithRetry())
	}

	al, err := activitylogger.New(opts...)
	if err != nil {
		l.With("error", err).Warn("activity logger init failed — geofence audit events will be discarded")
		return activitylogger.NoOp()
	}
	return al
}
