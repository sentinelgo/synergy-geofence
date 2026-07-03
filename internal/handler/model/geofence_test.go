package model

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/database/spatial"
)

func TestGeoJson_CircleRoundTrip(t *testing.T) {
	raw := `{"type":"Circle","coordinates":[-122.4194,37.7749],"radius":500}`

	var g GeoJson
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if g.Type() != "Circle" {
		t.Fatalf("Type() = %q, want Circle", g.Type())
	}
	if g.Circle == nil || g.Circle.Radius != 500 {
		t.Fatalf("unexpected circle: %+v", g.Circle)
	}

	out, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var roundTripped GeoJson
	if err = json.Unmarshal(out, &roundTripped); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if roundTripped.Type() != "Circle" || roundTripped.Circle.Radius != 500 {
		t.Fatalf("round trip mismatch: %+v", roundTripped.Circle)
	}
}

func TestGeoJson_RejectsUnknownType(t *testing.T) {
	var g GeoJson
	if err := json.Unmarshal([]byte(`{"type":"LineString","coordinates":[]}`), &g); err == nil {
		t.Fatal("expected error for unsupported geo_json type")
	}
}

func TestGeoJson_Rectangle(t *testing.T) {
	raw := `{"type":"Rectangle","coordinates":[[-122.43,37.77],[-122.41,37.79]]}`

	var g GeoJson
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if g.Type() != "Rectangle" {
		t.Fatalf("Type() = %q, want Rectangle", g.Type())
	}
	if g.Rectangle.Coordinates[0] != (spatial.Point{-122.43, 37.77}) || g.Rectangle.Coordinates[1] != (spatial.Point{-122.41, 37.79}) {
		t.Fatalf("unexpected rectangle: %+v", g.Rectangle)
	}
}

func TestGeofenceRequest_ToModel_Circle(t *testing.T) {
	req := &GeofenceRequest{
		Name:   "Test fence",
		Status: "active",
		UserID: uuid.New(),
		GeoJson: GeoJson{
			Circle: &GeoJsonCircle{Type: "Circle", Coordinates: spatial.Point{-122.4194, 37.7749}, Radius: 250},
		},
	}

	entity, err := req.ToModel(uuid.New(), spatial.DefaultSRID)
	if err != nil {
		t.Fatalf("ToModel: %v", err)
	}
	if entity.Type != dbmodel.GeofenceTypeCircle {
		t.Fatalf("entity.Type = %v, want Circle", entity.Type)
	}
	if entity.GeoJSON == "" {
		t.Fatal("expected geo_json to be populated")
	}
	wantOrdinates := (spatial.CircleSegments + 1) * 2 // densified polygon + closing repeat
	if len(entity.Geometry.Ordinates) != wantOrdinates {
		t.Fatalf("expected circle geometry to have %d ordinates, got %d", wantOrdinates, len(entity.Geometry.Ordinates))
	}

	resp, err := FromModel(entity)
	if err != nil {
		t.Fatalf("FromModel: %v", err)
	}
	if resp.GeoJson.Type() != "Circle" || resp.GeoJson.Circle.Radius != 250 {
		t.Fatalf("round trip mismatch: %+v", resp.GeoJson)
	}
}

func TestGeofenceRequest_ToModel_Rectangle(t *testing.T) {
	req := &GeofenceRequest{
		Name:   "Test rectangle",
		Status: "active",
		UserID: uuid.New(),
		GeoJson: GeoJson{
			Rectangle: &GeoJsonRectangle{
				Type:        "Rectangle",
				Coordinates: [2]spatial.Point{{-122.43, 37.77}, {-122.41, 37.79}},
			},
		},
	}

	entity, err := req.ToModel(uuid.New(), spatial.DefaultSRID)
	if err != nil {
		t.Fatalf("ToModel: %v", err)
	}
	if entity.Type != dbmodel.GeofenceTypeRectangle {
		t.Fatalf("entity.Type = %v, want Rectangle", entity.Type)
	}
	if len(entity.Geometry.Ordinates) != 10 { // 4 corners + closing repeat, 2 ordinates each
		t.Fatalf("expected rectangle geometry to have 10 ordinates, got %d", len(entity.Geometry.Ordinates))
	}

	resp, err := FromModel(entity)
	if err != nil {
		t.Fatalf("FromModel: %v", err)
	}
	if resp.GeoJson.Type() != "Rectangle" {
		t.Fatalf("round trip mismatch: %+v", resp.GeoJson)
	}
}

func TestGeofenceRequest_ToModel_RejectsPoint(t *testing.T) {
	req := &GeofenceRequest{
		Name:   "Bad fence",
		Status: "active",
		UserID: uuid.New(),
		GeoJson: GeoJson{
			Point: &GeoJsonPoint{Type: "Point", Coordinates: spatial.Point{0, 0}},
		},
	}

	if _, err := req.ToModel(uuid.New(), spatial.DefaultSRID); err == nil {
		t.Fatal("expected error: Point is not a valid geofence shape")
	}
}

