// Package events defines the geofence lifecycle event envelope published to
// Apache Pulsar for downstream consumers (e.g. the Co-Location Service),
// and a small Publisher abstraction so the rest of the service never
// imports the Pulsar SDK directly.
package events

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Topic is the Pulsar topic every geofence lifecycle event is published to,
// unless overridden by Settings.Topic.
const Topic = "persistent://synergy/geofence/events"

type EventType string

const (
	EventTypeCreated EventType = "CREATED"
	EventTypeUpdated EventType = "UPDATED"
	EventTypeDeleted EventType = "DELETED"
)

// Event is the common envelope for every geofence lifecycle event.
//
//	{
//	  "eventType": "CREATED | UPDATED | DELETED",
//	  "geofenceId": "uuid",
//	  "agencyId": "uuid",
//	  "userId": "uuid",
//	  "timestamp": "2026-06-16T12:00:00Z",
//	  "payload": { }
//	}
//
// Payload contents by event type:
//   - CREATED: full geofence snapshot (*handlermodel.Geofence)
//   - UPDATED: changed fields only (map[string]any, see handlermodel.DiffChangedFields)
//   - DELETED: none — the geofence id is already carried by the envelope
//
// userId extends the original schema (not present in the initial spec) so
// downstream consumers can build an actor-tracked audit trail (e.g. the
// Known Locations feature's "User Name" audit column) without a second
// round trip back to this service.
//
// AGENCY_UPDATED is not modeled: there is currently no operation that
// reassigns a geofence between agencies (agency_id is set once at creation
// from the URL path), so there is nothing to trigger it.
type Event struct {
	EventType  EventType `json:"eventType"`
	GeofenceID uuid.UUID `json:"geofenceId"`
	AgencyID   uuid.UUID `json:"agencyId"`
	UserID     uuid.UUID `json:"userId"`
	Timestamp  time.Time `json:"timestamp"`
	Payload    any       `json:"payload,omitempty"`
}

// Publisher publishes geofence lifecycle events. Publish is best-effort by
// design: the database is the system of record, and a broker outage must
// never fail the HTTP request that triggered the event — implementations
// log failures internally rather than returning an error the caller would
// have to decide what to do with.
type Publisher interface {
	Publish(ctx context.Context, event Event)
	Close()
}
