package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"
	"github.com/sentinelgo/synergy-geofence/internal/audit"
	"github.com/sentinelgo/synergy-geofence/internal/database"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/database/spatial"
	localerrors "github.com/sentinelgo/synergy-geofence/internal/errors"
	"github.com/sentinelgo/synergy-geofence/internal/events"
	hmodel "github.com/sentinelgo/synergy-geofence/internal/handler/model"

	activitylogger "github.com/sentinelgo/synergy-common/pkg/activity-logger"
	"github.com/sentinelgo/synergy-common/pkg/config"
	cmodel "github.com/sentinelgo/synergy-common/pkg/database/model"
	"github.com/sentinelgo/synergy-common/pkg/log"
	"github.com/sentinelgo/synergy-common/pkg/otelx"
	gincommon "github.com/sentinelgo/synergy-common/pkg/otelx/gin"
)

const (
	defaultPage     = 1
	defaultPageSize = 25
	maxPageSize     = 200
)

type GeofenceController struct {
	datasource  database.GeofenceDbAdapter
	cfg         *config.Config
	publisher   events.Publisher
	activityLog activitylogger.Logger
}

func NewGeofenceController(datasource database.GeofenceDbAdapter, cfg *config.Config, publisher events.Publisher, activityLog activitylogger.Logger) *GeofenceController {
	return &GeofenceController{datasource: datasource, cfg: cfg, publisher: publisher, activityLog: activityLog}
}

// auditActorID is the identity an audit event's Actor is attributed to:
// the authenticated caller's own subject (see authenticatedUserID)
// whenever one was resolved, falling back to clientSupplied — the same
// request-body/query user_id already used for the business record and the
// Pulsar lifecycle event — only in the dev-only bypass where no
// authenticated subject exists at all.
func auditActorID(c *gin.Context, clientSupplied uuid.UUID) uuid.UUID {
	if id, ok := authenticatedUserID(c); ok {
		return id
	}
	return clientSupplied
}

func (x *GeofenceController) CreateGeofence(c *gin.Context) {
	ctx, span := otelx.StartTracer(gincommon.UnwrapContext(c))
	defer otelx.End(span, nil)
	if deferred, ok := gincommon.UpdateContext(c, ctx); ok {
		defer deferred.Defer()
	}

	l := log.LoggerFromContext(c)
	agencyID := pkg.AgencyID(c)

	var req hmodel.GeofenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		l.With("error", err).Error("failed to bind create geofence request")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrBindJson, localerrors.GeofenceReq)})
		return
	}

	entity, err := req.ToModel(agencyID, spatial.DefaultSRID)
	if err != nil {
		l.With("error", err).Error("failed to convert geofence request")
		respondToModelError(c, err)
		return
	}

	if err = x.datasource.CreateGeofence(c, entity); err != nil {
		if errors.Is(err, cmodel.ErrDuplicateRecord) {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrDuplicate, localerrors.Name)})
			return
		}
		l.With(pkg.ErrorKey, err.Error()).Error("failed to create geofence")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceCreate, localerrors.Geofence)})
		return
	}

	resp, err := hmodel.FromModel(entity)
	if err != nil {
		l.With("error", err).Error("failed to build geofence response")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrConvertData, localerrors.Geofence)})
		return
	}

	x.publisher.Publish(c, events.Event{
		EventType:  events.EventTypeCreated,
		GeofenceID: entity.ID,
		AgencyID:   entity.AgencyID,
		UserID:     entity.CreatedBy,
		Timestamp:  time.Now().UTC(),
		Payload:    resp,
	})
	audit.LogGeofenceCreated(c, x.activityLog,
		audit.Actor{UserID: auditActorID(c, entity.CreatedBy), IP: c.ClientIP(), Role: pkg.ResolvedRole(c)},
		audit.Resource{ID: entity.ID, Name: entity.Name, AgencyID: entity.AgencyID},
		resp,
	)

	c.JSON(http.StatusCreated, gin.H{pkg.Code: http.StatusCreated, pkg.Message: "geofence created successfully", pkg.Type: pkg.SuccessKey, pkg.DataKey: resp})
}

