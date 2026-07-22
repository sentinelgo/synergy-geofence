package database

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/database/spatial"
	"gorm.io/gorm"

	"github.com/sentinelgo/synergy-common/pkg/database"
	cmodel "github.com/sentinelgo/synergy-common/pkg/database/model"
)

// GeofenceDbAdapter is the geofence-service repository interface: the
// generic CacheDbAdapter[uuid.UUID] gives CRUD/versioned-update/cache for
// free, plus a handful of geofence-specific finders and the spatial
// point-lookup query.
type GeofenceDbAdapter interface {
	database.CacheDbAdapter[uuid.UUID]

	FindByID(ctx context.Context, agencyID, id uuid.UUID) (*model.Geofence, error)
	// FindByAgencyAndClient lists geofences for an agency (optionally
	// scoped further to a client). When status is nil it excludes
	// GeofenceStatusDeleted by default (deleted rows only surface when
	// explicitly requested via status).
	FindByAgencyAndClient(ctx context.Context, agencyID uuid.UUID, clientID *uuid.UUID, status *model.GeofenceStatus, page, pageSize int) ([]*model.Geofence, int64, error)
	CreateGeofence(ctx context.Context, g *model.Geofence) error
	UpdateGeofence(ctx context.Context, g *model.Geofence, selectFields ...any) error
	// DeleteGeofence soft-deletes (status -> deleted, a terminal state
	// distinct from Archived) and records userID as the actor in updated_by.
	DeleteGeofence(ctx context.Context, agencyID, id, userID uuid.UUID) error
	FindContainingPoint(ctx context.Context, agencyID uuid.UUID, clientID *uuid.UUID, point spatial.Point, srid int) ([]*model.Geofence, error)
}

func NewGeofenceAdapter(dbConnection database.Adapter[uuid.UUID]) GeofenceDbAdapter {
	dba, _ := database.ToCacheDbAdapter[uuid.UUID](dbConnection)
	return &dbAdapter{CacheDbAdapter: dba}
}

type dbAdapter struct {
	database.CacheDbAdapter[uuid.UUID]
}

// geofenceReadColumns is every geofence column except geometry, and must be
// used (via .Select) on every query that reads rows from this table — not
// just relied on via Geometry's `->:false` gorm tag. Confirmed empirically
// against a live Oracle instance: a plain SELECT * (what ReadWhere/.Find()
// generate by default) makes go-ora's low-level column decoder attempt to
// decode the SDO_GEOMETRY UDT for every row, which panics
// (nil pointer dereference in decodeObject) since the type was never
// registered with the driver. This happens at the driver/wire level before
// GORM's own field-permission-based scan targeting ever gets involved, so
// `->:false` alone does not prevent it — the column must never be selected.
var geofenceReadColumns = []string{
	"id", "created_at", "updated_at", "version",
	"agency_id", "client_id", "name", "type", "geo_json", "status",
	"exclude_from_colocation", "notes", "created_by", "updated_by",
}

func (dbc *dbAdapter) FindByID(ctx context.Context, agencyID, id uuid.UUID) (*model.Geofence, error) {
	entity := new(model.Geofence)
	err := dbc.WithGormDB(ctx, func(db *gorm.DB) error {
		return cmodel.ConvertGormErrors(ctx, db.Select(geofenceReadColumns).Where("id = ? AND agency_id = ?", id, agencyID).First(entity), false)
	})
	if err != nil {
		return nil, err
	}
	return entity, nil
}

func (dbc *dbAdapter) FindByAgencyAndClient(ctx context.Context, agencyID uuid.UUID, clientID *uuid.UUID, status *model.GeofenceStatus, page, pageSize int) ([]*model.Geofence, int64, error) {
	entities := make([]*model.Geofence, 0)
	var total int64

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 25
	}

	err := dbc.WithGormDB(ctx, func(db *gorm.DB) error {
		q := db.Model(&model.Geofence{}).Where("agency_id = ?", agencyID)
		if clientID != nil {
			q = q.Where("client_id = ?", *clientID)
		}
		if status != nil {
			q = q.Where("status = ?", int(*status))
		} else {
			q = q.Where("status != ?", int(model.GeofenceStatusDeleted))
		}
		if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
			return err
		}
		return q.Select(geofenceReadColumns).Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&entities).Error
	})
	if err != nil {
		return nil, 0, err
	}

	return entities, total, nil
}

