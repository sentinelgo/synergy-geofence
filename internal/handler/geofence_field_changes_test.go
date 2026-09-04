package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/sentinelgo/synergy-geofence/internal/audit"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
)

// requireSingle fails the test unless fcs has exactly one element, then
// returns it — most geoJSONFieldChange cases produce exactly one
// audit.FieldChange; only a combined geometry+Address change produces two
// (see TestGeoJSONFieldChange_RadiusAndAddressChangedTogether).
func requireSingle(t *testing.T, fcs []audit.FieldChange) audit.FieldChange {
	t.Helper()
	if len(fcs) != 1 {
		t.Fatalf("geoJSONFieldChange returned %d changes, want 1: %+v", len(fcs), fcs)
	}
	return fcs[0]
}

// TestGeoJSONFieldChange_CircleRadiusChanged is the regression test for the
// reported bug: changing only a Circle's radius previously produced
// Field: "Location", Old: "Circle", New: "Circle" (Type unchanged), which
// rendered as the misleading "Modified Location from Circle to Circle".
func TestGeoJSONFieldChange_CircleRadiusChanged(t *testing.T) {
	original := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`,
	}
	updated := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":750}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Field != "Radius" {
		t.Errorf("Field = %q, want %q", fc.Field, "Radius")
	}
	if fc.Old != "500 m" || fc.New != "750 m" {
		t.Errorf("got Old=%q New=%q, want Old=%q New=%q", fc.Old, fc.New, "500 m", "750 m")
	}
	if fc.Old == fc.New {
		t.Errorf("Old and New must differ, both are %q", fc.Old)
	}
}

func TestGeoJSONFieldChange_CircleCenterMoved(t *testing.T) {
	original := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`,
	}
	updated := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.400,33.760],"radius":500}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Field != "Location" {
		t.Errorf("Field = %q, want %q", fc.Field, "Location")
	}
	wantOld, wantNew := "(33.749000, -84.388000)", "(33.760000, -84.400000)"
	if fc.Old != wantOld || fc.New != wantNew {
		t.Errorf("got Old=%q New=%q, want Old=%q New=%q", fc.Old, fc.New, wantOld, wantNew)
	}
}

func TestGeoJSONFieldChange_ShapeKindChanged(t *testing.T) {
	original := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`,
	}
	updated := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypePolygon,
		GeoJSON: `{"type":"Polygon","coordinates":[[[-84.4,33.7],[-84.3,33.7],[-84.3,33.8],[-84.4,33.7]]]}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Field != "Shape" || fc.Old != "Circle" || fc.New != "Polygon" {
		t.Errorf("got %+v, want Field=Shape Old=Circle New=Polygon", fc)
	}
}

func TestGeoJSONFieldChange_RectangleBoundaryChanged(t *testing.T) {
	original := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeRectangle,
		GeoJSON: `{"type":"Rectangle","coordinates":[[-84.4,33.7],[-84.3,33.8]]}`,
	}
	updated := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeRectangle,
		GeoJSON: `{"type":"Rectangle","coordinates":[[-84.5,33.6],[-84.2,33.9]]}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Field != "Boundary" {
		t.Errorf("Field = %q, want %q", fc.Field, "Boundary")
	}
	if fc.Old == fc.New {
		t.Errorf("Old and New must differ, both are %q", fc.Old)
	}
	if !strings.Contains(fc.Old, "33.700000") {
		t.Errorf("Old = %q, want it to contain the original min latitude", fc.Old)
	}
	if !strings.Contains(fc.New, "33.600000") {
		t.Errorf("New = %q, want it to contain the updated min latitude", fc.New)
	}
}

func TestGeoJSONFieldChange_PolygonBoundaryChanged(t *testing.T) {
	original := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypePolygon,
		GeoJSON: `{"type":"Polygon","coordinates":[[[-84.4,33.7],[-84.3,33.7],[-84.3,33.8],[-84.4,33.7]]]}`,
	}
	updated := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypePolygon,
		GeoJSON: `{"type":"Polygon","coordinates":[[[-84.4,33.7],[-84.3,33.7],[-84.35,33.9],[-84.4,33.7]]]}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Field != "Boundary" {
		t.Errorf("Field = %q, want %q", fc.Field, "Boundary")
	}
	if fc.Old == fc.New {
		t.Errorf("Old and New must differ, both are %q", fc.Old)
	}
	if !strings.HasPrefix(fc.Old, "4 point(s)") || !strings.HasPrefix(fc.New, "4 point(s)") {
		t.Errorf("got Old=%q New=%q, want both to start with the point count", fc.Old, fc.New)
	}
}

