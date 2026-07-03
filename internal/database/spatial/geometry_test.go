package spatial

import (
	"math"
	"testing"
)

func TestNewCircleGeometry_PointsLieOnCircle(t *testing.T) {
	center := Point{-122.4194, 37.7749} // San Francisco
	radius := 500.0

	geom, err := NewCircleGeometry(center, radius, DefaultSRID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if geom.GType != 2003 {
		t.Fatalf("expected GType 2003, got %d", geom.GType)
	}
	// Densified regular polygon (straight edges) — Oracle Spatial doesn't
	// support the arc element type for geodetic data (ORA-13035, confirmed
	// against a live instance). CircleSegments vertices + the closing repeat
	// of the first point.
	wantOrdinates := (CircleSegments + 1) * 2
	if len(geom.Ordinates) != wantOrdinates {
		t.Fatalf("expected %d ordinates (%d points), got %d", wantOrdinates, CircleSegments+1, len(geom.Ordinates))
	}
	wantElemInfo := []int{1, 1003, 1}
	if !intsEqual(geom.ElemInfo, wantElemInfo) {
		t.Fatalf("elem info = %v, want %v (straight-edge polygon, not an arc)", geom.ElemInfo, wantElemInfo)
	}

	for i := 0; i < CircleSegments; i++ {
		p := Point{geom.Ordinates[i*2], geom.Ordinates[i*2+1]}
		d := haversineMeters(center, p)
		if math.Abs(d-radius) > 1.0 { // within 1m tolerance
			t.Errorf("point %d is %fm from center, want %fm", i, d, radius)
		}
	}
}

func TestNewCircleGeometry_EnforcesRadiusBounds(t *testing.T) {
	cases := []struct {
		name    string
		radius  float64
		wantErr bool
	}{
		{"zero", 0, true},
		{"negative", -10, true},
		{"below minimum", MinCircleRadiusMeters - 1, true},
		{"at minimum", MinCircleRadiusMeters, false},
		{"at maximum", MaxCircleRadiusMeters, false},
		{"above maximum", MaxCircleRadiusMeters + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCircleGeometry(Point{0, 0}, tc.radius, DefaultSRID)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for radius %v", tc.radius)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for radius %v: %v", tc.radius, err)
			}
		})
	}
}