// respondToModelError maps a GeofenceRequest.ToModel error to a specific
// error code: name-charset violations get ErrInvalidName, anything else
// (an invalid/out-of-range geometry) gets ErrInvalidGeometry.
func respondToModelError(c *gin.Context, err error) {
	if errors.Is(err, hmodel.ErrInvalidNameCharset) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrInvalidName, localerrors.Name)})
		return
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrInvalidGeometry, localerrors.Geofence)})
}

func (x *GeofenceController) GetGeofence(c *gin.Context) {
	ctx, span := otelx.StartTracer(gincommon.UnwrapContext(c))
	defer otelx.End(span, nil)
	if deferred, ok := gincommon.UpdateContext(c, ctx); ok {
		defer deferred.Defer()
	}

	l := log.LoggerFromContext(c)
	agencyID := pkg.AgencyID(c)

	geofenceID, err := uuid.Parse(c.Param("geofence_id"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrUUIDParse, localerrors.GeofenceId)})
		return
	}

	entity, err := x.datasource.FindByID(c, agencyID, geofenceID)
	if err != nil {
		if errors.Is(err, cmodel.ErrRecordNotFound) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
			return
		}
		l.With(pkg.ErrorKey, err.Error()).Error("failed to find geofence")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
		return
	}

	resp, err := hmodel.FromModel(entity)
	if err != nil {
		l.With("error", err).Error("failed to build geofence response")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrConvertData, localerrors.Geofence)})
		return
	}

	c.JSON(http.StatusOK, gin.H{pkg.DataKey: resp})
}

func (x *GeofenceController) ListGeofences(c *gin.Context) {
	ctx, span := otelx.StartTracer(gincommon.UnwrapContext(c))
	defer otelx.End(span, nil)
	if deferred, ok := gincommon.UpdateContext(c, ctx); ok {
		defer deferred.Defer()
	}

	l := log.LoggerFromContext(c)
	agencyID := pkg.AgencyID(c)

	clientID, err := parseOptionalUUIDQuery(c, "client_id")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrUUIDParse, localerrors.ClientId)})
		return
	}

	// No status filter defaults to "everything except deleted" — see
	// FindByAgencyAndClient's doc comment.
	var status *dbmodel.GeofenceStatus
	if raw := c.Query("status"); raw != "" {
		s := hmodel.StatusToDB(raw)
		if !s.Valid() {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrMismatch, localerrors.Geofence)})
			return
		}
		status = &s
	}

	page, pageSize := parsePagination(c)

	entities, total, err := x.datasource.FindByAgencyAndClient(c, agencyID, clientID, status, page, pageSize)
	if err != nil {
		l.With(pkg.ErrorKey, err.Error()).Error("failed to list geofences")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
		return
	}

	items := make([]*hmodel.Geofence, 0, len(entities))
	for _, entity := range entities {
		resp, ferr := hmodel.FromModel(entity)
		if ferr != nil {
			l.With("error", ferr).Error("failed to build geofence response")
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrConvertData, localerrors.Geofence)})
			return
		}
		items = append(items, resp)
	}

	c.JSON(http.StatusOK, gin.H{
		pkg.DataKey: items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

func (x *GeofenceController) UpdateGeofence(c *gin.Context) {
	ctx, span := otelx.StartTracer(gincommon.UnwrapContext(c))
	defer otelx.End(span, nil)
	if deferred, ok := gincommon.UpdateContext(c, ctx); ok {
		defer deferred.Defer()
	}

	l := log.LoggerFromContext(c)
	agencyID := pkg.AgencyID(c)

	geofenceID, err := uuid.Parse(c.Param("geofence_id"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrUUIDParse, localerrors.GeofenceId)})
		return
	}

	var req hmodel.PatchGeofenceRequest
	if err = c.ShouldBindJSON(&req); err != nil {
		l.With("error", err).Error("failed to bind update geofence request")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrBindJson, localerrors.GeofenceReq)})
		return
	}

	entity, err := x.datasource.FindByID(c, agencyID, geofenceID)
	if err != nil {
		if errors.Is(err, cmodel.ErrRecordNotFound) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
			return
		}
		l.With(pkg.ErrorKey, err.Error()).Error("failed to find geofence")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
		return
	}
	original := *entity

	selectFields, err := req.ApplyTo(entity, spatial.DefaultSRID)
	if err != nil {
		l.With("error", err).Error("failed to apply geofence patch")
		respondToModelError(c, err)
		return
	}
	entity.UpdatedBy = &req.UserID
	selectFields = append(selectFields, "updated_by")

	// The WHERE clause compares against the client's claimed version, not
	// entity's just-fetched (and therefore always-current) one — otherwise
	// the optimistic-concurrency check could never fail.
	entity.Version = req.Version

	if err = x.datasource.UpdateGeofence(c, entity, selectFields...); err != nil {
		if errors.Is(err, cmodel.ErrOptimisticLock) {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrOptimisticLock, localerrors.Geofence)})
			return
		}
		if errors.Is(err, cmodel.ErrRecordNotFound) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
			return
		}
		if errors.Is(err, cmodel.ErrDuplicateRecord) {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrDuplicate, localerrors.Name)})
			return
		}
		l.With(pkg.ErrorKey, err.Error()).Error("failed to update geofence")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceUpdate, localerrors.Geofence)})
		return
	}

	resp, err := hmodel.FromModel(entity)
	if err != nil {
		l.With("error", err).Error("failed to build geofence response")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrConvertData, localerrors.Geofence)})
		return
	}

	diff := hmodel.DiffChangedFields(&original, entity)
	x.publisher.Publish(c, events.Event{
		EventType:  events.EventTypeUpdated,
		GeofenceID: entity.ID,
		AgencyID:   entity.AgencyID,
		UserID:     req.UserID,
		Timestamp:  time.Now().UTC(),
		Payload:    diff,
	})
	changes, colocationExclusionChanged := geofenceFieldChanges(c, &original, entity, diff)
	audit.LogGeofenceUpdated(c, x.activityLog,
		audit.Actor{UserID: auditActorID(c, req.UserID), IP: c.ClientIP(), Role: pkg.ResolvedRole(c)},
		audit.Resource{ID: entity.ID, Name: entity.Name, AgencyID: entity.AgencyID},
		changes, colocationExclusionChanged, diff,
	)

	c.JSON(http.StatusOK, gin.H{pkg.Code: http.StatusOK, pkg.Message: "geofence updated successfully", pkg.Type: pkg.SuccessKey, pkg.DataKey: resp})
}

