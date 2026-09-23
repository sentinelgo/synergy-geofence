// Package search implements ListGeofences' in-memory search matching —
// name/address/home_agency/exclude_from_colocation, no coordinates/radius —
// applied to an already-fetched []*dbmodel.Geofence slice (see
// GeofenceController.ListGeofences), not pushed into SQL. This mirrors the
// fetch-broadly-then-filter/sort/paginate-in-Go pattern sso-agency-service's
// advanced agency search already uses, rather than an Oracle Text index.
package search

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
)

// MinQueryLen is the shortest query MatchesQuery will actually search
// with, mirroring sso-agency-service's buildSearchCriteria (minSearchLen):
// a query shorter than this (but non-empty) is treated as an invalid
// search — it matches nothing at all, rather than matching every geofence
// whose fields happen to contain that short substring.
const MinQueryLen = 3

// MatchesQuery reports whether g matches a plain, case-insensitive
// substring search: query matches if it appears in the geofence's name,
// its flattened address (see AddressText), homeAgency (the owning agency's
// synergy identifier — pass g.SynergyIdentifier dereferenced, or "" if
// nil), or the "true"/"false" text of exclude_from_colocation (so
// query="true" matches every excluded geofence, "false" every included
// one — not a boolean-typed filter, just one more string field). An
// empty/blank query matches everything; a query shorter than MinQueryLen
// matches nothing (see its doc comment). There is no section-prefix syntax
// and no boolean operators — coordinates and radius are not searchable
// this way (they aren't meaningful text search targets).
func MatchesQuery(g *dbmodel.Geofence, homeAgency, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	if len(query) < MinQueryLen {
		return false
	}
	if g == nil {
		return false
	}
	if strings.Contains(strings.ToLower(g.Name), query) {
		return true
	}
	if strings.Contains(strings.ToLower(AddressText(g.GeoJSON)), query) {
		return true
	}
	if strings.Contains(strings.ToLower(homeAgency), query) {
		return true
	}
	if strings.Contains(strconv.FormatBool(g.ExcludeFromColocation), query) {
		return true
	}
	return false
}

// FieldFilters are ListGeofences' per-field search filters. Unlike the
// single free-text query (see MatchesQuery), which matches if *any* field
// contains it, every set field here must match (AND) — and they AND with
// the free-text query too. Name/Address are plain, case-insensitive
// substring matches against the same text MatchesQuery searches.
// HomeAgencies and ExcludeFromColocation are lists — a geofence matches if
// it equals *any* value in the list (OR within the field): HomeAgencies
// compares the owning agency's synergy identifier exactly (ignoring case,
// so "SYN1" doesn't also match "SYN10"), ExcludeFromColocation the
// boolean flag (so [true, false] matches everything). A blank string or
// empty list means "no filter on this field". There is no MinQueryLen
// floor here: a field filter is an explicit, targeted ask rather than a
// type-ahead search box, so even a one-character value is honored.
type FieldFilters struct {
	Name                  string
	Address               string
	HomeAgencies          []string
	ExcludeFromColocation []bool
}

// IsZero reports whether no field filter is set. Blank strings — Name,
// Address or HomeAgencies entries — don't count as set.
func (f FieldFilters) IsZero() bool {
	return strings.TrimSpace(f.Name) == "" &&
		strings.TrimSpace(f.Address) == "" &&
		!slices.ContainsFunc(f.HomeAgencies, isNotBlank) &&
		len(f.ExcludeFromColocation) == 0
}

// MatchesFields reports whether g satisfies every set filter in f (see
// FieldFilters). homeAgency is passed the same way as for MatchesQuery.
// Blank HomeAgencies entries are ignored, so a list of only blanks is no
// filter rather than a match on "no home agency".
func MatchesFields(g *dbmodel.Geofence, homeAgency string, f FieldFilters) bool {
	if f.IsZero() {
		return true
	}
	if g == nil {
		return false
	}
	if !containsFold(g.Name, f.Name) {
		return false
	}
	// Guarded separately so AddressText's JSON parse is skipped when
	// there's no address filter.
	if strings.TrimSpace(f.Address) != "" && !containsFold(AddressText(g.GeoJSON), f.Address) {
		return false
	}
	if slices.ContainsFunc(f.HomeAgencies, isNotBlank) && !slices.ContainsFunc(f.HomeAgencies, func(v string) bool {
		v = strings.TrimSpace(v)
		return v != "" && strings.EqualFold(v, homeAgency)
	}) {
		return false
	}
	if len(f.ExcludeFromColocation) > 0 && !slices.Contains(f.ExcludeFromColocation, g.ExcludeFromColocation) {
		return false
	}
	return true
}

func isNotBlank(s string) bool {
	return strings.TrimSpace(s) != ""
}

// containsFold reports whether s contains substr, case-insensitively,
// after trimming substr. A blank substr always matches.
func containsFold(s, substr string) bool {
	substr = strings.ToLower(strings.TrimSpace(substr))
	if substr == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s), substr)
}

// AddressText extracts a flattened, human-readable address string from a
// geofence's geo_json for search matching (e.g. "123 Main St Atlanta GA
// 30301") — built from the parsed address object's own fields (street1,
// street2, city, state, postal code), not a raw dump of the whole geo_json
// blob. Returns "" if geo_json has no address or isn't valid JSON.
func AddressText(geoJSON string) string {
	var doc struct {
		Address *struct {
			Street1       string `json:"street1"`
			Street2       string `json:"street2"`
			City          string `json:"city"`
			StateProvince string `json:"state_province"`
			PostalCode    string `json:"postal_code"`
			Country       string `json:"country"`
		} `json:"address"`
	}
	geoJSON = strings.TrimSpace(geoJSON)
	if geoJSON == "" || json.Unmarshal([]byte(geoJSON), &doc) != nil || doc.Address == nil {
		return ""
	}
	a := doc.Address
	parts := []string{a.Street1, a.Street2, a.City, a.StateProvince, a.PostalCode, a.Country}
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, " ")
}
