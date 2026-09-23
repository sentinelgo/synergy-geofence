package search

import (
	"testing"

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
		name       string
		query      string
		homeAgency string
		want       bool
	}{
		{name: "empty query matches everything", query: "", want: true},
		{name: "matches name, case-insensitive", query: "downtown", want: true},
		{name: "matches address", query: "Main St", want: true},
		{name: "matches home agency", query: "syn123", homeAgency: "SYN123", want: true},
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
			if got := MatchesQuery(g, tc.homeAgency, tc.query); got != tc.want {
				t.Errorf("MatchesQuery(query=%q, homeAgency=%q) = %v, want %v", tc.query, tc.homeAgency, got, tc.want)
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
			if got := MatchesQuery(tc.g, "", tc.query); got != tc.want {
				t.Errorf("MatchesQuery(query=%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestMatchesQuery_NilGeofence(t *testing.T) {
	if MatchesQuery(nil, "", "anything") {
		t.Error("MatchesQuery(nil, ...) with a non-empty query = true, want false")
	}
	if !MatchesQuery(nil, "", "") {
		t.Error("MatchesQuery(nil, ...) with an empty query = false, want true")
	}
}

func TestMatchesFields(t *testing.T) {
	g := &dbmodel.Geofence{
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
		{name: "home agency exact, case-insensitive", f: FieldFilters{HomeAgencies: []string{"syn123"}}, want: true},
		{name: "home agency is not a substring match", f: FieldFilters{HomeAgencies: []string{"SYN1"}}, want: false},
		{name: "home agency list is OR", f: FieldFilters{HomeAgencies: []string{"SYN999", "SYN123"}}, want: true},
		{name: "home agency list with no hit", f: FieldFilters{HomeAgencies: []string{"SYN998", "SYN999"}}, want: false},
		{name: "excluded true matches", f: FieldFilters{ExcludeFromColocation: []bool{true}}, want: true},
		{name: "excluded false does not match", f: FieldFilters{ExcludeFromColocation: []bool{false}}, want: false},
		{name: "excluded true and false matches either", f: FieldFilters{ExcludeFromColocation: []bool{false, true}}, want: true},
		{name: "all fields AND together", f: FieldFilters{Name: "parole", Address: "atlanta", HomeAgencies: []string{"SYN123"}, ExcludeFromColocation: []bool{true}}, want: true},
		{name: "one failing field fails the AND", f: FieldFilters{Name: "parole", Address: "boston"}, want: false},
		{name: "short value is honored (no MinQueryLen)", f: FieldFilters{Name: "P"}, want: true},
		{name: "blank-only home agencies is no filter", f: FieldFilters{HomeAgencies: []string{" ", ""}}, want: true},
		{name: "blank entries are ignored in a real list", f: FieldFilters{HomeAgencies: []string{" ", "SYN123"}}, want: true},
		{name: "whitespace name is no filter", f: FieldFilters{Name: "   "}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesFields(g, "SYN123", tc.f); got != tc.want {
				t.Errorf("MatchesFields(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

func TestMatchesFields_NilGeofence(t *testing.T) {
	if MatchesFields(nil, "", FieldFilters{Name: "x"}) {
		t.Error("MatchesFields(nil, filters) = true, want false")
	}
	if !MatchesFields(nil, "", FieldFilters{}) {
		t.Error("MatchesFields(nil, no filters) = false, want true")
	}
}

// TestMatchesFields_NoHomeAgency confirms a geofence with no home agency
// (nil synergy_identifier, passed as "") never matches a home_agency
// filter, including one that only contains blank entries alongside a
// real value.
func TestMatchesFields_NoHomeAgency(t *testing.T) {
	g := &dbmodel.Geofence{Name: "Parole Office"}
	if MatchesFields(g, "", FieldFilters{HomeAgencies: []string{"SYN1"}}) {
		t.Error("geofence without home agency matched home_agency=SYN1")
	}
	if MatchesFields(g, "", FieldFilters{HomeAgencies: []string{"", "SYN1"}}) {
		t.Error(`geofence without home agency matched home_agency=["", "SYN1"]`)
	}
}