// geofenceFieldChanges turns diff (hmodel.DiffChangedFields' new-value-only
// map, already computed by the caller for the Pulsar payload) into the
// audit trail's shape: one audit.FieldChange per changed business field,
// each carrying both the old and new value read straight off original/
// updated, plus a separate *bool for exclude_from_colocation, which the
// Known Location spec renders as an Excluded/Included message rather than
// a generic "Modified" one (see audit.LogGeofenceUpdated).
//
// Field labels here (Name/Status/Client/Shape/Radius/Boundary/Location/
// Address/Notes) are the "<Field Name>" that appears in the audit UI text
// and have no other consumer, so this is the one place they're defined
// (as the audit.Field* constants). The geo_json case is handled by
// geoJSONFieldChange, which can return more than one FieldChange for a
// single PATCH — see its doc comment for why a single "Location: <kind>
// to <kind>" summary isn't enough.
func geofenceFieldChanges(ctx context.Context, original, updated *dbmodel.Geofence, diff map[string]any) ([]audit.FieldChange, *bool) {
	var changes []audit.FieldChange
	if _, ok := diff["name"]; ok {
		changes = append(changes, audit.FieldChange{Field: audit.FieldName, Old: original.Name, New: updated.Name})
	}
	if _, ok := diff["status"]; ok {
		changes = append(changes, audit.FieldChange{Field: audit.FieldStatus, Old: original.Status.String(), New: updated.Status.String()})
	}
	if _, ok := diff["client_id"]; ok {
		changes = append(changes, audit.FieldChange{Field: audit.FieldClient, Old: formatUUIDPtr(original.ClientID), New: formatUUIDPtr(updated.ClientID)})
	}
	if _, ok := diff["geo_json"]; ok {
		changes = append(changes, geoJSONFieldChange(ctx, original, updated)...)
	}
	if _, ok := diff["notes"]; ok {
		changes = append(changes, audit.FieldChange{Field: audit.FieldNotes, Old: formatStringPtr(original.Notes), New: formatStringPtr(updated.Notes)})
	}

	var colocationExclusionChanged *bool
	if _, ok := diff["exclude_from_colocation"]; ok {
		v := updated.ExcludeFromColocation
		colocationExclusionChanged = &v
	}

	return changes, colocationExclusionChanged
}