func TestValidateNameCharset(t *testing.T) {
	valid := []string{"Main Parole Office", "O'Brien's Office", "Site-A", "Building 12"}
	for _, name := range valid {
		if err := validateNameCharset(name); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", name, err)
		}
	}

	invalid := []string{"Fence #1", "Site@Home", "50% Zone", "Office/Annex"}
	for _, name := range invalid {
		err := validateNameCharset(name)
		if err == nil {
			t.Errorf("expected %q to be rejected", name)
			continue
		}
		if !errors.Is(err, ErrInvalidNameCharset) {
			t.Errorf("expected error for %q to wrap ErrInvalidNameCharset, got: %v", name, err)
		}
	}
}

func TestGeofenceRequest_ToModel_RejectsInvalidNameCharset(t *testing.T) {
	req := &GeofenceRequest{
		Name:   "Fence #1",
		Status: "active",
		UserID: uuid.New(),
		GeoJson: GeoJson{
			Circle: &GeoJsonCircle{Type: "Circle", Coordinates: spatial.Point{0, 0}, Radius: 100},
		},
	}

	_, err := req.ToModel(uuid.New(), spatial.DefaultSRID)
	if !errors.Is(err, ErrInvalidNameCharset) {
		t.Fatalf("expected ErrInvalidNameCharset, got: %v", err)
	}
}

func TestGeofenceRequest_ToModel_KnownLocationFields(t *testing.T) {
	notes := "Parole office main building. Check-ins occur Monday-Friday 8 AM - 5 PM."
	req := &GeofenceRequest{
		Name:                  "Main Parole Office",
		Status:                "active",
		UserID:                uuid.New(),
		ExcludeFromColocation: true,
		Notes:                 &notes,
		GeoJson: GeoJson{
			Circle: &GeoJsonCircle{Type: "Circle", Coordinates: spatial.Point{-84.3880, 33.7490}, Radius: 100},
		},
	}

	entity, err := req.ToModel(uuid.New(), spatial.DefaultSRID)
	if err != nil {
		t.Fatalf("ToModel: %v", err)
	}
	if !entity.ExcludeFromColocation {
		t.Fatal("expected ExcludeFromColocation to be true")
	}
	if entity.Notes == nil || *entity.Notes != notes {
		t.Fatalf("expected notes to round trip, got %v", entity.Notes)
	}

	resp, err := FromModel(entity)
	if err != nil {
		t.Fatalf("FromModel: %v", err)
	}
	if !resp.ExcludeFromColocation || resp.Notes == nil || *resp.Notes != notes {
		t.Fatalf("unexpected response: exclude=%v notes=%v", resp.ExcludeFromColocation, resp.Notes)
	}
}

func TestDiffChangedFields_KnownLocationFlags(t *testing.T) {
	oldNotes := "old notes"
	newNotes := "new notes"

	old := &dbmodel.Geofence{ExcludeFromColocation: false, Notes: &oldNotes}
	updated := &dbmodel.Geofence{ExcludeFromColocation: true, Notes: &newNotes}

	changed := DiffChangedFields(old, updated)
	if v, ok := changed["exclude_from_colocation"]; !ok || v != true {
		t.Errorf("expected exclude_from_colocation to be reported as changed, got %v", changed)
	}
	if v, ok := changed["notes"]; !ok || v != updated.Notes {
		t.Errorf("expected notes to be reported as changed, got %v", changed)
	}

	// No change when both are nil/false.
	unchanged := DiffChangedFields(&dbmodel.Geofence{}, &dbmodel.Geofence{})
	if len(unchanged) != 0 {
		t.Errorf("expected no diff for identical geofences, got %v", unchanged)
	}
}

func TestStatusToDB_RoundTripsThroughFriendlyStrings(t *testing.T) {
	cases := map[string]dbmodel.GeofenceStatus{
		"active":   dbmodel.GeofenceStatusActive,
		"inactive": dbmodel.GeofenceStatusInactive,
		"archived": dbmodel.GeofenceStatusArchived,
		"deleted":  dbmodel.GeofenceStatusDeleted,
	}
	for str, code := range cases {
		if got := StatusToDB(str); got != code {
			t.Errorf("StatusToDB(%q) = %v, want %v", str, got, code)
		}
		if got := code.String(); got != str {
			t.Errorf("%v.String() = %q, want %q", code, got, str)
		}
		if !code.Valid() {
			t.Errorf("expected %v to be valid", code)
		}
	}

	if StatusToDB("bogus").Valid() {
		t.Error("expected an unrecognized status string to translate to an invalid code")
	}
}

func TestDiffChangedFields_StatusReportedAsFriendlyString(t *testing.T) {
	old := &dbmodel.Geofence{Status: dbmodel.GeofenceStatusActive}
	updated := &dbmodel.Geofence{Status: dbmodel.GeofenceStatusDeleted}

	changed := DiffChangedFields(old, updated)
	if v, ok := changed["status"]; !ok || v != "deleted" {
		t.Errorf(`expected status diff to be the string "deleted", got %v`, changed["status"])
	}
}
