// Package spatial builds Oracle MDSYS.SDO_GEOMETRY values from GeoJSON-style
// primitives (points, rings, polygons, circles) for INSERT/UPDATE, and
// deliberately does *not* attempt to reconstruct GeoJSON back out of
// SDO_GEOMETRY on read. See pkg/database/model.Geofence for the rationale:
// the geo_json column is the canonical, lossless source of truth for API
// responses; SDO_GEOMETRY exists purely to drive the spatial index and
// spatial predicate queries (see pkg/database.GeofenceDbAdapter.FindContainingPoint).
//
// The SDO_GTYPE / SDO_ELEM_INFO_ARRAY / SDO_ORDINATE_ARRAY encoding here
// follows Oracle Spatial's documented, stable object-type layout, but has
// not been exercised against a live Oracle Spatial instance in this
// environment — validate against your Oracle version during integration
// testing.
package spatial

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// DefaultSRID is the EPSG WGS84 geodetic coordinate system (longitude/latitude
// in degrees, SDO_GTYPE dimensionality 2) registered for GEOFENCE.GEOMETRY in
// USER_SDO_GEOM_METADATA. Must stay in sync with that metadata.
const DefaultSRID = 4326

// earthRadiusMeters is the mean Earth radius (WGS84) used by the spherical
// direct-geodesic formula in destinationPoint.
const earthRadiusMeters = 6371000.0

// Point is a [longitude, latitude] pair in degrees.
type Point [2]float64

// Ring is a sequence of [longitude, latitude] points describing one boundary
// of a polygon. It does not need to be pre-closed or pre-oriented — see
// NewPolygonGeometry.
type Ring []Point

// Geometry is a write-only Oracle MDSYS.SDO_GEOMETRY value: it knows how to
// render itself as SDO_GEOMETRY constructor SQL via GormValue for
// INSERT/UPDATE, but is never scanned back out of a query result (the
// pkg/database/model.Geofence.Geometry field is tagged `->:false`).
type Geometry struct {
	GType     int
	SRID      int
	ElemInfo  []int
	Ordinates []float64
}

func (Geometry) GormDataType() string {
	return "MDSYS.SDO_GEOMETRY"
}

func (Geometry) GormDBDataType(*gorm.DB, *schema.Field) string {
	return "MDSYS.SDO_GEOMETRY"
}

// GormValue renders the geometry as an SDO_GEOMETRY(...) constructor
// expression. The element-info/ordinate arrays are inlined as numeric SQL
// literals rather than bound driver parameters, since the Oracle driver has
// no native support for binding SDO_ELEM_INFO_ARRAY/SDO_ORDINATE_ARRAY
// (VARRAY) values. Inlining is safe: every literal is a formatted int/float,
// never user-controlled text.
//
// NOTE: cmmoran/gorm-oracle's Create/Update callbacks (create.go/update.go)
// never check GormValuerInterface — confirmed empirically against a live
// Oracle instance, where using this via a normal .Create()/.Updates() call
// raised go-ora's "call register type before use user defined type (UDT)"
// error instead of invoking GormValue at all. Writes go through InlineSQL
// via hand-written SQL instead (see geofence_adapter.go); GormValue is kept
// for spec-correctness/portability and is exercised directly by this
// package's tests.
func (g Geometry) GormValue(_ context.Context, _ *gorm.DB) clause.Expr {
	if len(g.Ordinates) == 0 {
		return clause.Expr{SQL: "NULL"}
	}

	return clause.Expr{
		SQL: fmt.Sprintf(
			"MDSYS.SDO_GEOMETRY(?, ?, NULL, MDSYS.SDO_ELEM_INFO_ARRAY(%s), MDSYS.SDO_ORDINATE_ARRAY(%s))",
			joinInts(g.ElemInfo), joinFloats(g.Ordinates),
		),
		Vars: []interface{}{g.GType, g.SRID},
	}
}

// InlineSQL renders the full SDO_GEOMETRY(...) constructor with every
// argument — including GType/SRID — inlined as a literal, for hand-written
// raw SQL (see GormValue's doc comment for why this exists instead of just
// using GormValue everywhere).
func (g Geometry) InlineSQL() string {
	if len(g.Ordinates) == 0 {
		return "NULL"
	}
	return fmt.Sprintf(
		"MDSYS.SDO_GEOMETRY(%d, %d, NULL, MDSYS.SDO_ELEM_INFO_ARRAY(%s), MDSYS.SDO_ORDINATE_ARRAY(%s))",
		g.GType, g.SRID, joinInts(g.ElemInfo), joinFloats(g.Ordinates),
	)
}

