// Package audit records geofence lifecycle mutations (create/update/delete)
// as audit events on synergy-common's activity-logger client, so agency
// admins have a "who changed this geofence, and how" trail independent of
// the Pulsar lifecycle events in package events (which exist for downstream
// consumers like the Co-Location Service, not for audit/compliance review).
//
// synergy-common's activity-logger proto (pkg/activity-logger/proto) has
// dedicated AGENCY_KNOWN_LOCATION_ADDED/DELETED/MODIFIED/INCLUDED/EXCLUDED
// EventCategory values (feat/audit-event-category-updated, post-v1.29.0)
// for exactly this resource, but still no ResourceType_GEOFENCE. This
// package integrates against the client as it exists today with a
// documented placeholder mapping (see events.go) for that remaining gap,
// confined to one file so picking up the real schema later is a small,
// isolated diff rather than a rewire of every call site.
package audit

import "github.com/spf13/viper"

// Config configures the activity-logger client this package wraps. It is
// decoded directly from common.activity-log via viper — mirroring
// geofenceAuthorizationConfig's standalone-decode pattern in
// internal/handler/auth.go — rather than extending synergy-common's shared
// config.Config, so adding it needs no upstream change either.
type Config struct {
	// Disabled turns off audit logging entirely (operator choice). Unlike an
	// unset Address, this is not a misconfiguration, so NewLogger logs it at
	// Info rather than Warn.
	Disabled bool `mapstructure:"disabled"`
	// Address is the activity-logger/happening service's gRPC address. Left
	// empty (the default in every environment until this is wired up),
	// NewLogger falls back to a no-op logger and warns on every boot.
	Address string `mapstructure:"address"`
	// Workers is the number of background goroutines draining the send
	// queue. Zero uses activitylogger's own default.
	Workers int `mapstructure:"workers"`
	// QueueSize bounds the in-memory event buffer; events are dropped (with
	// a warning) once it's full. Zero uses activitylogger's own default.
	QueueSize int `mapstructure:"queue-size"`
	// Retry opts into gRPC retry on transient failures. Off by default: Log
	// is a non-idempotent append, so retrying can duplicate audit entries —
	// see activitylogger.RetryPolicy's doc comment before enabling this.
	Retry bool `mapstructure:"retry"`
}

// ConfigFromViper decodes Config from the common.activity-log key.
func ConfigFromViper() (Config, error) {
	var cfg Config
	err := viper.UnmarshalKey("common.activity-log", &cfg)
	return cfg, err
}