// CreateGeofence inserts via hand-written SQL rather than the generic
// Adapter.Create: cmmoran/gorm-oracle's Create callback never checks
// GormValuerInterface (confirmed empirically — see
// spatial.Geometry.GormValue's doc comment), so a normal struct-based
// .Create() sends the raw Geometry struct to the driver instead of
// invoking GormValue, and go-ora rejects it as an unregistered user-defined
// type. InlineSQL sidesteps that entirely by embedding the SDO_GEOMETRY
// constructor as a literal.
func (dbc *dbAdapter) CreateGeofence(ctx context.Context, g *model.Geofence) error {
	now := time.Now().UTC().Truncate(time.Microsecond)
	g.CreatedAt = now
	g.UpdatedAt = &now

	// updated_at is NOT NULL but has no DB-level DEFAULT (GORM's
	// autoUpdateTime tag only populates it via the normal callback
	// pipeline, which this raw INSERT bypasses) — set it explicitly,
	// matching created_at on first insert.
	query := fmt.Sprintf(`INSERT INTO geofence_rules (
		id, created_at, updated_at, version, agency_id, client_id, name, type, geometry, geo_json, status,
		exclude_from_colocation, notes, created_by
	) VALUES (
		:id, :created_at, :updated_at, :version, :agency_id, :client_id, :name, :type, %s, :geo_json, :status,
		:exclude_from_colocation, :notes, :created_by
	)`, g.Geometry.InlineSQL())

	args := []interface{}{
		uuidBytes(g.ID), g.CreatedAt, now, g.Version, uuidBytes(g.AgencyID), uuidBytesPtr(g.ClientID), g.Name, int(g.Type),
		g.GeoJSON, int(g.Status), boolToInt(g.ExcludeFromColocation), g.Notes, uuidBytes(g.CreatedBy),
	}

	return dbc.WithGormDB(ctx, func(db *gorm.DB) error {
		return cmodel.ConvertGormErrors(ctx, db.Exec(query, args...), true)
	})
}

// updateColumnSQL maps a selectable field name to its `column = :bind` SQL
// fragment, for UpdateGeofence's dynamic SET clause. geometry has no entry
// here — it's always inlined directly (see UpdateGeofence), for the same
// GormValuerInterface gap CreateGeofence works around.
var updateColumnSQL = map[string]string{
	"name":                    "name = :name",
	"type":                    "type = :type",
	"geo_json":                "geo_json = :geo_json",
	"status":                  "status = :status",
	"client_id":               "client_id = :client_id",
	"exclude_from_colocation": "exclude_from_colocation = :exclude_from_colocation",
	"notes":                   "notes = :notes",
	"updated_by":              "updated_by = :updated_by",
}

