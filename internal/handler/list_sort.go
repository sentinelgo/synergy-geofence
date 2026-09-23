package handler

import (
	"sort"
	"strings"

	"github.com/google/uuid"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/search"
)

// geofenceSortFields whitelists the ListGeofences sort_by values: the
// fields search.MatchesQuery matches on (name, address,
// exclude_from_colocation), plus home_agency, which sorts by the owning
// agency's synergy identifier — looked up per request from the agency
// service (see resolveListScope), never stored — and radius, which only
// Circle geofences have (every other shape sorts last). coordinates aren't
// supported.
var geofenceSortFields = map[string]bool{
	"name":                    true,
	"exclude_from_colocation": true,
	"address":                 true,
	"home_agency":             true,
	"radius":                  true,
}

// isValidGeofenceSortField reports whether field is a recognized
// ListGeofences sort_by value. An empty field is not valid here — "no
// sort_by given" is the caller's decision to make, not this function's.
func isValidGeofenceSortField(field string) bool {
	return geofenceSortFields[strings.ToLower(strings.TrimSpace(field))]
}

// sortGeofences sorts entities in place by sortField/order, entirely in
// memory (search, sort, and pagination are all applied after the DB fetch —
// see ListGeofences). An empty/unrecognized sortField preserves the
// original default order (most-recently-created first) and ignores order
// entirely, matching the previous SQL fallback's behavior. synergyIDs maps
// an agency id to its synergy identifier, for sort_by=home_agency; agencies
// missing from it (lookup failed) sort last, like any other missing value.
func sortGeofences(entities []*dbmodel.Geofence, sortField, order string, synergyIDs map[uuid.UUID]string) {
	if !isValidGeofenceSortField(sortField) {
		sort.SliceStable(entities, func(i, j int) bool {
			return lessGeofence(entities[i], entities[j], "", false, nil)
		})
		return
	}
	desc := strings.EqualFold(order, "desc")
	if strings.EqualFold(strings.TrimSpace(sortField), "radius") {
		sortByRadius(entities, desc)
		return
	}
	sort.SliceStable(entities, func(i, j int) bool {
		return lessGeofence(entities[i], entities[j], sortField, desc, synergyIDs)
	})
}

// sortByRadius sorts by Circle radius. Radii are parsed out of geo_json
// once up front rather than on every comparison. Non-Circle geofences
// (no radius) sort last regardless of direction, like any missing value.
func sortByRadius(entities []*dbmodel.Geofence, desc bool) {
	radii := make(map[uuid.UUID]float64, len(entities))
	for _, g := range entities {
		if r, ok := search.CircleRadius(g); ok {
			radii[g.ID] = r
		}
	}
	sort.SliceStable(entities, func(i, j int) bool {
		a, b := entities[i], entities[j]
		ar, aok := radii[a.ID]
		br, bok := radii[b.ID]
		if aok != bok {
			return aok
		}
		if aok && ar != br {
			if desc {
				return ar > br
			}
			return ar < br
		}
		return tiebreak(a.ID, b.ID, desc)
	})
}

// homeAgencySynergyIDs extracts sortGeofences' agency id → synergy
// identifier map from resolveListScope's agency details.
func homeAgencySynergyIDs(details map[uuid.UUID]agencyInfo) map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(details))
	for id, info := range details {
		out[id] = info.SynergyIdentifier
	}
	return out
}

func lessGeofence(a, b *dbmodel.Geofence, sortField string, desc bool, synergyIDs map[uuid.UUID]string) bool {
	switch strings.ToLower(strings.TrimSpace(sortField)) {
	case "name":
		return lessWithTiebreak(strings.ToLower(a.Name), strings.ToLower(b.Name), a.ID, b.ID, desc)
	case "exclude_from_colocation":
		return lessBoolWithTiebreak(a.ExcludeFromColocation, b.ExcludeFromColocation, a.ID, b.ID, desc)
	case "address":
		aAddr, bAddr := nilIfEmpty(search.AddressText(a.GeoJSON)), nilIfEmpty(search.AddressText(b.GeoJSON))
		return lessNullableWithTiebreak(aAddr, bAddr, a.ID, b.ID, desc)
	case "home_agency":
		aHome, bHome := nilIfEmpty(synergyIDs[a.AgencyID]), nilIfEmpty(synergyIDs[b.AgencyID])
		return lessNullableWithTiebreak(aHome, bHome, a.ID, b.ID, desc)
	default:
		// Original default: most-recently-created first, regardless of the
		// order param — id is the tiebreaker for created_at collisions
		// (only microsecond precision).
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return tiebreak(a.ID, b.ID, false)
	}
}

func lessWithTiebreak(a, b string, aID, bID uuid.UUID, desc bool) bool {
	if a != b {
		if desc {
			return a > b
		}
		return a < b
	}
	return tiebreak(aID, bID, desc)
}

func lessBoolWithTiebreak(a, b bool, aID, bID uuid.UUID, desc bool) bool {
	if a != b {
		if desc {
			return a && !b
		}
		return !a && b
	}
	return tiebreak(aID, bID, desc)
}

// lessNullableWithTiebreak treats a nil pointer as always sorting last,
// regardless of desc (mirrors SQL's NULLS LAST from the earlier SQL-driven
// implementation) — only present values are compared against each other,
// subject to desc.
func lessNullableWithTiebreak(a, b *string, aID, bID uuid.UUID, desc bool) bool {
	if (a == nil) != (b == nil) {
		return a != nil
	}
	if a == nil {
		return tiebreak(aID, bID, desc)
	}
	av, bv := strings.ToLower(*a), strings.ToLower(*b)
	if av != bv {
		if desc {
			return av > bv
		}
		return av < bv
	}
	return tiebreak(aID, bID, desc)
}

func tiebreak(aID, bID uuid.UUID, desc bool) bool {
	if desc {
		return aID.String() > bID.String()
	}
	return aID.String() < bID.String()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