func joinInts(vs []int) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

func joinFloats(vs []float64) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strings.Join(parts, ",")
}

// MinCircleRadiusMeters and MaxCircleRadiusMeters bound every circle
// geofence in this service (not just Known Locations) — 10m to 10km,
// matching the product requirement's validation range.
const (
	MinCircleRadiusMeters = 10
	MaxCircleRadiusMeters = 10000
)

// CircleSegments is the number of vertices used to approximate a circle as
// a regular polygon (see NewCircleGeometry) — 32 matches the common
// "quad_segs=8 per quadrant" default used by other geodetic GIS systems
// (e.g. PostGIS ST_Buffer).
const CircleSegments = 32

// NewCircleGeometry builds a circle as a densified regular polygon (32
// vertices at evenly-spaced bearings from the center, via the great-circle
// destination-point formula — center+radius(meters) can't be added directly
// to longitude/latitude degrees, since a degree of longitude shrinks with
// cos(latitude)).
//
// This is NOT a true circular arc: Oracle Spatial's arc element type
// (SDO_ELEM_INFO interpretation 2) is documented to be unsupported for
// geodetic data — confirmed empirically against a live Oracle instance,
// where storing a 3-point arc geometry raised ORA-13035 ("Invalid data
// (arcs in geodetic data)") the moment the spatial index tried to index it.
// A many-sided polygon approximation is the standard workaround used by
// geodetic GIS systems generally (e.g. PostGIS's ST_Buffer), and reuses
// NewPolygonGeometry's closing/orientation logic directly.
func NewCircleGeometry(center Point, radiusMeters float64, srid int) (Geometry, error) {
	if radiusMeters < MinCircleRadiusMeters || radiusMeters > MaxCircleRadiusMeters {
		return Geometry{}, fmt.Errorf("radius must be between %d and %d meters", MinCircleRadiusMeters, MaxCircleRadiusMeters)
	}
	if err := validatePoint(center); err != nil {
		return Geometry{}, err
	}

	ring := make(Ring, CircleSegments)
	for i := 0; i < CircleSegments; i++ {
		bearing := float64(i) * 360.0 / float64(CircleSegments)
		ring[i] = destinationPoint(center, bearing, radiusMeters)
	}

	return NewPolygonGeometry([]Ring{ring}, srid)
}

// destinationPoint returns the point reached by traveling distanceMeters
// from start along the given initial bearing (degrees clockwise from north),
// using the spherical direct-geodesic formula.
func destinationPoint(start Point, bearingDeg, distanceMeters float64) Point {
	lon1 := start[0] * math.Pi / 180
	lat1 := start[1] * math.Pi / 180
	bearing := bearingDeg * math.Pi / 180
	angularDistance := distanceMeters / earthRadiusMeters

	lat2 := math.Asin(math.Sin(lat1)*math.Cos(angularDistance) + math.Cos(lat1)*math.Sin(angularDistance)*math.Cos(bearing))
	lon2 := lon1 + math.Atan2(
		math.Sin(bearing)*math.Sin(angularDistance)*math.Cos(lat1),
		math.Cos(angularDistance)-math.Sin(lat1)*math.Sin(lat2),
	)

	lonDeg := lon2 * 180 / math.Pi
	lonDeg = math.Mod(lonDeg+540, 360) - 180 // normalize to [-180,180)

	return Point{lonDeg, lat2 * 180 / math.Pi}
}

// NewPolygonGeometry builds a single polygon. rings[0] is the exterior
// boundary; any subsequent rings are holes. Each ring is closed
// automatically (first point repeated as last, if not already) and
// re-oriented to Oracle Spatial's required winding order — exterior
// clockwise, holes counterclockwise, the opposite of GeoJSON/RFC 7946.
// Orientation is computed from the ring's own geometry (shoelace formula)
// rather than assumed from the input, since not all GeoJSON producers follow
// RFC 7946 winding strictly.
func NewPolygonGeometry(rings []Ring, srid int) (Geometry, error) {
	if len(rings) == 0 {
		return Geometry{}, errors.New("polygon requires at least one (exterior) ring")
	}

	elemInfo, ordinates, err := encodeRingsFromOffset(rings, 1)
	if err != nil {
		return Geometry{}, err
	}

	return Geometry{GType: 2003, SRID: srid, ElemInfo: elemInfo, Ordinates: ordinates}, nil
}

