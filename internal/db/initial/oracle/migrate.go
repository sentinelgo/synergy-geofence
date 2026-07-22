// Package oracle holds the frozen "genesis" schema snapshot used by the
// initial migration. It intentionally does NOT import the live
// pkg/database/model package: migrations must stay stable even as the live
// model evolves, so each migration (including this initial one) defines its
// own minimal, frozen struct copies.
package oracle

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentinelgo/synergy-geofence/internal/database/spatial"
	"gorm.io/gorm"
)

type MutableModel struct {
	ID        uuid.UUID  `gorm:"<-:create;primaryKey;type:uuid" json:"id"`
	CreatedAt time.Time  `gorm:"<-:create;autoCreateTime;not null" json:"created_at,omitempty"`
	UpdatedAt *time.Time `gorm:"autoUpdateTime;not null" json:"updated_at,omitempty"`
	Version   uint64     `gorm:"not null;version" json:"version"`
}

// Geofence is the frozen genesis definition of the `geofence_rules` table,
// matching the Core DDL:
//
//	CREATE TABLE geofence_rules (
//	    id            RAW(16)            DEFAULT PRIMARY KEY,
//	    agency_id     RAW(16)            NOT NULL,
//	    client_id     RAW(16),
//	    name          VARCHAR2(150)      NOT NULL,
//	    type          NUMERIC            NOT NULL CHECK (type IN (1,2,3)),
//	    geometry      MDSYS.SDO_GEOMETRY NOT NULL,
//	    status        NUMERIC            DEFAULT 1 CHECK (status IN (1,2,3,4)),
//	    created_at    TIMESTAMP WITHOUT TIME ZONE DEFAULT SYSTIMESTAMP,
//	    updated_at    TIMESTAMP WITHOUT TIME ZONE DEFAULT SYSTIMESTAMP,
//	    version       NUMERIC,
//	    created_by    RAW(16)            NOT NULL
//	);
//
// type codes: 1=Circle, 2=Polygon, 3=Rectangle (pkg/database/model.GeofenceType).
// status codes: 1=active, 2=inactive, 3=archived, 4=deleted
// (pkg/database/model.GeofenceStatus). Both are numeric-coded enums —
// mirroring sso-agency-service's AgencyStatus/AgencyType convention — and
// the API layer never sees the raw codes; pkg/handler/model translates
// to/from friendly strings at the request/response boundary.
//
// plus additions beyond the given DDL:
//   - geo_json CLOB NOT NULL — stores the canonical GeoJSON alongside the
//     spatial column so API reads are lossless without reverse-engineering
//     Oracle's internal SDO_GEOMETRY encoding (see pkg/database/model.Geofence's
//     doc comment).
//   - exclude_from_colocation NUMBER(1) NOT NULL DEFAULT 0, notes VARCHAR2(500) —
//     back the "Known Locations" feature (circle geofences excluded from
//     co-location detection).
//   - updated_by RAW(16) — actor tracking for the Known Locations audit
//     table, alongside created_by.
//   - a unique index on (agency_id, name) — location names must be unique
//     within an agency.
//
// This is still the pre-release genesis schema (no deployed data), so these
// additions are folded directly into the initial migration rather than
// tracked as an incremental m0000XX migration.
type Geofence struct {
	MutableModel

	AgencyID uuid.UUID        `gorm:"column:agency_id;type:uuid;not null;index:idx_geofence_agency_client,priority:1;uniqueIndex:uni_geofence_agency_name,priority:1"`
	ClientID *uuid.UUID       `gorm:"column:client_id;type:uuid;index:idx_geofence_agency_client,priority:2"`
	Name     string           `gorm:"column:name;type:varchar2;size:150;not null;uniqueIndex:uni_geofence_agency_name,priority:2"`
	Type     int              `gorm:"column:type;type:numeric;not null;check:chk_geofence_type,type IN (1,2,3)"`
	Geometry spatial.Geometry `gorm:"column:geometry;type:MDSYS.SDO_GEOMETRY;not null;->:false;<-:create,update"`
	GeoJSON  string           `gorm:"column:geo_json;type:clob;not null"`
	Status   int              `gorm:"column:status;type:numeric;not null;default:1;index:idx_geofence_agency_client,priority:3;check:chk_geofence_status,status IN (1,2,3,4)"`

	ExcludeFromColocation bool    `gorm:"column:exclude_from_colocation;type:numeric(1);not null;default:0"`
	Notes                 *string `gorm:"column:notes;type:varchar2;size:500"`

	CreatedBy uuid.UUID  `gorm:"column:created_by;type:uuid;<-:create;not null"`
	UpdatedBy *uuid.UUID `gorm:"column:updated_by;type:uuid"`
}

func (Geofence) TableName() string {
	return "geofence_rules"
}

// CreateSpatialIndex registers the geometry column's dimensional metadata
// (a mandatory prerequisite for Oracle Spatial) and creates the spatial
// index backing FindContainingPoint's SDO_RELATE query.
//
// SRID 4326 (spatial.DefaultSRID) matches the SRID used when constructing
// SDO_GEOMETRY values in pkg/database/spatial — these must stay in sync.
// This has not been validated against a live Oracle Spatial instance;
// validate during integration testing.
func CreateSpatialIndex(tx *gorm.DB) error {
	stmts := []string{
		`DELETE FROM USER_SDO_GEOM_METADATA WHERE TABLE_NAME = 'GEOFENCE_RULES' AND COLUMN_NAME = 'GEOMETRY'`,
		fmt.Sprintf(`INSERT INTO USER_SDO_GEOM_METADATA (TABLE_NAME, COLUMN_NAME, DIMINFO, SRID)
VALUES ('GEOFENCE_RULES', 'GEOMETRY',
  MDSYS.SDO_DIM_ARRAY(
    MDSYS.SDO_DIM_ELEMENT('LONGITUDE', -180, 180, 0.005),
    MDSYS.SDO_DIM_ELEMENT('LATITUDE', -90, 90, 0.005)
  ), %d)`, spatial.DefaultSRID),
		`CREATE INDEX idx_geofence_spatial ON geofence_rules(geometry) INDEXTYPE IS MDSYS.SPATIAL_INDEX`,
	}

	for _, stmt := range stmts {
		if err := tx.Exec(stmt).Error; err != nil {
			return err
		}
	}

	return nil
}
