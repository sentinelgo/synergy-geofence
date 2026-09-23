// Package model holds the API-facing DTOs matching
// pkg/swagger/geofence-openapi.yaml, and the conversions to/from the GORM
// model in pkg/database/model.
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/database/spatial"
)

// ErrInvalidNameCharset is wrapped by ToModel when a name contains
// characters outside nameCharsetPattern, so callers can distinguish it
// (errors.Is) from geometry-validation failures and respond with a more
// specific error code.
var ErrInvalidNameCharset = errors.New("name contains disallowed characters")

// nameCharsetPattern allows letters, digits, spaces, hyphens, and
// apostrophes only, per the Known Locations name validation requirement.
// Length (2-100 chars) is enforced separately via GeofenceRequest's binding
// tag.
var nameCharsetPattern = regexp.MustCompile(`^[A-Za-z0-9 '-]+$`)

func validateNameCharset(name string) error {
	if !nameCharsetPattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidNameCharset, name)
	}
	return nil
}

// StatusToDB translates the API's friendly status string into the
// numeric-coded dbmodel.GeofenceStatus stored in the database, via the
// enumer-generated GeofenceStatusString parser. The API never sees the raw
// codes; GeofenceRequest.Status's `oneof=...` binding tag already restricts
// input to these four values. An unrecognized string maps to
// GeofenceStatusUnknown, which callers reject via !IsAGeofenceStatus().
func StatusToDB(s string) dbmodel.GeofenceStatus {
	status, err := dbmodel.GeofenceStatusString(s)
	if err != nil {
		return dbmodel.GeofenceStatusUnknown
	}
	return status
}

// Address mirrors components.schemas.Address, a structured physical address
// attached to a geometry.
type Address struct {
	Name              string `json:"name,omitempty"`
	Street1           string `json:"street1,omitempty"`
	Street2           string `json:"street2,omitempty"`
	City              string `json:"city,omitempty"`
	StateProvinceID   int    `json:"state_province_id,omitempty"`
	StateProvince     string `json:"state_province,omitempty"`
	StateProvinceName string `json:"state_province_name,omitempty"`
	PostalCode        string `json:"postal_code,omitempty"`
	Country           string `json:"country,omitempty"`
}

// GeoJsonPoint mirrors components.schemas.GeoJsonPoint.
type GeoJsonPoint struct {
	Type        string        `json:"type"`
	Coordinates spatial.Point `json:"coordinates"`
	Address     *Address      `json:"address,omitempty"`
}

// GeoJsonPolygon mirrors components.schemas.GeoJsonPolygon.
type GeoJsonPolygon struct {
	Type        string         `json:"type"`
	Coordinates []spatial.Ring `json:"coordinates"`
	Address     *Address       `json:"address,omitempty"`
}

// GeoJsonRectangle extends the OpenAPI GeoJson oneOf (absent from the
// original fragment) so it lines up with the `type` CHECK constraint on the
// geofence table, which allows 'Rectangle'. Coordinates is exactly two
// points: [southwest, northeast], describing an axis-aligned box — the
// simplest input shape for a map-drawn rectangle. See
// pkg/swagger/geofence-openapi.yaml.
type GeoJsonRectangle struct {
	Type        string           `json:"type"`
	Coordinates [2]spatial.Point `json:"coordinates"`
	Address     *Address         `json:"address,omitempty"`
}

// GeoJsonCircle mirrors components.schemas.GeoJsonCircle.
type GeoJsonCircle struct {
	Type        string        `json:"type"`
	Coordinates spatial.Point `json:"coordinates"`
	Radius      float64       `json:"radius"`
	Address     *Address      `json:"address,omitempty"`
}

// GeoJson is a hand-rolled discriminated union over the `type` field,
// mirroring components.schemas.GeoJson's oneOf/discriminator. Exactly one of
// the pointer fields is populated after UnmarshalJSON.
type GeoJson struct {
	Point     *GeoJsonPoint
	Polygon   *GeoJsonPolygon
	Rectangle *GeoJsonRectangle
	Circle    *GeoJsonCircle
}