// geoJSONFieldChange builds the audit.FieldChange(s) for a geo_json diff —
// usually one, but two when a single PATCH changes both the shape and its
// address (see below).
//
// DiffChangedFields flags geo_json as changed on *any* redraw — moving a
// Point, resizing a Rectangle, reshaping a Polygon, changing a Circle's
// radius, or editing the embedded Address — not just when the shape kind
// itself changes. Reporting Type.String() as both Old and New (the
// original behavior) is only accurate for an actual kind change; whenever
// the kind stays the same (the common case — e.g. only a Circle's radius
// moved) it degenerates into a misleading "Modified Location from Circle
// to Circle", since Old == New. This picks a field label and Old/New pair
// per aspect that actually differs instead:
//   - kind changed (Point/Polygon/Rectangle/Circle): "Shape", old/new kind
//     (reported alone — Address isn't diffed here; a changed kind isn't a
//     structurally comparable Address before/after).
//   - Circle, radius changed: "Radius", old/new radius in meters.
//   - Circle, only the center moved: "Location", old/new center point.
//   - Rectangle/Polygon: "Boundary", old/new bounding box + point count.
//   - Point: "Location", old/new coordinates.
//   - Address (any shape, alongside the above if both changed in one
//     PATCH): "Address", old/new formatted address — same treatment as
//     Name touching two events for one PATCH, so a PATCH that both moves a
//     shape and corrects its address produces two distinct, readable
//     events rather than losing one of them.
//
// original/updated.GeoJSON are parsed into hmodel.GeoJson to reach these
// shape-specific attributes — geo_json diffing alone can't surface them. A
// parse failure or a Type/geo_json variant mismatch (neither expected for
// already-persisted rows — see the Warn calls below), or a diff where
// nothing above actually differs (e.g. floating-point rounding masked a
// sub-precision coordinate nudge — see formatPoint), falls back to
// noSummarizableChange rather than dropping the audit event or repeating
// the "X to X" bug via original.Type.String() == updated.Type.String().
// ctx is used only for that logging — ctx.Err() isn't checked, since this
// is pure in-memory string formatting over already-fetched rows, not an
// I/O call that could be cancelled partway through.
func geoJSONFieldChange(ctx context.Context, original, updated *dbmodel.Geofence) []audit.FieldChange {
	if original.Type != updated.Type {
		return []audit.FieldChange{{Field: audit.FieldShape, Old: original.Type.String(), New: updated.Type.String()}}
	}

	var oldGeo, newGeo hmodel.GeoJson
	if err := json.Unmarshal([]byte(original.GeoJSON), &oldGeo); err != nil {
		log.LoggerFromContext(ctx).With("error", err, "geofence_id", updated.ID).
			Warn("failed to parse original geo_json for audit diff — falling back to a generic Boundary message")
		return []audit.FieldChange{noSummarizableChange}
	}
	if err := json.Unmarshal([]byte(updated.GeoJSON), &newGeo); err != nil {
		log.LoggerFromContext(ctx).With("error", err, "geofence_id", updated.ID).
			Warn("failed to parse updated geo_json for audit diff — falling back to a generic Boundary message")
		return []audit.FieldChange{noSummarizableChange}
	}

	geomChange, ok := geometryFieldChange(oldGeo, newGeo)
	if !ok {
		// The parsed variants didn't line up with original/updated's shared
		// Type (e.g. Type says Circle but geo_json's own "type" discriminator
		// says something else) — a data-integrity anomaly worth flagging,
		// unlike the "nothing to report" case below.
		log.LoggerFromContext(ctx).With("geofence_id", updated.ID, "type", updated.Type.String()).
			Warn("geo_json variant does not match Type column for audit diff — falling back to a generic Boundary message")
		return []audit.FieldChange{noSummarizableChange}
	}

	var changes []audit.FieldChange
	if geomChange.Old != geomChange.New {
		changes = append(changes, geomChange)
	}
	if oldAddr, newAddr := addressOf(oldGeo), addressOf(newGeo); !addressEqual(oldAddr, newAddr) {
		changes = append(changes, audit.FieldChange{Field: audit.FieldAddress, Old: formatAddress(oldAddr), New: formatAddress(newAddr)})
	}
	if len(changes) == 0 {
		// geo_json differs (DiffChangedFields already confirmed the raw
		// bytes changed) but neither the geometry nor the address
		// summarizes an actual difference. Expected occasionally (e.g. the
		// floating-point rounding case above), not an anomaly — don't warn,
		// but don't repeat the "X to X" bug with a different field name
		// either.
		changes = append(changes, noSummarizableChange)
	}
	return changes
}

