package search

import (
	"testing"

	"github.com/google/uuid"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
)

func TestAddressText(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "full address",
			raw:  `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,"address":{"street1":"123 Main St","city":"Atlanta","state_province":"GA","postal_code":"30301"}}`,
			want: "123 Main St Atlanta GA 30301",
		},
		{
			name: "no address",
			raw:  `{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]}`,
			want: "",
		},
		{
			name: "blank geo_json",
			raw:  "",
			want: "",
		},
		{
			name: "invalid json",
			raw:  "not json",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AddressText(tc.raw); got != tc.want {
				t.Errorf("AddressText(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestMatchesQuery(t *testing.T) {
	g := &dbmodel.Geofence{
		Name:    "Downtown Office",
		GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,"address":{"street1":"123 Main St","city":"Atlanta"}}`,
	}

	for _, tc := range []struct {
		name  string
		query string
		want  bool
	}{
		{name: "empty query matches everything", query: "", want: true},
		{name: "matches name, case-insensitive", query: "downtown", want: true},
		{name: "matches address", query: "Main St", want: true},
		{name: "no match", query: "nonexistent", want: false},
		{name: "does not match radius", query: "500", want: false},
		{name: "does not match coordinates", query: "-84.388", want: false},
		{name: "1-char query matches nothing, even a real substring", query: "D", want: false},
		{name: "2-char query matches nothing, even a real substring", query: "Do", want: false},
		{name: "3-char query (MinQueryLen) matches normally", query: "Dow", want: true},
		{name: "matches exclude_from_colocation=false as text", query: "false", want: true},
		{name: "does not match true when excluded is false", query: "true", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesQuery(g, tc.query); got != tc.want {
				t.Errorf("MatchesQuery(query=%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestMatchesQuery_ExcludeFromColocation(t *testing.T) {
	excluded := &dbmodel.Geofence{Name: "Parole Office", ExcludeFromColocation: true}
	included := &dbmodel.Geofence{Name: "Downtown Office", ExcludeFromColocation: false}

	for _, tc := range []struct {
		name  string
		g     *dbmodel.Geofence
		query string
		want  bool
	}{
		{name: "true matches an excluded geofence", g: excluded, query: "true", want: true},
		{name: "false does not match an excluded geofence", g: excluded, query: "false", want: false},
		{name: "false matches an included geofence", g: included, query: "false", want: true},
		{name: "true does not match an included geofence", g: included, query: "true", want: false},
		{name: "tru (3-char) matches an excluded geofence", g: excluded, query: "tru", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesQuery(tc.g, tc.query); got != tc.want {
				t.Errorf("MatchesQuery(query=%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestMatchesQuery_NilGeofence(t *testing.T) {
	if MatchesQuery(nil, "anything") {
		t.Error("MatchesQuery(nil, ...) with a non-empty query = true, want false")
	}
	if !MatchesQuery(nil, "") {
		t.Error("MatchesQuery(nil, ...) with an empty query = false, want true")
	}
}

func TestMatchesFields(t *testing.T) {
	agencyID, otherAgencyID := uuid.New(), uuid.New()
	g := &dbmodel.Geofence{
		AgencyID:              agencyID,
		Name:                  "Parole Office",
		GeoJSON:               `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,"address":{"street1":"123 Main St","city":"Atlanta"}}`,
		ExcludeFromColocation: true,
	}

	for _, tc := range []struct {
		name string
		f    FieldFilters
		want bool
	}{
		{name: "no filters matches everything", f: FieldFilters{}, want: true},
		{name: "name substring, case-insensitive", f: FieldFilters{Name: "parole"}, want: true},
		{name: "name mismatch", f: FieldFilters{Name: "downtown"}, want: false},
		{name: "address substring", f: FieldFilters{Address: "main st"}, want: true},
		{name: "address is not matched against name", f: FieldFilters{Address: "parole"}, want: false},
		{name: "home agency matches owning agency", f: FieldFilters{HomeAgencyIDs: []uuid.UUID{agencyID}}, want: true},
		{name: "home agency mismatch", f: FieldFilters{HomeAgencyIDs: []uuid.UUID{otherAgencyID}}, want: false},
		{name: "home agency list is OR", f: FieldFilters{HomeAgencyIDs: []uuid.UUID{otherAgencyID, agencyID}}, want: true},
		{name: "excluded true matches", f: FieldFilters{ExcludeFromColocation: []bool{true}}, want: true},
		{name: "excluded false does not match", f: FieldFilters{ExcludeFromColocation: []bool{false}}, want: false},
		{name: "excluded true and false matches either", f: FieldFilters{ExcludeFromColocation: []bool{false, true}}, want: true},
		{name: "all fields AND together", f: FieldFilters{Name: "parole", Address: "atlanta", HomeAgencyIDs: []uuid.UUID{agencyID}, ExcludeFromColocation: []bool{true}}, want: true},
		{name: "one failing field fails the AND", f: FieldFilters{Name: "parole", Address: "boston"}, want: false},
		{name: "short value is honored (no MinQueryLen)", f: FieldFilters{Name: "P"}, want: true},
		{name: "whitespace name is no filter", f: FieldFilters{Name: "   "}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesFields(g, tc.f); got != tc.want {
				t.Errorf("MatchesFields(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

func TestMatchesFields_NilGeofence(t *testing.T) {
	if MatchesFields(nil, FieldFilters{Name: "x"}) {
		t.Error("MatchesFields(nil, filters) = true, want false")
	}
	if !MatchesFields(nil, FieldFilters{}) {
		t.Error("MatchesFields(nil, no filters) = false, want true")
	}
}

func TestCircleRadius(t *testing.T) {
	for _, tc := range []struct {
		name   string
		g      *dbmodel.Geofence
		want   float64
		wantOK bool
	}{
		{name: "circle", g: &dbmodel.Geofence{Type: dbmodel.GeofenceTypeCircle, GeoJSON: `{"type":"Circle","coordinates":[-84.388,33.749],"radius":750.5}`}, want: 750.5, wantOK: true},
		{name: "polygon has no radius", g: &dbmodel.Geofence{Type: dbmodel.GeofenceTypePolygon, GeoJSON: `{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]}`}},
		{name: "rectangle has no radius", g: &dbmodel.Geofence{Type: dbmodel.GeofenceTypeRectangle, GeoJSON: `{"type":"Rectangle","coordinates":[[0,0],[1,1]],"radius":10}`}},
		{name: "circle missing radius", g: &dbmodel.Geofence{Type: dbmodel.GeofenceTypeCircle, GeoJSON: `{"type":"Circle","coordinates":[0,0]}`}},
		{name: "circle zero radius", g: &dbmodel.Geofence{Type: dbmodel.GeofenceTypeCircle, GeoJSON: `{"type":"Circle","coordinates":[0,0],"radius":0}`}},
		{name: "invalid json", g: &dbmodel.Geofence{Type: dbmodel.GeofenceTypeCircle, GeoJSON: `not json`}},
		{name: "nil", g: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CircleRadius(tc.g)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("CircleRadius = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
