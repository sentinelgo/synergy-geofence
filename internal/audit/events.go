package audit

import (
	"context"
	"encoding/json"
	"fmt"

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
	// Role is the caller's resolved role for this request — one of the
	// api.*Admin template constants (e.g. api.AgencyAdmin) — stamped into
	// the event's Metadata["role"] by log1. Left "" (the metadata key is
	// then omitted entirely, not set-but-empty) when no role could be
	// resolved, e.g. JWT verification is disabled. Every geofence mutation
	// route is gated by requireAgencyAdmin, which already resolves this
	// synchronously from JWT claims before the handler runs — see
	// pkg.ResolvedRole — so unlike synergy-sidecar's RoleNameForAgency, no
	// extra RPC or background goroutine is needed to populate it.
	Role string
}

// Resource identifies the geofence a LogGeofenceCreated, LogGeofenceUpdated
// or LogGeofenceDeleted call acted on. Name is required as of the Known
// Location message rework below — every UI-facing message now interpolates
// it (e.g. "Known Location <Name> Added") — so every call site must load
// the row (or otherwise know its name) before logging, unlike before, when
// an empty Name was an accepted gap for DeleteGeofence.
type Resource struct {
	ID       uuid.UUID
	Name     string
	AgencyID uuid.UUID
}

// FieldChange describes one mutable business field's before/after value
// for a single PATCH, pre-formatted for display in the "Known Location
// <Name> Modified <Field> from <Old> to <New>" message below. This package
// has no business knowledge of geofence fields (status codes, client ids,
// GeoJSON, etc.), so the caller (controller.go's UpdateGeofence, using the
// same original/updated entities it already diffs via
// hmodel.DiffChangedFields) is responsible for turning each changed field
// into a human-readable Field label and Old/New strings before handing it
// to LogGeofenceUpdated.
type FieldChange struct {
	Field string
	Old   string
	New   string
}

// ── schema mapping ──────────────────────────────────────────────────────
//
// synergy-common's feat/audit-event-category-updated (post-v1.29.0)
// replaced the generic, resource-agnostic EventCategory_CREATED/UPDATED/
// DELETED values (SYN-3606) this package originally used with a dedicated
// "Known Location" category per event shape —
// AGENCY_KNOWN_LOCATION_ADDED/DELETED/MODIFIED/INCLUDED/EXCLUDED — since
// geofence *is* the Known Location feature's backing resource. This is a
// breaking renumbering (CREATED/UPDATED/DELETED's old slots 10-12 are
// reused for unrelated categories), not an addition, so there is no
// version of synergy-common where both old and new values are valid —
// bumping past v1.29.0 requires this rewrite.
//
// ResourceType still has no GEOFENCE value, and the proto documents
// Metadata as the sanctioned place for attributes that don't have a schema
// field of their own, so Resource.Type stays RESOURCE_OTHER with the real
// kind stamped into Metadata["resource_kind"] until that's added upstream
// too (tracked separately from the Category work — see geofenceResource
// below).
//
// The six events below (create, the four update variants, delete) are the
// full geofence audit event catalog, with UI text per the Known Location
// requirement: every message leads with "Agency Configuration: Known
// Location <Name>" and the four update variants collapse into two shapes —
// "Modified <Field> from <Old> to <New>" (one event per changed field,
// Category MODIFIED regardless of which field) and "Excluded from"/
// "Included for Co-Location Report." (the exclusion flag flips between the
// two, rather than reporting a before/after value, and carries its own
// dedicated EXCLUDED/INCLUDED Category instead of MODIFIED).
//
//	Action                                  Category                       UI text
//	geofence.created                        AGENCY_KNOWN_LOCATION_ADDED    Known Location <Name> Added
//	geofence.updated                        AGENCY_KNOWN_LOCATION_MODIFIED Known Location <Name> Modified <Field> from <Old> to <New>
//	geofence.geometry.updated               AGENCY_KNOWN_LOCATION_MODIFIED Known Location <Name> Modified Shape/Radius/Boundary/Location from <Old> to <New>
//	geofence.status.changed                 AGENCY_KNOWN_LOCATION_MODIFIED Known Location <Name> Modified Status from <Old> to <New>
//	geofence.deleted                        AGENCY_KNOWN_LOCATION_DELETED  Known Location <Name> Deleted
//	geofence.colocation_exclusion.changed   AGENCY_KNOWN_LOCATION_EXCLUDED/INCLUDED   Known Location <Name> Excluded from/Included for Co-Location Report.
//
// Field labels used as FieldChange.Field, exported so that callers
// building FieldChange values (controller.go's geofenceFieldChanges /
// geoJSONFieldChange) and this package's own Action/Category routing
// (LogGeofenceUpdated's switch, below) share one definition instead of
// each hardcoding the same string literals — a typo or a renamed label in
// only one of the two places used to degrade silently (the event would
// still log, just under the generic action/category) with no compiler or
// test signal.
const (
	FieldName     = "Name"
	FieldStatus   = "Status"
	FieldClient   = "Client"
	FieldShape    = "Shape"
	FieldRadius   = "Radius"
	FieldBoundary = "Boundary"
	FieldLocation = "Location"
	FieldAddress  = "Address"
	FieldNotes    = "Notes"
)

