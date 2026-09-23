package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	hmodel "github.com/sentinelgo/synergy-geofence/internal/handler/model"
	"github.com/sentinelgo/synergy-geofence/internal/search"

	localerrors "github.com/sentinelgo/synergy-geofence/internal/errors"
)

// listFilterField names which list-request field a buildGeofenceListFilters
// error came from, so the handler can report the right error code.
type listFilterField string

const (
	includeSubagencyField listFilterField = "includeSubagency"
	sortByField           listFilterField = "sort_by"
	clientIdField         listFilterField = "client_id"
	statusField           listFilterField = "status"
	excludeFromColocField listFilterField = "exclude_from_colocation"
	homeAgencyField       listFilterField = "home_agency"
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
// internal/search.MatchesQuery) and fields the per-field AND filters (see
// internal/search.MatchesFields), both applied in memory after the DB
// fetch — not SQL clauses.
type geofenceListFilters struct {
	query            string
	fields           search.FieldFilters
	includeSubagency bool
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

	// Per-field search filters, ANDed with each other and with Any —
	// see search.FieldFilters. HomeAgency (owning agency UUIDs) and
	// ExcludeFromColocation take either a single value or an array (OR
	// within the field).
	Name                  string     `json:"name,omitempty"`
	Address               string     `json:"address,omitempty"`
	HomeAgency            stringList `json:"home_agency,omitempty"`
	ExcludeFromColocation boolList   `json:"exclude_from_colocation,omitempty"`
}

// stringList is a JSON string-or-array-of-strings body field: "<uuid>" and
// ["<uuid1>", "<uuid2>"] both decode, so a single value doesn't need
// wrapping.
// Blank entries are dropped, and a top-level null means "no filter".
type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	if isJSONNull(b) {
		*l = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = compactStrings([]string{one})
		return nil
	}
	var many []*string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("must be a string or an array of strings: %w", err)
	}
	out := make([]string, 0, len(many))
	for _, v := range many {
		if v == nil {
			return errors.New("array entries must not be null")
		}
		out = append(out, *v)
	}
	*l = compactStrings(out)
	return nil
}

// boolList is stringList's boolean counterpart: true and [true, false]
// both decode. A top-level null means "no filter"; a null array entry is
// an error rather than silently decoding as false (encoding/json's
// default for null into a bool).
type boolList []bool

func (l *boolList) UnmarshalJSON(b []byte) error {
	if isJSONNull(b) {
		*l = nil
		return nil
	}
	var one bool
	if err := json.Unmarshal(b, &one); err == nil {
		*l = boolList{one}
		return nil
	}
	var many []*bool
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("must be a boolean or an array of booleans: %w", err)
	}
	out := make(boolList, 0, len(many))
	for _, v := range many {
		if v == nil {
			return errors.New("array entries must not be null")
		}
		out = append(out, *v)
	}
	*l = out
	return nil
}

func isJSONNull(b []byte) bool {
	return bytes.Equal(bytes.TrimSpace(b), []byte("null"))
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
		return geofenceListFiltersFromBody(c, *body)
	}
	return geofenceListFiltersFromQuery(c)
}

