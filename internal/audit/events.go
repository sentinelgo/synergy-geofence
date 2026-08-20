package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	activitylogger "github.com/sentinelgo/synergy-common/pkg/activity-logger"
	pb "github.com/sentinelgo/synergy-common/pkg/activity-logger/proto"
	"github.com/sentinelgo/synergy-common/pkg/log"
	"github.com/sentinelgo/synergy-common/pkg/util"
)

// Actor identifies who performed a geofence mutation, for LogGeofenceCreated,
// LogGeofenceUpdated and LogGeofenceDeleted.
type Actor struct {
	UserID uuid.UUID
	IP     string
}

// Resource identifies the geofence a LogGeofenceCreated, LogGeofenceUpdated
// or LogGeofenceDeleted call acted on.
type Resource struct {
	ID       uuid.UUID
	Name     string
	AgencyID uuid.UUID
}

// ── schema mapping ──────────────────────────────────────────────────────
//
// synergy-common v1.28.x+ added generic, resource-agnostic
// EventCategory_CREATED/UPDATED/DELETED values (SYN-3606) specifically so
// plain create/update/delete audit events — like every one of geofence's —
// have a real category to set instead of EventCategory's zero value
// (LOGIN, a real, meaningful category — not an "unspecified" sentinel).
//
// ResourceType still has no GEOFENCE value, and the proto documents
// Metadata as the sanctioned place for attributes that don't have a schema
// field of their own, so Resource.Type stays RESOURCE_OTHER with the real
// kind stamped into Metadata["resource_kind"] until that's added upstream
// too (tracked separately from SYN-3606's Category work — see
// geofenceResource below).
//
// The six events below (create, the four update variants, delete) are the
// full geofence audit event catalog as specified for SYN-3606:
//
//	Action                                  Category         UI text
//	geofence.created                        CREATED          Geofence created
//	geofence.updated                        UPDATED          Geofence details updated
//	geofence.geometry.updated               UPDATED          Geofence boundary redrawn
//	geofence.status.changed                 SETTINGS_CHANGED Geofence status changed
//	geofence.deleted                        DELETED          Geofence deleted
//	geofence.colocation_exclusion.changed   SETTINGS_CHANGED Co-location exclusion changed
const (
	resourceKindGeofence = "geofence"

	actionCreated                    = "geofence.created"
	actionUpdated                    = "geofence.updated"
	actionGeometryUpdated            = "geofence.geometry.updated"
	actionStatusChanged              = "geofence.status.changed"
	actionDeleted                    = "geofence.deleted"
	actionColocationExclusionChanged = "geofence.colocation_exclusion.changed"

	messageCreated                    = "Geofence created"
	messageUpdated                    = "Geofence details updated"
	messageGeometryUpdated            = "Geofence boundary redrawn"
	messageStatusChanged              = "Geofence status changed"
	messageDeleted                    = "Geofence deleted"
	messageColocationExclusionChanged = "Co-location exclusion changed"
)

// LogGeofenceCreated records a geofence-created audit event. details, if
// non-nil, is marshaled to JSON and attached as the request's Details (e.g.
// the created geofence's response payload) — pass the same value you're
// already publishing on events.EventTypeCreated.
func LogGeofenceCreated(ctx context.Context, l activitylogger.Logger, actor Actor, resource Resource, details any) {
	log1(ctx, l, actor, resource, actionCreated, pb.EventCategory_CREATED, messageCreated, details)
}