const (
	resourceKindGeofence = "geofence"

	actionCreated                    = "geofence.created"
	actionUpdated                    = "geofence.updated"
	actionGeometryUpdated            = "geofence.geometry.updated"
	actionStatusChanged              = "geofence.status.changed"
	actionDeleted                    = "geofence.deleted"
	actionColocationExclusionChanged = "geofence.colocation_exclusion.changed"

	messageAddedFmt    = "Agency Configuration: Known Location %s Added"
	messageModifiedFmt = "Agency Configuration: Known Location %s Modified %s from %s to %s"
	messageDeletedFmt  = "Agency Configuration: Known Location %s Deleted"
	messageExcludedFmt = "Agency Configuration: Known Location %s Excluded from Co-Location Report."
	messageIncludedFmt = "Agency Configuration: Known Location %s Included for Co-Location Report."
)

// LogGeofenceCreated records a geofence-created audit event. details, if
// non-nil, is marshaled to JSON and attached as the request's Details (e.g.
// the created geofence's response payload) — pass the same value you're
// already publishing on events.EventTypeCreated.
func LogGeofenceCreated(ctx context.Context, l activitylogger.Logger, actor Actor, resource Resource, details any) {
	log1(ctx, l, actor, resource, actionCreated, pb.EventCategory_AGENCY_KNOWN_LOCATION_ADDED, fmt.Sprintf(messageAddedFmt, resource.Name), details)
}

// LogGeofenceUpdated records one audit event per changed business field
// from a single PATCH, plus (if the co-location exclusion flag itself
// changed) one dedicated Excluded/Included event — driven entirely by what
// the caller reports changed, never by re-inspecting a request body.
//
// changes is one FieldChange per non-exclusion business field that
// differs (see FieldChange's doc comment for who builds these and how);
// each becomes its own "Modified <Field> from <Old> to <New>" event, so a
// PATCH touching e.g. both name and status emits two distinct, readable
// events rather than one ambiguous "updated" event. Every one of these
// shares Category AGENCY_KNOWN_LOCATION_MODIFIED — unlike the old
// CREATED/UPDATED/DELETED-based schema, this one doesn't distinguish
// "which field changed" at the category level, only at the Action/Field
// level — but a geometry-derived field (i.e. what DiffChangedFields
// reports as geo_json/type, rendered by controller.go's geoJSONFieldChange
// as "Shape", "Radius", "Boundary", "Location" or "Address") and a "Status" field
// still keep their own, more specific Action (actionGeometryUpdated /
// actionStatusChanged) for anyone filtering the activity log by Action
// rather than Category; every other field name falls under the generic
// actionUpdated.
//
// colocationExclusionChanged, if non-nil, means the exclude_from_colocation
// flag changed as part of this PATCH; its pointee is the flag's *new*
// value (true -> Excluded/AGENCY_KNOWN_LOCATION_EXCLUDED, false ->
// Included/AGENCY_KNOWN_LOCATION_INCLUDED) — this flag flips a binary
// setting, so there's no "from/to" wording for it, unlike every other
// field, and it gets its own dedicated Category rather than MODIFIED.
//
// details is attached to every emitted event's Details, exactly like the
// diff map this replaced — pass the same value you're already publishing
// on events.EventTypeUpdated (see hmodel.DiffChangedFields), so a reviewer
// sees everything that changed in that PATCH regardless of which line
// they're reading.
func LogGeofenceUpdated(ctx context.Context, l activitylogger.Logger, actor Actor, resource Resource, changes []FieldChange, colocationExclusionChanged *bool, details any) {
	for _, ch := range changes {
		action := actionUpdated
		switch ch.Field {
		case FieldShape, FieldRadius, FieldBoundary, FieldLocation, FieldAddress:
			action = actionGeometryUpdated
		case FieldStatus:
			action = actionStatusChanged
		}
		log1(ctx, l, actor, resource, action, pb.EventCategory_AGENCY_KNOWN_LOCATION_MODIFIED, fmt.Sprintf(messageModifiedFmt, resource.Name, ch.Field, ch.Old, ch.New), details)
	}

	if colocationExclusionChanged != nil {
		message, category := messageIncludedFmt, pb.EventCategory_AGENCY_KNOWN_LOCATION_INCLUDED
		if *colocationExclusionChanged {
			message, category = messageExcludedFmt, pb.EventCategory_AGENCY_KNOWN_LOCATION_EXCLUDED
		}
		log1(ctx, l, actor, resource, actionColocationExclusionChanged, category, fmt.Sprintf(message, resource.Name), details)
	}
}

// LogGeofenceDeleted records a geofence-deleted audit event.
func LogGeofenceDeleted(ctx context.Context, l activitylogger.Logger, actor Actor, resource Resource) {
	log1(ctx, l, actor, resource, actionDeleted, pb.EventCategory_AGENCY_KNOWN_LOCATION_DELETED, fmt.Sprintf(messageDeletedFmt, resource.Name), nil)
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
		// Platform: set here, per event, rather than once via
		// activitylogger.WithPlatform at logger construction — matching how
		// synergy-sidecar's call sites each set Platform: model.CurrentPlatform()
		// on their own pb.ActivityLogRequest (see e.g. its webhook.go).
		Platform: currentPlatform(),
	}
	if actor.Role != "" {
		// Mirrors synergy-sidecar's WithRole: the key is omitted entirely
		// when there's no role to report, rather than set to "".
		req.Metadata["role"] = actor.Role
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