func (g *GeoJson) UnmarshalJSON(data []byte) error {
	var disc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &disc); err != nil {
		return err
	}

	switch disc.Type {
	case "Point":
		v := &GeoJsonPoint{}
		if err := json.Unmarshal(data, v); err != nil {
			return err
		}
		g.Point = v
	case "Polygon":
		v := &GeoJsonPolygon{}
		if err := json.Unmarshal(data, v); err != nil {
			return err
		}
		g.Polygon = v
	case "Rectangle":
		v := &GeoJsonRectangle{}
		if err := json.Unmarshal(data, v); err != nil {
			return err
		}
		g.Rectangle = v
	case "Circle":
		v := &GeoJsonCircle{}
		if err := json.Unmarshal(data, v); err != nil {
			return err
		}
		g.Circle = v
	default:
		return fmt.Errorf("geo_json: unsupported type %q", disc.Type)
	}

	return nil
}

func (g GeoJson) MarshalJSON() ([]byte, error) {
	switch {
	case g.Point != nil:
		return json.Marshal(g.Point)
	case g.Polygon != nil:
		return json.Marshal(g.Polygon)
	case g.Rectangle != nil:
		return json.Marshal(g.Rectangle)
	case g.Circle != nil:
		return json.Marshal(g.Circle)
	default:
		return nil, fmt.Errorf("geo_json: empty union")
	}
}

// Type returns the discriminator value of whichever variant is populated,
// or "" if none is.
func (g GeoJson) Type() string {
	switch {
	case g.Point != nil:
		return "Point"
	case g.Polygon != nil:
		return "Polygon"
	case g.Rectangle != nil:
		return "Rectangle"
	case g.Circle != nil:
		return "Circle"
	default:
		return ""
	}
}

// GeofenceRequest mirrors components.schemas.GeofenceRequest, plus
// client_id, exclude_from_colocation and notes additions (absent from the
// original fragment) needed to support the DDL's agency-wide-vs-client-
// specific scoping and the Known Locations feature.
type GeofenceRequest struct {
	Name                  string     `json:"name" binding:"required,min=2,max=150"`
	GeoJson               GeoJson    `json:"geo_json" binding:"required"`
	Status                string     `json:"status" binding:"required,oneof=active inactive archived deleted"`
	UserID                uuid.UUID  `json:"user_id" binding:"required"`
	ClientID              *uuid.UUID `json:"client_id,omitempty"`
	ExcludeFromColocation bool       `json:"exclude_from_colocation"`
	Notes                 *string    `json:"notes,omitempty" binding:"omitempty,max=500"`
}

// PatchGeofenceRequest is a partial update for PATCH: every business field
// is optional, and a field absent from the request body leaves the
// existing value untouched (JSON Merge Patch-style — RFC 7396). ClientID
// and Notes are nullable, so "absent" and "explicit null" are ambiguous on
// a plain pointer; clientIDSet/notesSet (populated by UnmarshalJSON) tell
// ApplyTo whether the key was present at all, so it can tell "leave
// unchanged" apart from "clear it".
//
// UserID and Version stay required: UserID identifies who's performing
// *this* update (not a resource field, so there's no "existing value" to
// fall back to) and Version drives the optimistic-concurrency check.
type PatchGeofenceRequest struct {
	Name                  *string    `json:"name,omitempty" binding:"omitempty,min=2,max=150"`
	GeoJson               *GeoJson   `json:"geo_json,omitempty"`
	Status                *string    `json:"status,omitempty" binding:"omitempty,oneof=active inactive archived deleted"`
	UserID                uuid.UUID  `json:"user_id" binding:"required"`
	ClientID              *uuid.UUID `json:"client_id,omitempty"`
	ExcludeFromColocation *bool      `json:"exclude_from_colocation,omitempty"`
	Notes                 *string    `json:"notes,omitempty" binding:"omitempty,max=500"`
	Version               uint64     `json:"version" binding:"required"`

	clientIDSet bool
	notesSet    bool
}