// LogGeofenceUpdated records one or more geofence-updated audit events from
// a single PATCH, driven entirely by which fields diff reports changed —
// it never needs to know what the caller's request body looked like. diff
// is the same changed-fields map you're already publishing on
// events.EventTypeUpdated (see hmodel.DiffChangedFields); each emitted
// event's Details carries the whole diff, not just the field it's named
// for, so a reviewer sees everything that changed in that PATCH regardless
// of which line they're reading.
//
// A single PATCH touching e.g. both geometry and status emits two distinct
// events (one per aspect) rather than one ambiguous "updated" event,
// because Action/Category are singular per event. Any diff key this
// function doesn't recognize (i.e. anything other than geo_json/type,
// status, or exclude_from_colocation) falls into the generic
// geofence.updated bucket rather than being silently dropped — so a future
// field added to DiffChangedFields is still audited, just not with its own
// specific action, until this function is taught about it.
func LogGeofenceUpdated(ctx context.Context, l activitylogger.Logger, actor Actor, resource Resource, diff map[string]any) {
	_, geometryChanged := diff["geo_json"]
	_, statusChanged := diff["status"]
	_, colocationChanged := diff["exclude_from_colocation"]

	detailFieldsChanged := false
	for field := range diff {
		switch field {
		case "geo_json", "type", "status", "exclude_from_colocation":
			// has its own specific event below, not the generic bucket
		default:
			detailFieldsChanged = true
		}
	}

	if detailFieldsChanged {
		log1(ctx, l, actor, resource, actionUpdated, pb.EventCategory_UPDATED, messageUpdated, diff)
	}
	if geometryChanged {
		log1(ctx, l, actor, resource, actionGeometryUpdated, pb.EventCategory_UPDATED, messageGeometryUpdated, diff)
	}
	if statusChanged {
		log1(ctx, l, actor, resource, actionStatusChanged, pb.EventCategory_SETTINGS_CHANGED, messageStatusChanged, diff)
	}
	if colocationChanged {
		log1(ctx, l, actor, resource, actionColocationExclusionChanged, pb.EventCategory_SETTINGS_CHANGED, messageColocationExclusionChanged, diff)
	}
}

// LogGeofenceDeleted records a geofence-deleted audit event.
func LogGeofenceDeleted(ctx context.Context, l activitylogger.Logger, actor Actor, resource Resource) {
	log1(ctx, l, actor, resource, actionDeleted, pb.EventCategory_DELETED, messageDeleted, nil)
}

// userActor builds the Actor for a geofence mutation. There is no
// human-readable display name available at any of the three call sites
// (only a user id), which matches how synergy-sidecar itself builds
// actors/resources it has no display name for — see e.g. its
// activitylogger.NewResource(pb.ResourceType_RESOURCE_USER, subject, "").
func userActor(actor Actor, agencyID uuid.UUID) *pb.Actor {
	a := activitylogger.UserActor(actor.UserID.String(), "", actor.IP)
	return activitylogger.ActorWithAgencyID(a, agencyID.String())
}

// geofenceResource builds the Resource. Type is RESOURCE_OTHER — see this
// file's package-level doc comment above for why, and what's still
// outstanding upstream.
func geofenceResource(r Resource) *pb.Resource {
	return activitylogger.NewResourceWithParent(
		pb.ResourceType_RESOURCE_OTHER, r.ID.String(), r.Name,
		pb.ResourceType_AGENCY, r.AgencyID.String(),
	)
}

// log1 builds and enqueues a single ActivityLogRequest. l is nil-guarded,
// matching activitylogger.Logger's documented contract elsewhere in this
// org (see synergy-sidecar's stampAndEnqueue) — every geofence call site is
// expected to pass a real logger from NewLogger, but tests/partial
// construction should not panic.
func log1(ctx context.Context, l activitylogger.Logger, actor Actor, resource Resource, action string, category pb.EventCategory, message string, details any) {
	if l == nil {
		l = activitylogger.NoOp()
	}

	req := &pb.ActivityLogRequest{
		Type:     pb.EventType_ACTIVITY,
		Category: category,
		Action:   action,
		Status:   pb.ActionStatus_SUCCESS,
		Actor:    userActor(actor, resource.AgencyID),
		Resource: geofenceResource(resource),
		Message:  message,
		Metadata: map[string]string{"resource_kind": resourceKindGeofence},
	}

	if details != nil {
		if b, err := json.Marshal(details); err != nil {
			log.LoggerFromContext(ctx).With("error", err, "action", action).
				Warn("failed to marshal audit event details — event logged without them")
		} else {
			req.Details = util.Ptr(string(b))
		}
	}

	if corrID := log.CorrelationIdFromContext(ctx); corrID != "" {
		req = activitylogger.WithCorrelationID(req, corrID)
	}

	l.Log(ctx, req)
}
