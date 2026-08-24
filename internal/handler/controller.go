package handler

import (
	"errors"
	"net/http"
	"strconv"
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
	changes, colocationExclusionChanged := geofenceFieldChanges(&original, entity, diff)
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
// Field labels here (Name/Status/Client/Location/Notes) are the "<Field
// Name>" that appears in the audit UI text and have no other consumer, so
// this is the one place they're defined. Location's value is the geofence
// shape kind (Point/Polygon/Rectangle/Circle) rather than raw GeoJSON —
// dumping full coordinates into a one-line audit message would be
// unreadable, and unlike name/status/notes there is no other short,
// human-meaningful summary of "what changed" about a redrawn boundary.
func geofenceFieldChanges(original, updated *dbmodel.Geofence, diff map[string]any) ([]audit.FieldChange, *bool) {
	var changes []audit.FieldChange
	if _, ok := diff["name"]; ok {
		changes = append(changes, audit.FieldChange{Field: "Name", Old: original.Name, New: updated.Name})
	}
	if _, ok := diff["status"]; ok {
		changes = append(changes, audit.FieldChange{Field: "Status", Old: original.Status.String(), New: updated.Status.String()})
	}
	if _, ok := diff["client_id"]; ok {
		changes = append(changes, audit.FieldChange{Field: "Client", Old: formatUUIDPtr(original.ClientID), New: formatUUIDPtr(updated.ClientID)})
	}
	if _, ok := diff["geo_json"]; ok {
		changes = append(changes, audit.FieldChange{Field: "Location", Old: original.Type.String(), New: updated.Type.String()})
	}
	if _, ok := diff["notes"]; ok {
		changes = append(changes, audit.FieldChange{Field: "Notes", Old: formatStringPtr(original.Notes), New: formatStringPtr(updated.Notes)})
	}

	var colocationExclusionChanged *bool
	if _, ok := diff["exclude_from_colocation"]; ok {
		v := updated.ExcludeFromColocation
		colocationExclusionChanged = &v
	}

	return changes, colocationExclusionChanged
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

// DeleteGeofence handles DELETE: it sets status -> deleted (a terminal
// state distinct from Archived, which a client can still reach directly via
// PUT for its own lifecycle purposes). The acting user is supplied by the
// caller via the required user_id query param and recorded as the row's
// updated_by; requireAgencyAdmin only verifies the caller is an Agency
// Admin/System Administrator/Global Administrator for the target agency,
// not that user_id matches the authenticated subject. That's an accepted
// tradeoff for the business record and the Pulsar lifecycle event, but not
// for the audit trail below: see auditActorID/authenticatedUserID, which
// attribute it to the authenticated subject instead whenever one's
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