func (r *PatchGeofenceRequest) UnmarshalJSON(data []byte) error {
	type alias PatchGeofenceRequest
	if err := json.Unmarshal(data, (*alias)(r)); err != nil {
		return err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, r.clientIDSet = raw["client_id"]
	_, r.notesSet = raw["notes"]
	return nil
}

// ApplyTo merges the patch onto an already-fetched entity, mutating only
// the fields present in the request, and returns the column names that
// changed for GeofenceDbAdapter.UpdateGeofence's selectFields — so the
// UPDATE statement leaves every untouched column, including geometry/
// geo_json, alone.
func (r *PatchGeofenceRequest) ApplyTo(entity *dbmodel.Geofence, srid int) ([]any, error) {
	var selectFields []any

	if r.Name != nil {
		if err := validateNameCharset(*r.Name); err != nil {
			return nil, err
		}
		entity.Name = *r.Name
		selectFields = append(selectFields, "name")
	}

	if r.GeoJson != nil {
		geoJSONBytes, err := json.Marshal(*r.GeoJson)
		if err != nil {
			return nil, fmt.Errorf("marshal geo_json: %w", err)
		}
		geofenceType, geom, err := buildGeometry(*r.GeoJson, srid)
		if err != nil {
			return nil, err
		}
		entity.GeoJSON = string(geoJSONBytes)
		entity.Type = geofenceType
		entity.Geometry = geom
		selectFields = append(selectFields, "geo_json", "type", "geometry")
	}

	if r.Status != nil {
		entity.Status = StatusToDB(*r.Status)
		selectFields = append(selectFields, "status")
	}

	if r.clientIDSet {
		entity.ClientID = r.ClientID
		selectFields = append(selectFields, "client_id")
	}

	if r.ExcludeFromColocation != nil {
		entity.ExcludeFromColocation = *r.ExcludeFromColocation
		selectFields = append(selectFields, "exclude_from_colocation")
	}

	if r.notesSet {
		entity.Notes = r.Notes
		selectFields = append(selectFields, "notes")
	}

	return selectFields, nil
}

// Geofence mirrors components.schemas.Geofence (GeofenceRequest + id/version/agency_id).
type Geofence struct {
	ID                    uuid.UUID  `json:"id"`
	AgencyID              uuid.UUID  `json:"agency_id"`
	ClientID              *uuid.UUID `json:"client_id,omitempty"`
	Name                  string     `json:"name"`
	GeoJson               GeoJson    `json:"geo_json"`
	Status                string     `json:"status"`
	UserID                uuid.UUID  `json:"user_id"`
	UpdatedBy             *uuid.UUID `json:"updated_by,omitempty"`
	ExcludeFromColocation bool       `json:"exclude_from_colocation"`
	Notes                 *string    `json:"notes,omitempty"`
	Version               uint64     `json:"version"`
}

// ToModel converts a validated create/update request into the persistence
// model, building both the canonical geo_json column and the write-only
// spatial.Geometry used for indexing/querying. Only Circle, Polygon and
// Rectangle geo_json types are valid geofence shapes (per the `type`
// CHECK constraint) — Point is rejected.
func (r *GeofenceRequest) ToModel(agencyID uuid.UUID, srid int) (*dbmodel.Geofence, error) {
	if err := validateNameCharset(r.Name); err != nil {
		return nil, err
	}

	geoJSONBytes, err := json.Marshal(r.GeoJson)
	if err != nil {
		return nil, fmt.Errorf("marshal geo_json: %w", err)
	}

	geofenceType, geom, err := buildGeometry(r.GeoJson, srid)
	if err != nil {
		return nil, err
	}

	entity := dbmodel.NewGeofence()
	entity.AgencyID = agencyID
	entity.ClientID = r.ClientID
	entity.Name = r.Name
	entity.Status = StatusToDB(r.Status)
	entity.CreatedBy = r.UserID
	entity.GeoJSON = string(geoJSONBytes)
	entity.ExcludeFromColocation = r.ExcludeFromColocation
	entity.Notes = r.Notes
	entity.Type = geofenceType
	entity.Geometry = geom

	return entity, nil
}

// buildGeometry constructs the write-only spatial.Geometry (and its
// corresponding GeofenceType) from a geo_json value. Only Circle, Polygon
// and Rectangle are valid geofence shapes (per the `type` CHECK constraint)
// — Point is rejected.
func buildGeometry(g GeoJson, srid int) (dbmodel.GeofenceType, spatial.Geometry, error) {
	switch {
	case g.Circle != nil:
		geom, err := spatial.NewCircleGeometry(g.Circle.Coordinates, g.Circle.Radius, srid)
		if err != nil {
			return 0, spatial.Geometry{}, err
		}
		return dbmodel.GeofenceTypeCircle, geom, nil
	case g.Polygon != nil:
		geom, err := spatial.NewPolygonGeometry(g.Polygon.Coordinates, srid)
		if err != nil {
			return 0, spatial.Geometry{}, err
		}
		return dbmodel.GeofenceTypePolygon, geom, nil
	case g.Rectangle != nil:
		corners := g.Rectangle.Coordinates
		geom, err := spatial.NewRectangleGeometry(corners[0], corners[1], srid)
		if err != nil {
			return 0, spatial.Geometry{}, err
		}
		return dbmodel.GeofenceTypeRectangle, geom, nil
	default:
		return 0, spatial.Geometry{}, fmt.Errorf("geo_json type %q is not a valid geofence shape (must be Circle, Polygon or Rectangle)", g.Type())
	}
}

// FromModel converts a persisted geofence back into its API representation,
// unmarshaling the canonical geo_json column.
func FromModel(m *dbmodel.Geofence) (*Geofence, error) {
	var geoJSON GeoJson
	if err := json.Unmarshal([]byte(m.GeoJSON), &geoJSON); err != nil {
		return nil, fmt.Errorf("unmarshal geo_json: %w", err)
	}

	return &Geofence{
		ID:                    m.ID,
		AgencyID:              m.AgencyID,
		ClientID:              m.ClientID,
		Name:                  m.Name,
		GeoJson:               geoJSON,
		Status:                m.Status.String(),
		UserID:                m.CreatedBy,
		UpdatedBy:             m.UpdatedBy,
		ExcludeFromColocation: m.ExcludeFromColocation,
		Notes:                 m.Notes,
		Version:               m.Version,
	}, nil
}

// DiffChangedFields compares old against updated and returns a map of field
// name -> new value for every mutable business field that differs, for an
// UPDATED event's "changed fields only" payload. geo_json and type are
// reported together, since a geometry change is only observable as a
// geo_json change (the geometry column itself is write-only — see
// pkg/database/model.Geofence's doc comment).
func DiffChangedFields(old, updated *dbmodel.Geofence) map[string]any {
	changed := make(map[string]any)

	if old.Name != updated.Name {
		changed["name"] = updated.Name
	}
	if old.Status != updated.Status {
		changed["status"] = updated.Status.String()
	}
	if !uuidPtrEqual(old.ClientID, updated.ClientID) {
		changed["client_id"] = updated.ClientID
	}
	if old.GeoJSON != updated.GeoJSON {
		changed["type"] = updated.Type.String()
		changed["geo_json"] = json.RawMessage(updated.GeoJSON)
	}
	if old.ExcludeFromColocation != updated.ExcludeFromColocation {
		changed["exclude_from_colocation"] = updated.ExcludeFromColocation
	}
	if !stringPtrEqual(old.Notes, updated.Notes) {
		changed["notes"] = updated.Notes
	}

	return changed
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func uuidPtrEqual(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
