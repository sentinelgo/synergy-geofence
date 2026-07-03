//go:generate go tool enumer -type GeofenceType -trimprefix GeofenceType -json -text -sql:int geofence.go
//go:generate go tool enumer -type GeofenceStatus -trimprefix GeofenceStatus -transform lower -json -text -sql:int geofence.go
package model

import (
	"github.com/google/uuid"
	cmodel "github.com/sentinelgo/synergy-common/pkg/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/database/spatial"
)

const (
	AgencyIDColumn = "agency_id"
	ClientIDColumn = "client_id"
	StatusColumn   = "status"
)

// GeofenceType mirrors the `type` CHECK constraint on the geofence table —
// a numeric-coded enum stored in a NUMERIC column, following the same
// convention as sso-agency-service's AgencyStatus/AgencyType. The API layer
// never sees these codes directly; pkg/handler/model translates to/from the
// GeoJSON type discriminator ("Circle"/"Polygon"/"Rectangle").
type GeofenceType int

const (
	GeofenceTypeUnknown GeofenceType = iota
	GeofenceTypeCircle
	GeofenceTypePolygon
	GeofenceTypeRectangle
)

// String, MarshalJSON/UnmarshalJSON, MarshalText/UnmarshalText, Value/Scan,
// GeofenceTypeString, and IsAGeofenceType are generated — see
// geofencetype_enumer.go (go:generate directive above).
//
// Valid is hand-written rather than reusing the generated IsAGeofenceType:
// enumer's IsAGeofenceType treats every declared constant — including the
// zero-value sentinel GeofenceTypeUnknown — as "a legitimate member of the
// enum," which isn't the same question as "did the caller actually provide
// a real type."
func (t GeofenceType) Valid() bool {
	switch t {
	case GeofenceTypeCircle, GeofenceTypePolygon, GeofenceTypeRectangle:
		return true
	default:
		return false
	}
}

// GeofenceStatus mirrors the `status` CHECK constraint on the geofence
// table — a numeric-coded enum; see GeofenceType's doc comment. The API
// layer only ever sees the friendly strings ("active"/"inactive"/
// "archived"/"deleted"), translated by pkg/handler/model.
type GeofenceStatus int

const (
	GeofenceStatusUnknown GeofenceStatus = iota
	GeofenceStatusActive
	GeofenceStatusInactive
	GeofenceStatusArchived
	// GeofenceStatusDeleted is the terminal state set by DELETE — a second,
	// distinct soft-delete state from Archived (which a client can still set
	// directly via PUT for its own lifecycle purposes).
	GeofenceStatusDeleted
)

// String (lowercased: "active"/"inactive"/"archived"/"deleted"),
// MarshalJSON/UnmarshalJSON, MarshalText/UnmarshalText, Value/Scan,
// GeofenceStatusString, and IsAGeofenceStatus are generated — see
// geofencestatus_enumer.go (go:generate directive above).
//
// Valid is hand-written for the same reason as GeofenceType.Valid: the
// generated IsAGeofenceStatus treats the zero-value sentinel
// GeofenceStatusUnknown as a legitimate member too.
func (s GeofenceStatus) Valid() bool {
	switch s {
	case GeofenceStatusActive, GeofenceStatusInactive, GeofenceStatusArchived, GeofenceStatusDeleted:
		return true
	default:
		return false
	}
}

// Geofence is the GORM model for the `geofence` table.
//
// Geometry is populated on create/update via spatial.Geometry's GormValue
// (SDO_GEOMETRY constructor SQL) but is never read back (`->:false`) — the
// GeoJSON column is the canonical, lossless representation used to build API
// responses. Geometry exists solely to drive the Oracle Spatial index and
// spatial predicate queries (see GeofenceDbAdapter.FindContainingPoint).
type Geofence struct {
	MutableModel `mapstructure:",squash,omitempty"`

	AgencyID uuid.UUID        `json:"agency_id" gorm:"column:agency_id;type:uuid;not null;index:idx_geofence_agency_client,priority:1;uniqueIndex:uni_geofence_agency_name,priority:1" mapstructure:"agency_id"`
	ClientID *uuid.UUID       `json:"client_id,omitempty" gorm:"column:client_id;type:uuid;index:idx_geofence_agency_client,priority:2" mapstructure:"client_id"`
	Name     string           `json:"name" gorm:"column:name;type:varchar2;size:150;not null;uniqueIndex:uni_geofence_agency_name,priority:2" mapstructure:"name"`
	Type     GeofenceType     `json:"type" gorm:"column:type;type:numeric;not null;check:chk_geofence_type,type IN (1,2,3)" mapstructure:"type"`
	Geometry spatial.Geometry `json:"-" gorm:"column:geometry;type:MDSYS.SDO_GEOMETRY;not null;->:false;<-:create,update" mapstructure:"-"`
	GeoJSON  string           `json:"-" gorm:"column:geo_json;type:clob;not null" mapstructure:"-"`
	Status   GeofenceStatus   `json:"status" gorm:"column:status;type:numeric;not null;default:1;index:idx_geofence_agency_client,priority:3;check:chk_geofence_status,status IN (1,2,3,4)" mapstructure:"status"`

	// ExcludeFromColocation and Notes back the "Known Locations" feature:
	// circle geofences (parole offices, courthouses, etc.) that should be
	// excluded from co-location detection to reduce false positives. Both
	// are generic columns on the shared geofence table rather than a
	// separate "known location" entity, since there is no distinct resource
	// type today — any geofence can be flagged this way.
	ExcludeFromColocation bool    `json:"exclude_from_colocation" gorm:"column:exclude_from_colocation;type:numeric(1);not null;default:0" mapstructure:"exclude_from_colocation"`
	Notes                 *string `json:"notes,omitempty" gorm:"column:notes;type:varchar2;size:500" mapstructure:"notes"`

	CreatedBy uuid.UUID  `json:"created_by" gorm:"column:created_by;type:uuid;<-:create;not null" mapstructure:"created_by"`
	UpdatedBy *uuid.UUID `json:"updated_by,omitempty" gorm:"column:updated_by;type:uuid" mapstructure:"updated_by"`
}

func (t *Geofence) TableName() string {
	return "geofence"
}

func (t *Geofence) PrimaryKey() uuid.UUID {
	return t.ID
}

func (t *Geofence) ToOut() any {
	return cmodel.ToOut[uuid.UUID](t)
}

func (t *Geofence) ToIn(m any) error {
	return cmodel.ToIn[uuid.UUID](t, m)
}

func NewGeofence() *Geofence {
	return &Geofence{
		MutableModel: DefaultMutableModel(),
		Status:       GeofenceStatusActive,
	}
}
