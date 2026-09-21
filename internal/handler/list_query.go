package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	hmodel "github.com/sentinelgo/synergy-geofence/internal/handler/model"

	localerrors "github.com/sentinelgo/synergy-geofence/internal/errors"
)

// listFilterField names which list-request field a buildGeofenceListFilters
// error came from, so the handler can report the right error code.
type listFilterField string

const (
	includeSubagencyField listFilterField = "includeSubagency"
	agencyIdsField        listFilterField = "agency_ids"
	sortByField           listFilterField = "sort_by"
	clientIdField         listFilterField = "client_id"
	statusField           listFilterField = "status"
	bodyField             listFilterField = "body"
)

// listFilterFieldError maps a failed list-request field to its response
// error, preserving the exact codes client_id/status already used before
// body support existed; the rest share the generic ErrInvalidQuery code
// with a dynamic input, and a malformed JSON body gets the same
// ErrBindJson code Create/UpdateGeofence use for their own bind failures.
func listFilterFieldError(field listFilterField) *localerrors.CustomError {
	switch field {
	case clientIdField:
		return localerrors.NewError(localerrors.ErrUUIDParse, localerrors.ClientId)
	case statusField:
		return localerrors.NewError(localerrors.ErrMismatch, localerrors.Geofence)
	case bodyField:
		return localerrors.NewError(localerrors.ErrBindJson, localerrors.GeofenceReq)
	default:
		return localerrors.NewError(localerrors.ErrInvalidQuery, localerrors.ErrorInput(field))
	}
}

// geofenceListFilters is buildGeofenceListFilters' fully resolved result —
// every ListGeofences input, regardless of whether it came from the JSON
// body or query-string params. query is a plain search term (see
// internal/search.MatchesQuery), applied in memory after the DB fetch —
// not a SQL clause.
type geofenceListFilters struct {
	query            string
	includeSubagency bool
	agencyIDs        []uuid.UUID
	sortField        string
	order            string
	clientID         *uuid.UUID
	status           *dbmodel.GeofenceStatus
	page             int
	pageSize         int
}

// SearchRequest is the list request body's "search" envelope key, mirroring
// sso-user-service's parse.SearchRequest.
type SearchRequest struct {
	Any string `json:"any,omitempty"`
}

// PaginationRequest is the list request body's "pagination" envelope key,
// mirroring sso-user-service's parse.PaginationRequest. Limit plays the
// same role as the page_size query param, under the shared LIST-body
// contract's own field name.
type PaginationRequest struct {
	Limit *int `json:"limit,omitempty"`
	Page  *int `json:"page,omitempty"`
}

// GeofenceListData is the list request body's "data" envelope key —
// geofence-list-specific filters and sort, mirroring sso-user-service's
// UserListData.
type GeofenceListData struct {
	// agency_ids (the explicit pre-resolved subagency scope) isn't
	// supported in the body yet — only via its existing query param.
	Status           string `json:"status,omitempty"`
	SortBy           string `json:"sort_by,omitempty"`
	Order            string `json:"order,omitempty"`
	IncludeSubagency bool   `json:"include_subagency,omitempty"`
}

// GeofenceListRequest is the standardized LIST request envelope for
// ListGeofences, mirroring sso-user-service's UserListRequest exactly:
// detected by the presence of a top-level search/pagination/data key in the
// raw JSON body. If none of those keys are present — including an empty or
// non-JSON body — ListGeofences falls back to today's query-string parsing
// instead, so existing callers are unaffected.
type GeofenceListRequest struct {
	Search     *SearchRequest     `json:"search,omitempty"`
	Pagination *PaginationRequest `json:"pagination,omitempty"`
	Data       GeofenceListData   `json:"data"`
}

// geofenceListRequestFromBody reads and decodes the request body into a
// GeofenceListRequest, mirroring sso-user-service's
// usersListQueryFromRequest. It returns (nil, nil) — not an error — for an
// absent/empty/non-JSON body, or JSON lacking all three envelope keys, so
// the caller falls back to query-string parsing untouched.
func geofenceListRequestFromBody(c *gin.Context) (*GeofenceListRequest, error) {
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	hasJSONBody := strings.Contains(contentType, "application/json")
	if c.Request.ContentLength == 0 && !hasJSONBody {
		return nil, nil
	}

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}

	var bodyKeys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &bodyKeys); err != nil {
		return nil, err
	}
	_, hasData := bodyKeys["data"]
	_, hasSearch := bodyKeys["search"]
	_, hasPagination := bodyKeys["pagination"]
	if !hasData && !hasSearch && !hasPagination {
		return nil, nil
	}

	var body GeofenceListRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return &body, nil
}

func buildGeofenceListFilters(c *gin.Context) (geofenceListFilters, listFilterField, error) {
	body, err := geofenceListRequestFromBody(c)
	if err != nil {
		return geofenceListFilters{}, bodyField, err
	}
	if body != nil {
		return geofenceListFiltersFromBody(*body)
	}
	return geofenceListFiltersFromQuery(c)
}