func TestGeoJSONFieldChange_PointMoved(t *testing.T) {
	original := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeUnknown, // Point has no dedicated GeofenceType constant today
		GeoJSON: `{"type":"Point","coordinates":[-84.388,33.749]}`,
	}
	updated := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeUnknown,
		GeoJSON: `{"type":"Point","coordinates":[-84.4,33.76]}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Field != "Location" {
		t.Errorf("Field = %q, want %q", fc.Field, "Location")
	}
	if fc.Old == fc.New {
		t.Errorf("Old and New must differ, both are %q", fc.Old)
	}
}

// TestGeoJSONFieldChange_AddressOnlyChanged covers editing a shape's
// embedded Address without touching its geometry — e.g. correcting a Known
// Location's street address. This must produce its own "Address" field
// change with the actual before/after address, not collapse into the
// generic noSummarizableChange placeholder the way it used to.
func TestGeoJSONFieldChange_AddressOnlyChanged(t *testing.T) {
	original := &dbmodel.Geofence{
		Type: dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,` +
			`"address":{"street1":"123 Main St","city":"Atlanta","state_province":"GA","postal_code":"30301"}}`,
	}
	updated := &dbmodel.Geofence{
		Type: dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,` +
			`"address":{"street1":"456 Oak Ave","city":"Atlanta","state_province":"GA","postal_code":"30302"}}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Field != "Address" {
		t.Errorf("Field = %q, want %q", fc.Field, "Address")
	}
	wantOld := "123 Main St, Atlanta, GA, 30301"
	wantNew := "456 Oak Ave, Atlanta, GA, 30302"
	if fc.Old != wantOld || fc.New != wantNew {
		t.Errorf("got Old=%q New=%q, want Old=%q New=%q", fc.Old, fc.New, wantOld, wantNew)
	}
}

// TestGeoJSONFieldChange_AddressAdded covers a shape that had no address at
// all gaining one — formatAddress's nil case ("(none)") must appear as the
// Old value, not an empty string.
func TestGeoJSONFieldChange_AddressAdded(t *testing.T) {
	original := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`,
	}
	updated := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,"address":{"street1":"456 Oak Ave"}}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Field != "Address" {
		t.Errorf("Field = %q, want %q", fc.Field, "Address")
	}
	if fc.Old != "(none)" {
		t.Errorf("Old = %q, want %q", fc.Old, "(none)")
	}
	if fc.New != "456 Oak Ave" {
		t.Errorf("New = %q, want %q", fc.New, "456 Oak Ave")
	}
}

// TestGeoJSONFieldChange_RadiusAndAddressChangedTogether covers a single
// PATCH that changes both the geometry and the address: it must emit two
// distinct FieldChange entries (like Name+Status already do at the
// geofenceFieldChanges level) rather than reporting only one and silently
// dropping the other.
func TestGeoJSONFieldChange_RadiusAndAddressChangedTogether(t *testing.T) {
	original := &dbmodel.Geofence{
		Type: dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,` +
			`"address":{"street1":"123 Main St"}}`,
	}
	updated := &dbmodel.Geofence{
		Type: dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":750,` +
			`"address":{"street1":"456 Oak Ave"}}`,
	}

	fcs := geoJSONFieldChange(context.Background(), original, updated)
	if len(fcs) != 2 {
		t.Fatalf("geoJSONFieldChange returned %d changes, want 2: %+v", len(fcs), fcs)
	}

	byField := map[string]audit.FieldChange{}
	for _, fc := range fcs {
		byField[fc.Field] = fc
	}
	if radius, ok := byField["Radius"]; !ok || radius.Old != "500 m" || radius.New != "750 m" {
		t.Errorf("Radius change = %+v, want Old=500 m New=750 m", radius)
	}
	if addr, ok := byField["Address"]; !ok || addr.Old != "123 Main St" || addr.New != "456 Oak Ave" {
		t.Errorf("Address change = %+v, want Old=123 Main St New=456 Oak Ave", addr)
	}
}

// TestGeoJSONFieldChange_NoSummarizableDifference covers the residual
// degenerate case — geo_json's raw bytes differ (an unrecognized field was
// added, say) but neither geometry nor Address diffing finds an actual
// difference: the safety net must never report the same Old and New value
// under any field label.
func TestGeoJSONFieldChange_NoSummarizableDifference(t *testing.T) {
	original := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`,
	}
	updated := &dbmodel.Geofence{
		Type:    dbmodel.GeofenceTypeCircle,
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,"unrecognized_field":"x"}`,
	}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Old == fc.New {
		t.Errorf("Old and New must differ, both are %q", fc.Old)
	}
}

// TestGeoJSONFieldChange_UnparsableGeoJSONFallsBackSafely covers a
// malformed persisted geo_json value: since Type is unchanged here, the old
// fallback (reporting Type.String() for both Old and New) would have
// reproduced the exact reported bug ("Circle to Circle") under a different
// root cause. It must fall back to the safe, distinct placeholder instead.
func TestGeoJSONFieldChange_UnparsableGeoJSONFallsBackSafely(t *testing.T) {
	original := &dbmodel.Geofence{Type: dbmodel.GeofenceTypeCircle, GeoJSON: `not json`}
	updated := &dbmodel.Geofence{Type: dbmodel.GeofenceTypeCircle, GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`}

	fc := requireSingle(t, geoJSONFieldChange(context.Background(), original, updated))

	if fc.Old == fc.New {
		t.Errorf("Old and New must differ, both are %q", fc.Old)
	}
}