// noSummarizableChange is the geoJSONFieldChange fallback for every case
// where geo_json is known to differ (DiffChangedFields already checked
// old.GeoJSON != updated.GeoJSON) but this function has no field-specific
// value pair left to report — a parse failure, a variant mismatch, or a
// diff confined to something neither the geometry nor Address diffing
// summarizes. Old and New are deliberately distinct placeholder text so
// this never repeats the original "Modified Location from Circle to
// Circle" bug, whose defining symptom was Old == New.
var noSummarizableChange = audit.FieldChange{Field: audit.FieldBoundary, Old: "(previous shape details)", New: "(updated shape details)"}

// addressOf returns whichever populated hmodel.GeoJson variant's Address
// field is set, or nil if none is (an unpopulated union, which shouldn't
// happen here — the caller only reaches this after geometryFieldChange
// already confirmed a populated, matching variant — or a variant with no
// Address on either side).
func addressOf(g hmodel.GeoJson) *hmodel.Address {
	switch {
	case g.Circle != nil:
		return g.Circle.Address
	case g.Rectangle != nil:
		return g.Rectangle.Address
	case g.Polygon != nil:
		return g.Polygon.Address
	case g.Point != nil:
		return g.Point.Address
	default:
		return nil
	}
}

// addressEqual compares two possibly-nil Addresses by value — Address's
// fields are all plain strings/ints, so a direct struct comparison (once
// both are known non-nil) is exact and doesn't need reflect.DeepEqual.
func addressEqual(a, b *hmodel.Address) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// formatAddress renders an Address as a single "(none)"-or-comma-joined
// line for the audit "from <Old> to <New>" text, e.g. "123 Main St,
// Atlanta, GA, 30301" — matching formatUUIDPtr/formatStringPtr's "(none)"
// convention for a field that's absent rather than empty. Fields left
// blank on the Address itself (e.g. no Street2) are simply omitted rather
// than leaving a stray ", " in the line.
func formatAddress(a *hmodel.Address) string {
	if a == nil {
		return "(none)"
	}
	state := a.StateProvince
	if state == "" {
		state = a.StateProvinceName
	}
	var parts []string
	for _, p := range []string{a.Name, a.Street1, a.Street2, a.City, state, a.PostalCode, a.Country} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

// geometryFieldChange dispatches on which hmodel.GeoJson variant is
// populated. ok is false if old/new don't share a populated variant, which
// shouldn't happen given original.Type == updated.Type already checked by
// the caller.
func geometryFieldChange(oldGeo, newGeo hmodel.GeoJson) (audit.FieldChange, bool) {
	switch {
	case oldGeo.Circle != nil && newGeo.Circle != nil:
		return circleFieldChange(oldGeo.Circle, newGeo.Circle), true
	case oldGeo.Rectangle != nil && newGeo.Rectangle != nil:
		return audit.FieldChange{
			Field: audit.FieldBoundary,
			Old:   boundingBox(oldGeo.Rectangle.Coordinates[:]),
			New:   boundingBox(newGeo.Rectangle.Coordinates[:]),
		}, true
	case oldGeo.Polygon != nil && newGeo.Polygon != nil:
		return audit.FieldChange{
			Field: audit.FieldBoundary,
			Old:   polygonBoundarySummary(oldGeo.Polygon.Coordinates),
			New:   polygonBoundarySummary(newGeo.Polygon.Coordinates),
		}, true
	case oldGeo.Point != nil && newGeo.Point != nil:
		return audit.FieldChange{
			Field: audit.FieldLocation,
			Old:   formatPoint(oldGeo.Point.Coordinates),
			New:   formatPoint(newGeo.Point.Coordinates),
		}, true
	default:
		return audit.FieldChange{}, false
	}
}

// circleFieldChange reports a Circle's radius change if the radius
// differs — the common case, and the one the reported bug was about —
// falling back to reporting the center point otherwise (a Circle redrawn
// by dragging its center, radius unchanged).
func circleFieldChange(old, updated *hmodel.GeoJsonCircle) audit.FieldChange {
	if old.Radius != updated.Radius {
		return audit.FieldChange{Field: audit.FieldRadius, Old: formatRadius(old.Radius), New: formatRadius(updated.Radius)}
	}
	return audit.FieldChange{Field: audit.FieldLocation, Old: formatPoint(old.Coordinates), New: formatPoint(updated.Coordinates)}
}

// formatRadius renders a Circle's radius in meters, e.g. "500 m". Radius is
// validated to MinCircleRadiusMeters..MaxCircleRadiusMeters
// (spatial.NewCircleGeometry) before persistence, so this only ever
// formats a sane, finite value.
func formatRadius(radiusMeters float64) string {
	return strconv.FormatFloat(radiusMeters, 'f', -1, 64) + " m"
}

// formatPoint renders a [longitude, latitude] pair (spatial.Point's
// documented order) as human-reading-order "(lat, lng)".
func formatPoint(p spatial.Point) string {
	return fmt.Sprintf("(%.6f, %.6f)", p[1], p[0])
}

// polygonBoundarySummary summarizes a Polygon's rings as a point count plus
// bounding box, e.g. "6 point(s), (33.749000, -84.388000) to (33.755000,
// -84.380000)" — a full point-by-point diff would be unreadable in a
// one-line audit message, and unlike Circle's radius there's no single
// scalar that captures "what changed" about a redrawn polygon.
func polygonBoundarySummary(rings []spatial.Ring) string {
	var points int
	var pts []spatial.Point
	for _, ring := range rings {
		points += len(ring)
		pts = append(pts, ring...)
	}
	return fmt.Sprintf("%d point(s), %s", points, boundingBox(pts))
}

// boundingBox renders the smallest box containing every point as
// "(minLat, minLng) to (maxLat, maxLng)".
func boundingBox(pts []spatial.Point) string {
	if len(pts) == 0 {
		return "(empty)"
	}
	minLng, minLat := pts[0][0], pts[0][1]
	maxLng, maxLat := pts[0][0], pts[0][1]
	for _, p := range pts[1:] {
		minLng, maxLng = math.Min(minLng, p[0]), math.Max(maxLng, p[0])
		minLat, maxLat = math.Min(minLat, p[1]), math.Max(maxLat, p[1])
	}
	return fmt.Sprintf("%s to %s", formatPoint(spatial.Point{minLng, minLat}), formatPoint(spatial.Point{maxLng, maxLat}))
}

// formatUUIDPtr and formatStringPtr render a nullable field for the audit
// "from <Old> to <New>" text — "(none)" rather than an empty string, so a
// value that was/became absent still reads as a real before/after state.
func formatUUIDPtr(id *uuid.UUID) string {
	if id == nil {
		return "(none)"
	}
	return id.String()
}

func formatStringPtr(s *string) string {
	if s == nil {
		return "(none)"
	}
	return *s
}

// DeleteGeofence handles DELETE: it hard-deletes the row (distinct from
// Archived, which a client can still reach directly via PUT for its own
// lifecycle purposes) so the Location Name is immediately free for reuse
// within the agency — see GeofenceDbAdapter.DeleteGeofence's doc comment.
// The acting user is supplied by the caller via the required user_id query
// param and used for the Pulsar lifecycle event below (there's no row left
// to persist it on); requireAgencyAdmin only verifies the caller is an
// Agency Admin/System Administrator/Global Administrator for the target
// agency, not that user_id matches the authenticated subject. That's an
// accepted tradeoff for the business record and the Pulsar lifecycle event,
// but not for the audit trail below: see auditActorID/authenticatedUserID,
// which attribute it to the authenticated subject instead whenever one's
// available.
func (x *GeofenceController) DeleteGeofence(c *gin.Context) {
	ctx, span := otelx.StartTracer(gincommon.UnwrapContext(c))
	defer otelx.End(span, nil)
	if deferred, ok := gincommon.UpdateContext(c, ctx); ok {
		defer deferred.Defer()
	}

	l := log.LoggerFromContext(c)
	agencyID := pkg.AgencyID(c)

	geofenceID, err := uuid.Parse(c.Param("geofence_id"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrUUIDParse, localerrors.GeofenceId)})
		return
	}

	userID, err := uuid.Parse(c.Query("user_id"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrUUIDParse, localerrors.UserId)})
		return
	}

	// Known Location's Deleted message requires the location's name (see
	// audit.Resource's doc comment), so unlike before, this handler now
	// loads the row before deleting it by id rather than deleting blind.
	// DeleteGeofence itself does its own FindByID internally (it needs the
	// full row to flip status/updated_by), so this is a second read, not a
	// second write — an accepted tradeoff for a required audit field.
	entity, err := x.datasource.FindByID(c, agencyID, geofenceID)
	if err != nil {
		if errors.Is(err, cmodel.ErrRecordNotFound) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
			return
		}
		l.With(pkg.ErrorKey, err.Error()).Error("failed to find geofence")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
		return
	}

	if err = x.datasource.DeleteGeofence(c, agencyID, geofenceID, userID); err != nil {
		if errors.Is(err, cmodel.ErrRecordNotFound) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceFind, localerrors.Geofence)})
			return
		}
		l.With(pkg.ErrorKey, err.Error()).Error("failed to delete geofence")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrGeofenceDelete, localerrors.Geofence)})
		return
	}

	x.publisher.Publish(c, events.Event{
		EventType:  events.EventTypeDeleted,
		GeofenceID: geofenceID,
		AgencyID:   agencyID,
		UserID:     userID,
		Timestamp:  time.Now().UTC(),
	})
	audit.LogGeofenceDeleted(c, x.activityLog,
		audit.Actor{UserID: auditActorID(c, userID), IP: c.ClientIP(), Role: pkg.ResolvedRole(c)},
		audit.Resource{ID: geofenceID, Name: entity.Name, AgencyID: agencyID},
	)

	c.JSON(http.StatusOK, gin.H{pkg.Code: http.StatusOK, pkg.Message: "geofence deleted successfully", pkg.Type: pkg.SuccessKey})
}

