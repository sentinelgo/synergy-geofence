package handler

import (
	"sort"
	"strings"

	"github.com/google/uuid"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/search"
)

// geofenceSortFields whitelists the ListGeofences sort_by values — exactly
// the fields search.MatchesQuery also matches on (name, address,
// home_agency, exclude_from_colocation). coordinates/radius aren't
// supported here, same as search.
var geofenceSortFields = map[string]bool{
	"name":                    true,
	"exclude_from_colocation": true,
	"home_agency":             true,
	"address":                 true,
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
// entirely, matching the previous SQL fallback's behavior.
func sortGeofences(entities []*dbmodel.Geofence, sortField, order string) {
	if !isValidGeofenceSortField(sortField) {
		sort.SliceStable(entities, func(i, j int) bool {
			return lessGeofence(entities[i], entities[j], "", false)
		})
		return
	}
	desc := strings.EqualFold(order, "desc")
	sort.SliceStable(entities, func(i, j int) bool {
		return lessGeofence(entities[i], entities[j], sortField, desc)
	})
}

func lessGeofence(a, b *dbmodel.Geofence, sortField string, desc bool) bool {
	switch strings.ToLower(strings.TrimSpace(sortField)) {
	case "name":
		return lessWithTiebreak(strings.ToLower(a.Name), strings.ToLower(b.Name), a.ID, b.ID, desc)
	case "exclude_from_colocation":
		return lessBoolWithTiebreak(a.ExcludeFromColocation, b.ExcludeFromColocation, a.ID, b.ID, desc)
	case "home_agency":
		return lessNullableWithTiebreak(a.SynergyIdentifier, b.SynergyIdentifier, a.ID, b.ID, desc)
	case "address":
		aAddr, bAddr := nilIfEmpty(search.AddressText(a.GeoJSON)), nilIfEmpty(search.AddressText(b.GeoJSON))
		return lessNullableWithTiebreak(aAddr, bAddr, a.ID, b.ID, desc)
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