func TestNewPolygonGeometry_SingleRing(t *testing.T) {
	// A simple square, given in GeoJSON (RFC 7946) exterior winding: CCW.
	ring := Ring{{-1, -1}, {1, -1}, {1, 1}, {-1, 1}, {-1, -1}}

	geom, err := NewPolygonGeometry([]Ring{ring}, DefaultSRID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if geom.GType != 2003 {
		t.Fatalf("expected GType 2003, got %d", geom.GType)
	}
	wantElemInfo := []int{1, 1003, 1}
	if !intsEqual(geom.ElemInfo, wantElemInfo) {
		t.Fatalf("elem info = %v, want %v", geom.ElemInfo, wantElemInfo)
	}
	// Exterior boundary must end up clockwise for Oracle: verify the encoded
	// ring's signed area is negative (clockwise) even though the input was CCW.
	if signedAreaFromOrdinates(geom.Ordinates) > 0 {
		t.Fatal("expected exterior ring to be re-oriented clockwise for Oracle")
	}
}

func TestNewPolygonGeometry_ExteriorWithHole(t *testing.T) {
	exterior := Ring{{-10, -10}, {10, -10}, {10, 10}, {-10, 10}, {-10, -10}}
	hole := Ring{{-1, -1}, {-1, 1}, {1, 1}, {1, -1}, {-1, -1}}

	geom, err := NewPolygonGeometry([]Ring{exterior, hole}, DefaultSRID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Two elements: exterior (etype 1003) then hole (etype 2003).
	if len(geom.ElemInfo) != 6 {
		t.Fatalf("expected 2 elem-info triplets (6 ints), got %d: %v", len(geom.ElemInfo), geom.ElemInfo)
	}
	if geom.ElemInfo[0] != 1 || geom.ElemInfo[1] != 1003 {
		t.Fatalf("expected exterior triplet starting (1,1003,...), got %v", geom.ElemInfo[:3])
	}
	if geom.ElemInfo[4] != 2003 {
		t.Fatalf("expected hole etype 2003, got %v", geom.ElemInfo[3:6])
	}
	// The hole's offset must start right after the exterior ring's ordinates.
	exteriorPoints := 5 // closed ring: 4 distinct + repeat
	wantHoleOffset := 1 + exteriorPoints*2
	if geom.ElemInfo[3] != wantHoleOffset {
		t.Fatalf("hole offset = %d, want %d", geom.ElemInfo[3], wantHoleOffset)
	}
}

func TestNewPolygonGeometry_RejectsEmptyRings(t *testing.T) {
	if _, err := NewPolygonGeometry(nil, DefaultSRID); err == nil {
		t.Fatal("expected error for no rings")
	}
	if _, err := NewPolygonGeometry([]Ring{{{0, 0}, {1, 1}}}, DefaultSRID); err == nil {
		t.Fatal("expected error for a ring with fewer than 3 points")
	}
}

func TestNewRectangleGeometry_BuildsAxisAlignedBox(t *testing.T) {
	sw := Point{-122.43, 37.77}
	ne := Point{-122.41, 37.79}

	geom, err := NewRectangleGeometry(sw, ne, DefaultSRID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if geom.GType != 2003 {
		t.Fatalf("expected GType 2003, got %d", geom.GType)
	}
	wantElemInfo := []int{1, 1003, 1}
	if !intsEqual(geom.ElemInfo, wantElemInfo) {
		t.Fatalf("elem info = %v, want %v", geom.ElemInfo, wantElemInfo)
	}
	if len(geom.Ordinates) != 10 { // 4 corners + closing repeat
		t.Fatalf("expected 10 ordinates, got %d", len(geom.Ordinates))
	}

	// Every ordinate pair must be one of the 4 rectangle corners.
	corners := map[Point]bool{
		sw:             true,
		{ne[0], sw[1]}: true,
		ne:             true,
		{sw[0], ne[1]}: true,
	}
	for i := 0; i < len(geom.Ordinates); i += 2 {
		p := Point{geom.Ordinates[i], geom.Ordinates[i+1]}
		if !corners[p] {
			t.Errorf("ordinate pair %v is not one of the rectangle's corners", p)
		}
	}
}

func TestNewRectangleGeometry_RejectsDegenerateOrAntimeridianBoxes(t *testing.T) {
	cases := []struct {
		name string
		sw   Point
		ne   Point
	}{
		{"sw==ne", Point{10, 10}, Point{10, 10}},
		{"sw lon >= ne lon", Point{10, 0}, Point{5, 10}},
		{"sw lat >= ne lat", Point{0, 10}, Point{10, 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRectangleGeometry(tc.sw, tc.ne, DefaultSRID); err == nil {
				t.Fatalf("expected error for sw=%v ne=%v", tc.sw, tc.ne)
			}
		})
	}
}

func TestGormValue_RendersConstructorSQL(t *testing.T) {
	geom := Geometry{GType: 2003, SRID: DefaultSRID, ElemInfo: []int{1, 1003, 1}, Ordinates: []float64{0, 0, 1, 0, 1, 1, 0, 1, 0, 0}}
	expr := geom.GormValue(nil, nil)

	wantSQL := "MDSYS.SDO_GEOMETRY(?, ?, NULL, MDSYS.SDO_ELEM_INFO_ARRAY(1,1003,1), MDSYS.SDO_ORDINATE_ARRAY(0,0,1,0,1,1,0,1,0,0))"
	if expr.SQL != wantSQL {
		t.Fatalf("SQL = %q, want %q", expr.SQL, wantSQL)
	}
	if len(expr.Vars) != 2 || expr.Vars[0] != 2003 || expr.Vars[1] != DefaultSRID {
		t.Fatalf("Vars = %v, want [2003 %d]", expr.Vars, DefaultSRID)
	}
}

func TestGormValue_EmptyGeometryIsNull(t *testing.T) {
	expr := Geometry{}.GormValue(nil, nil)
	if expr.SQL != "NULL" {
		t.Fatalf("SQL = %q, want NULL", expr.SQL)
	}
}

func TestNewPointGeometry_ValidatesRange(t *testing.T) {
	if _, err := NewPointGeometry(Point{200, 0}, DefaultSRID); err == nil {
		t.Fatal("expected error for out-of-range longitude")
	}
	if _, err := NewPointGeometry(Point{0, -100}, DefaultSRID); err == nil {
		t.Fatal("expected error for out-of-range latitude")
	}
	geom, err := NewPointGeometry(Point{10, 20}, DefaultSRID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if geom.GType != 2001 || len(geom.Ordinates) != 2 {
		t.Fatalf("unexpected point geometry: %+v", geom)
	}
}

// -- test helpers --

func haversineMeters(a, b Point) float64 {
	lat1, lon1 := a[1]*math.Pi/180, a[0]*math.Pi/180
	lat2, lon2 := b[1]*math.Pi/180, b[0]*math.Pi/180
	dLat := lat2 - lat1
	dLon := lon2 - lon1
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusMeters * math.Asin(math.Sqrt(h))
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func signedAreaFromOrdinates(ordinates []float64) float64 {
	n := len(ordinates) / 2
	ring := make(Ring, n)
	for i := 0; i < n; i++ {
		ring[i] = Point{ordinates[i*2], ordinates[i*2+1]}
	}
	return signedArea(ring)
}