// LookupGeofences answers the key query pattern this service exists for:
// "which active geofences (scoped to an agency, optionally a client)
// contain this point?" — driven by the Oracle Spatial index via SDO_RELATE.
func (x *GeofenceController) LookupGeofences(c *gin.Context) {
	ctx, span := otelx.StartTracer(gincommon.UnwrapContext(c))
	defer otelx.End(span, nil)
	if deferred, ok := gincommon.UpdateContext(c, ctx); ok {
		defer deferred.Defer()
	}

	l := log.LoggerFromContext(c)
	agencyID := pkg.AgencyID(c)

	lon, err := strconv.ParseFloat(c.Query("lon"), 64)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrInvalidQuery, localerrors.Coordinates)})
		return
	}
	lat, err := strconv.ParseFloat(c.Query("lat"), 64)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrInvalidQuery, localerrors.Coordinates)})
		return
	}

	clientID, err := parseOptionalUUIDQuery(c, "client_id")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrUUIDParse, localerrors.ClientId)})
		return
	}

	entities, err := x.datasource.FindContainingPoint(c, agencyID, clientID, spatial.Point{lon, lat}, spatial.DefaultSRID)
	if err != nil {
		l.With(pkg.ErrorKey, err.Error()).Error("failed to look up geofences containing point")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrInvalidQuery, localerrors.Lookup)})
		return
	}

	items := make([]*hmodel.Geofence, 0, len(entities))
	for _, entity := range entities {
		resp, ferr := hmodel.FromModel(entity)
		if ferr != nil {
			l.With("error", ferr).Error("failed to build geofence response")
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrConvertData, localerrors.Geofence)})
			return
		}
		items = append(items, resp)
	}

	c.JSON(http.StatusOK, gin.H{pkg.DataKey: items})
}

func parseOptionalUUIDQuery(c *gin.Context, key string) (*uuid.UUID, error) {
	raw := c.Query(key)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func parsePagination(c *gin.Context) (page, pageSize int) {
	page = defaultPage
	pageSize = defaultPageSize

	if raw := c.Query("page"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			page = v
		}
	}
	if raw := c.Query("page_size"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= maxPageSize {
			pageSize = v
		}
	}

	return page, pageSize
}