func geofenceListFiltersFromQuery(c *gin.Context) (geofenceListFilters, listFilterField, error) {
	query := strings.TrimSpace(c.Query("q"))

	includeSubagency, err := parseIncludeSubagencyQuery(c)
	if err != nil {
		return geofenceListFilters{}, includeSubagencyField, err
	}

	agencyIDs, err := parseAgencyIdsQuery(c)
	if err != nil {
		return geofenceListFilters{}, agencyIdsField, err
	}

	sortField, err := parseSortByQuery(c)
	if err != nil {
		return geofenceListFilters{}, sortByField, err
	}

	clientID, err := parseOptionalUUIDQuery(c, "client_id")
	if err != nil {
		return geofenceListFilters{}, clientIdField, err
	}

	status, err := parseStatusValue(c.Query("status"))
	if err != nil {
		return geofenceListFilters{}, statusField, err
	}

	page, pageSize := parsePagination(c)

	return geofenceListFilters{
		query:            query,
		includeSubagency: includeSubagency,
		agencyIDs:        agencyIDs,
		sortField:        sortField,
		order:            parseOrderQuery(c),
		clientID:         clientID,
		status:           status,
		page:             page,
		pageSize:         pageSize,
	}, "", nil
}

func geofenceListFiltersFromBody(body GeofenceListRequest) (geofenceListFilters, listFilterField, error) {
	var query string
	if body.Search != nil {
		query = strings.TrimSpace(body.Search.Any)
	}

	data := body.Data

	sortField, err := validateSortByValue(data.SortBy)
	if err != nil {
		return geofenceListFilters{}, sortByField, err
	}

	status, err := parseStatusValue(data.Status)
	if err != nil {
		return geofenceListFilters{}, statusField, err
	}

	var pagePtr, pageSizePtr *int
	if body.Pagination != nil {
		pagePtr = body.Pagination.Page
		pageSizePtr = body.Pagination.Limit
	}
	page, pageSize := paginationFromValues(pagePtr, pageSizePtr)

	return geofenceListFilters{
		query:            query,
		includeSubagency: data.IncludeSubagency,
		sortField:        sortField,
		order:            normalizeOrderValue(data.Order),
		status:           status,
		page:             page,
		pageSize:         pageSize,
	}, "", nil
}

func parseIncludeSubagencyQuery(c *gin.Context) (bool, error) {
	for _, key := range []string{"includeSubagency"} {
		raw := strings.TrimSpace(c.Query(key))
		if raw == "" {
			continue
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return false, err
		}
		return v, nil
	}
	return false, nil
}

// parseSortByQuery validates the sort_by query param (if present) against
// the whitelist FindByAgencyAndClient actually sorts on. An empty/absent
// value is valid — it preserves ListGeofences' original default order.
func parseSortByQuery(c *gin.Context) (string, error) {
	return validateSortByValue(c.Query("sort_by"))
}

// validateSortByValue is parseSortByQuery's source-agnostic core, reused by
// both the query-string and JSON-body paths.
func validateSortByValue(sortField string) (string, error) {
	sortField = strings.TrimSpace(sortField)
	if sortField == "" {
		return "", nil
	}
	if !isValidGeofenceSortField(sortField) {
		return "", fmt.Errorf("invalid sort_by: %q", sortField)
	}
	return sortField, nil
}

// parseOrderQuery normalizes the order query param the same way
// sso-user-service does: anything other than exactly "desc"
// (case-insensitive) is treated as "asc" rather than rejected.
func parseOrderQuery(c *gin.Context) string {
	return normalizeOrderValue(c.Query("order"))
}

// normalizeOrderValue is parseOrderQuery's source-agnostic core, reused by
// both the query-string and JSON-body paths.
func normalizeOrderValue(order string) string {
	if strings.EqualFold(strings.TrimSpace(order), "desc") {
		return "desc"
	}
	return "asc"
}

// parseAgencyIdsQuery parses the comma-separated agency_ids query param
// (an explicit subagency scope the caller already resolved) into UUIDs. It
// returns a nil slice when the param is absent so callers can distinguish
// "no explicit scope given" from "explicit but empty".
func parseAgencyIdsQuery(c *gin.Context) ([]uuid.UUID, error) {
	raw := strings.TrimSpace(c.Query("agency_ids"))
	if raw == "" {
		return nil, nil
	}
	return parseAgencyIDStrings(strings.Split(raw, ","))
}

// parseAgencyIDStrings is parseAgencyIdsQuery's source-agnostic core: the
// query path splits its comma-joined string into a slice first, the body
// path already has one natively (agency_ids is a JSON array there).
func parseAgencyIDStrings(raw []string) ([]uuid.UUID, error) {
	agencyIDs := make([]uuid.UUID, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := uuid.Parse(p)
		if err != nil {
			return nil, err
		}
		agencyIDs = append(agencyIDs, id)
	}
	if len(agencyIDs) == 0 {
		return nil, nil
	}
	return agencyIDs, nil
}

// parseStatusValue validates a status string (from either the query param
// or the body) against the same friendly-string set ListGeofences has
// always accepted. An empty value is valid — it's ListGeofences'
// "everything except deleted" default (see FindByAgencyAndClient's doc
// comment), not an error.
func parseStatusValue(raw string) (*dbmodel.GeofenceStatus, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	s := hmodel.StatusToDB(raw)
	if !s.Valid() {
		return nil, fmt.Errorf("invalid status: %q", raw)
	}
	return &s, nil
}

// paginationFromValues is parsePagination's source-agnostic core for the
// body path (Page/Limit there are already *int, not query strings).
func paginationFromValues(page, pageSize *int) (int, int) {
	p := defaultPage
	ps := defaultPageSize
	if page != nil && *page > 0 {
		p = *page
	}
	if pageSize != nil && *pageSize > 0 && *pageSize <= maxPageSize {
		ps = *pageSize
	}
	return p, ps
}