// UpdateGeofence performs a hand-rolled optimistic, version-checked update
// via raw SQL (for the same driver-compatibility reason as CreateGeofence —
// this is also why cmmoran/optimistic's plugin isn't used here: it hooks
// into GORM's Update callback chain, which a raw db.Exec bypasses entirely,
// so the version check/bump has to be done explicitly). When selectFields
// is empty it updates every mutable business column, including geometry;
// pass an explicit subset (e.g. just "status", "updated_by") for partial
// updates that must leave geometry/geo_json/etc. untouched.
func (dbc *dbAdapter) UpdateGeofence(ctx context.Context, g *model.Geofence, selectFields ...any) error {
	if len(selectFields) == 0 {
		selectFields = []any{"name", "type", "geometry", "geo_json", "status", "client_id", "exclude_from_colocation", "notes", "updated_by"}
	}

	argByField := map[string]interface{}{
		"name":                    g.Name,
		"type":                    int(g.Type),
		"geo_json":                g.GeoJSON,
		"status":                  int(g.Status),
		"client_id":               uuidBytesPtr(g.ClientID),
		"exclude_from_colocation": boolToInt(g.ExcludeFromColocation),
		"notes":                   g.Notes,
		"updated_by":              uuidBytesPtr(g.UpdatedBy),
	}

	var setSQL []string
	var args []interface{}
	for _, f := range selectFields {
		name, _ := f.(string)
		if name == "geometry" {
			setSQL = append(setSQL, "geometry = "+g.Geometry.InlineSQL())
			continue
		}
		frag, known := updateColumnSQL[name]
		if !known {
			continue
		}
		setSQL = append(setSQL, frag)
		args = append(args, argByField[name])
	}
	if len(setSQL) == 0 {
		return nil
	}

	newVersion := g.Version + 1
	setSQL = append(setSQL, "updated_at = :updated_at", "version = :new_version")
	args = append(args, time.Now().UTC().Truncate(time.Microsecond), newVersion, uuidBytes(g.ID), g.Version)

	query := fmt.Sprintf(`UPDATE geofence_rules SET %s WHERE id = :id AND version = :expected_version`, strings.Join(setSQL, ", "))

	err := dbc.WithGormDB(ctx, func(db *gorm.DB) error {
		result := db.Exec(query, args...)
		if result.Error != nil {
			return cmodel.ConvertGormErrors(ctx, result, true)
		}
		if result.RowsAffected == 0 {
			return cmodel.ErrOptimisticLock
		}
		return nil
	})
	if err != nil {
		return err
	}

	g.Version = newVersion
	return nil
}

func (dbc *dbAdapter) DeleteGeofence(ctx context.Context, agencyID, id, userID uuid.UUID) error {
	entity, err := dbc.FindByID(ctx, agencyID, id)
	if err != nil {
		return err
	}
	entity.Status = model.GeofenceStatusDeleted
	entity.UpdatedBy = &userID
	return dbc.UpdateGeofence(ctx, entity, "status", "updated_by")
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func uuidBytes(id uuid.UUID) []byte {
	b := id
	return b[:]
}

func uuidBytesPtr(id *uuid.UUID) []byte {
	if id == nil {
		return nil
	}
	b := *id
	return b[:]
}

// FindContainingPoint returns active geofences (scoped to agencyID, and
// optionally clientID) whose geometry contains the given point, using
// SDO_RELATE against the spatial index created in
// internal/db/initial/oracle.CreateSpatialIndex. This is the standard,
// documented Oracle Spatial point-in-polygon/circle query pattern; validate
// against your Oracle Spatial version during integration testing.
//
// The geo_json column (not geometry) is selected, matching
// pkg/database/model.Geofence's write-only Geometry field.
func (dbc *dbAdapter) FindContainingPoint(ctx context.Context, agencyID uuid.UUID, clientID *uuid.UUID, point spatial.Point, srid int) ([]*model.Geofence, error) {
	entities := make([]*model.Geofence, 0)

	query := `SELECT g.id, g.created_at, g.updated_at, g.version,
	                 g.agency_id, g.client_id, g.name, g.type, g.geo_json, g.status,
	                 g.exclude_from_colocation, g.notes, g.created_by, g.updated_by
	          FROM geofence_rules g
	          WHERE g.agency_id = :agency_id
	            AND g.status = :active_status`
	args := []interface{}{agencyID, int(model.GeofenceStatusActive)}

	if clientID != nil {
		query += ` AND (g.client_id = :client_id OR g.client_id IS NULL)`
		args = append(args, *clientID)
	}

	query += ` AND SDO_RELATE(g.geometry, MDSYS.SDO_GEOMETRY(2001, :srid, MDSYS.SDO_POINT_TYPE(:lon, :lat, NULL), NULL, NULL), 'mask=ANYINTERACT') = 'TRUE'`
	args = append(args, srid, point[0], point[1])

	if err := dbc.Raw(ctx, &entities, query, args...); err != nil {
		return nil, err
	}

	return entities, nil
}