func geofenceListFiltersFromQuery(c *gin.Context) (geofenceListFilters, listFilterField, error) {
	query := strings.TrimSpace(c.Query("q"))

	includeSubagency, err := parseIncludeSubagencyQuery(c)
	if err != nil {
		return geofenceListFilters{}, includeSubagencyField, err
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

	excludeFromColocation, err := parseBoolListQuery(c, "exclude_from_colocation")
	if err != nil {
		return geofenceListFilters{}, excludeFromColocField, err
	}

	homeAgencyIDs, err := parseAgencyIDStrings(parseListQuery(c, "home_agency"))
	if err != nil {
		return geofenceListFilters{}, homeAgencyField, err
	}

	page, pageSize := parsePagination(c)

	return geofenceListFilters{
		query: query,
		fields: search.FieldFilters{
			Name:                  strings.TrimSpace(c.Query("name")),
			Address:               strings.TrimSpace(c.Query("address")),
			HomeAgencyIDs:         homeAgencyIDs,
			ExcludeFromColocation: excludeFromColocation,
		},
		includeSubagency: includeSubagency,
		sortField:        sortField,
		order:            parseOrderQuery(c),
		clientID:         clientID,
		status:           status,
		page:             page,
		pageSize:         pageSize,
	}, "", nil
}

// geofenceListFiltersFromBody reads every filter from the body, except
// client_id: it has no body field, so it's still read from the query
// string (the only query param honored when a body is present).
func geofenceListFiltersFromBody(c *gin.Context, body GeofenceListRequest) (geofenceListFilters, listFilterField, error) {
	var query string
	var fields search.FieldFilters
	if body.Search != nil {
		homeAgencyIDs, err := parseAgencyIDStrings(body.Search.HomeAgency)
		if err != nil {
			return geofenceListFilters{}, homeAgencyField, err
		}
		query = strings.TrimSpace(body.Search.Any)
		fields = search.FieldFilters{
			Name:                  strings.TrimSpace(body.Search.Name),
			Address:               strings.TrimSpace(body.Search.Address),
			HomeAgencyIDs:         homeAgencyIDs,
			ExcludeFromColocation: body.Search.ExcludeFromColocation,
		}
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

	clientID, err := parseOptionalUUIDQuery(c, "client_id")
	if err != nil {
		return geofenceListFilters{}, clientIdField, err
	}

	return geofenceListFilters{
		query:            query,
		fields:           fields,
		clientID:         clientID,
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

// parseListQuery reads a multi-valued query param, accepting both
// repeated (?k=a&k=b) and comma-separated (?k=a,b) forms, or a mix. Blank
// entries are dropped; nil means the param is absent (no filter).
func parseListQuery(c *gin.Context, key string) []string {
	var out []string
	for _, raw := range c.QueryArray(key) {
		out = append(out, strings.Split(raw, ",")...)
	}
	return compactStrings(out)
}

// parseBoolListQuery is parseListQuery for booleans (e.g.
// ?exclude_from_colocation=true,false). Only "true"/"false" (any case)
// are accepted — not strconv.ParseBool's 1/0/t/f — matching the JSON
// body, which only takes real booleans.
func parseBoolListQuery(c *gin.Context, key string) ([]bool, error) {
	raw := parseListQuery(c, key)
	if raw == nil {
		return nil, nil
	}
	out := make([]bool, 0, len(raw))
	for _, r := range raw {
		switch {
		case strings.EqualFold(r, "true"):
			out = append(out, true)
		case strings.EqualFold(r, "false"):
			out = append(out, false)
		default:
			return nil, fmt.Errorf("invalid %s: %q (want true or false)", key, r)
		}
	}
	return out, nil
}

// compactStrings trims every entry and drops blank ones, returning nil
// (not an empty slice) when nothing is left.
func compactStrings(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
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

// parseAgencyIDStrings parses the home_agency filter's agency UUID strings,
// from either the query string or the body. Blank entries are skipped; nil
// means none.
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

// paginationFromValues resolves page/page size for both the query string
// (page, page_size — see parsePagination) and the body (pagination.page,
// pagination.limit). nil means absent. Page size: absent or negative →
// defaultPageSize; 0 → allPageSize (no pagination: every row, as page 1);
// above maxPageSize → capped at maxPageSize (not silently reset to the
// default). Page: absent or below 1 → defaultPage.
func paginationFromValues(page, pageSize *int) (int, int) {
	p := defaultPage
	ps := defaultPageSize
	if page != nil && *page > 0 {
		p = *page
	}
	if pageSize != nil {
		switch {
		case *pageSize == allPageSize:
			return defaultPage, allPageSize
		case *pageSize > maxPageSize:
			ps = maxPageSize
		case *pageSize > 0:
			ps = *pageSize
		}
	}
	return p, ps
}
