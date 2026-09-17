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