// NewRectangleGeometry builds an axis-aligned rectangle from its southwest
// and northeast corners. Oracle Spatial has no dedicated "rectangle"
// element type — an axis-aligned box is encoded as an ordinary single-ring
// polygon (SDO_GTYPE 2003), sharing NewPolygonGeometry's closing/orientation
// logic. Rectangles that would cross the antimeridian (sw longitude >= ne
// longitude) are rejected rather than silently built wrong.
func NewRectangleGeometry(sw, ne Point, srid int) (Geometry, error) {
	if err := validatePoint(sw); err != nil {
		return Geometry{}, err
	}
	if err := validatePoint(ne); err != nil {
		return Geometry{}, err
	}
	if sw[0] >= ne[0] {
		return Geometry{}, fmt.Errorf("rectangle: southwest longitude (%f) must be less than northeast longitude (%f); antimeridian-crossing rectangles are not supported", sw[0], ne[0])
	}
	if sw[1] >= ne[1] {
		return Geometry{}, fmt.Errorf("rectangle: southwest latitude (%f) must be less than northeast latitude (%f)", sw[1], ne[1])
	}

	ring := Ring{
		sw,
		{ne[0], sw[1]},
		ne,
		{sw[0], ne[1]},
		sw,
	}

	return NewPolygonGeometry([]Ring{ring}, srid)
}

func encodeRingsFromOffset(rings []Ring, startOffset int) ([]int, []float64, error) {
	var (
		elemInfo  []int
		ordinates []float64
		offset    = startOffset
	)
	for i, ring := range rings {
		if len(ring) < 3 {
			return nil, nil, fmt.Errorf("ring %d requires at least 3 distinct points", i)
		}
		for _, pt := range ring {
			if err := validatePoint(pt); err != nil {
				return nil, nil, err
			}
		}

		isExterior := i == 0
		oriented := ensureOrientation(closeRing(ring), !isExterior) // exterior clockwise, holes counterclockwise

		etype := 1003
		if !isExterior {
			etype = 2003
		}
		elemInfo = append(elemInfo, offset, etype, 1)

		for _, pt := range oriented {
			ordinates = append(ordinates, pt[0], pt[1])
		}
		offset += len(oriented) * 2
	}

	return elemInfo, ordinates, nil
}

func closeRing(ring Ring) Ring {
	if len(ring) == 0 {
		return ring
	}
	first, last := ring[0], ring[len(ring)-1]
	if first[0] == last[0] && first[1] == last[1] {
		return ring
	}
	closed := make(Ring, len(ring)+1)
	copy(closed, ring)
	closed[len(ring)] = first
	return closed
}

// ensureOrientation reverses ring if its signed area doesn't already match
// the requested winding.
func ensureOrientation(ring Ring, wantCounterClockwise bool) Ring {
	if (signedArea(ring) > 0) == wantCounterClockwise {
		return ring
	}
	reversed := make(Ring, len(ring))
	for i, pt := range ring {
		reversed[len(ring)-1-i] = pt
	}
	return reversed
}

// signedArea computes twice the signed area of ring via the shoelace
// formula; positive means counterclockwise winding under standard
// x-increases-right, y-increases-up axes, which longitude/latitude follow.
func signedArea(ring Ring) float64 {
	var sum float64
	for i := 0; i < len(ring)-1; i++ {
		sum += ring[i][0]*ring[i+1][1] - ring[i+1][0]*ring[i][1]
	}
	return sum
}

// NewPointGeometry builds a simple 2D point geometry, used for spatial
// point-lookup queries (SDO_RELATE against stored geofence geometries) —
// not for persisted geofences.
func NewPointGeometry(p Point, srid int) (Geometry, error) {
	if err := validatePoint(p); err != nil {
		return Geometry{}, err
	}
	return Geometry{GType: 2001, SRID: srid, ElemInfo: []int{1, 1, 1}, Ordinates: []float64{p[0], p[1]}}, nil
}

func validatePoint(p Point) error {
	if p[0] < -180 || p[0] > 180 {
		return fmt.Errorf("longitude %f out of range [-180,180]", p[0])
	}
	if p[1] < -90 || p[1] > 90 {
		return fmt.Errorf("latitude %f out of range [-90,90]", p[1])
	}
	return nil
}
