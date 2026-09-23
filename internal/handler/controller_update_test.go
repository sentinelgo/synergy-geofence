package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	pkg "github.com/sentinelgo/synergy-geofence/internal"
	"github.com/sentinelgo/synergy-geofence/internal/database"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/events"

	activitylogger "github.com/sentinelgo/synergy-common/pkg/activity-logger"
	pb "github.com/sentinelgo/synergy-common/pkg/activity-logger/proto"
	"github.com/sentinelgo/synergy-common/pkg/config"
)

// fakeGeofenceDbAdapter is a minimal database.GeofenceDbAdapter test double
// for controller-level (HTTP handler) tests. It embeds the interface as a
// nil value so only the methods a given test actually exercises need
// overriding below — calling anything else panics on the nil embedded
// interface, which is deliberate: an un-anticipated call fails loudly
// instead of silently no-op-ing.
type fakeGeofenceDbAdapter struct {
	database.GeofenceDbAdapter

	findByID              func(ctx context.Context, agencyID, id uuid.UUID) (*dbmodel.Geofence, error)
	findByAgencyAndClient func(ctx context.Context, agencyID uuid.UUID, clientID *uuid.UUID, status *dbmodel.GeofenceStatus, agencyIDs []uuid.UUID) ([]*dbmodel.Geofence, error)
	update                func(ctx context.Context, g *dbmodel.Geofence, selectFields ...any) error
	create                func(ctx context.Context, g *dbmodel.Geofence) error
	delete_               func(ctx context.Context, agencyID, id, userID uuid.UUID) error
}

func (f *fakeGeofenceDbAdapter) FindByID(ctx context.Context, agencyID, id uuid.UUID) (*dbmodel.Geofence, error) {
	return f.findByID(ctx, agencyID, id)
}

func (f *fakeGeofenceDbAdapter) UpdateGeofence(ctx context.Context, g *dbmodel.Geofence, selectFields ...any) error {
	return f.update(ctx, g, selectFields...)
}

func (f *fakeGeofenceDbAdapter) CreateGeofence(ctx context.Context, g *dbmodel.Geofence) error {
	return f.create(ctx, g)
}

func (f *fakeGeofenceDbAdapter) DeleteGeofence(ctx context.Context, agencyID, id, userID uuid.UUID) error {
	return f.delete_(ctx, agencyID, id, userID)
}

func (f *fakeGeofenceDbAdapter) FindByAgencyAndClient(ctx context.Context, agencyID uuid.UUID, clientID *uuid.UUID, status *dbmodel.GeofenceStatus, agencyIDs []uuid.UUID) ([]*dbmodel.Geofence, error) {
	return f.findByAgencyAndClient(ctx, agencyID, clientID, status, agencyIDs)
}

var _ database.GeofenceDbAdapter = (*fakeGeofenceDbAdapter)(nil)

// fakeActivityLogger is a minimal activitylogger.Logger test double that
// records every request handed to it. This mirrors internal/audit's own
// (unexported, so not reusable here) captureLogger, but one level up: these
// tests assert on the audit events a real HTTP handler call produces
// end-to-end — including the original/updated wiring into
// geofenceFieldChanges — not just what LogGeofenceUpdated produces in
// isolation given already-built FieldChange values.
type fakeActivityLogger struct {
	reqs []*pb.ActivityLogRequest
}

func (f *fakeActivityLogger) Log(_ context.Context, req *pb.ActivityLogRequest) {
	f.reqs = append(f.reqs, req)
}
func (f *fakeActivityLogger) Close() error { return nil }

var _ activitylogger.Logger = (*fakeActivityLogger)(nil)

// newTestGinContext builds a *gin.Context for a handler call, wiring up
// what withAgencyId/claimsValidationMiddleware would have set: the
// resolved agency id (via pkg.AgencyIDKey) and, for routes with a
// :geofence_id path param, the given params. No claims are set, matching
// the dev-only JWT-verification-disabled bypass every other test in this
// package (see audit_actor_test.go) already relies on.
func newTestGinContext(t *testing.T, method, target, body string, params gin.Params, agencyID uuid.UUID) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Params = params
	c.Set(pkg.AgencyIDKey(), agencyID)
	return c, w
}

func newTestController(datasource database.GeofenceDbAdapter, activityLog activitylogger.Logger) *GeofenceController {
	return NewGeofenceController(datasource, &config.Config{}, events.NoopPublisher{}, activityLog, http.DefaultClient)
}

// TestUpdateGeofence_RadiusChangeOnly_AuditsRadiusField is the end-to-end
// regression test for the reported bug, exercised through the real HTTP
// handler rather than geoJSONFieldChange in isolation — it guards the
// wiring at UpdateGeofence's call site (original/updated arguments,
// diff/geofenceFieldChanges plumbing) that a unit test of
// geoJSONFieldChange alone cannot catch.
func TestUpdateGeofence_RadiusChangeOnly_AuditsRadiusField(t *testing.T) {
	agencyID := uuid.New()
	geofenceID := uuid.New()
	createdBy := uuid.New()

	existing := &dbmodel.Geofence{
		MutableModel:          dbmodel.DefaultMutableModel(),
		AgencyID:              agencyID,
		Name:                  "Costco Wholesale",
		Type:                  dbmodel.GeofenceTypeCircle,
		GeoJSON:               `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`,
		Status:                dbmodel.GeofenceStatusActive,
		ExcludeFromColocation: false,
		CreatedBy:             createdBy,
	}
	existing.ID = geofenceID
	existing.Version = 1

	datasource := &fakeGeofenceDbAdapter{
		findByID: func(_ context.Context, gotAgencyID, gotID uuid.UUID) (*dbmodel.Geofence, error) {
			if gotAgencyID != agencyID || gotID != geofenceID {
				t.Fatalf("FindByID(%v, %v), want (%v, %v)", gotAgencyID, gotID, agencyID, geofenceID)
			}
			cp := *existing
			return &cp, nil
		},
		update: func(_ context.Context, g *dbmodel.Geofence, _ ...any) error {
			return nil
		},
	}
	activityLog := &fakeActivityLogger{}
	ctrl := newTestController(datasource, activityLog)

	body := `{"geo_json":{"type":"Circle","coordinates":[-84.388,33.749],"radius":750},"user_id":"` + uuid.New().String() + `","version":1}`
	c, w := newTestGinContext(t, http.MethodPatch, "/agencies/"+agencyID.String()+"/geofences/"+geofenceID.String(), body,
		gin.Params{{Key: "geofence_id", Value: geofenceID.String()}}, agencyID)

	ctrl.UpdateGeofence(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	var modified *pb.ActivityLogRequest
	for _, req := range activityLog.reqs {
		if req.GetCategory() == pb.EventCategory_AGENCY_KNOWN_LOCATION_MODIFIED {
			modified = req
		}
	}
	if modified == nil {
		t.Fatalf("no AGENCY_KNOWN_LOCATION_MODIFIED event among %d captured", len(activityLog.reqs))
	}

	wantMessage := "Agency Configuration: Known Location Costco Wholesale Modified Radius from 500 m to 750 m"
	if modified.GetMessage() != wantMessage {
		t.Errorf("Message = %q, want %q", modified.GetMessage(), wantMessage)
	}
}

// TestUpdateGeofence_ExcludeFromColocationOnly_AuditsExcludedCategory is the
// end-to-end regression test for the "exclude from colocation report is not
// tracking" report: toggling only that flag must still produce exactly one
// audit event, under the dedicated EXCLUDED/INCLUDED category the
// downstream Known Location audit view filters on.
func TestUpdateGeofence_ExcludeFromColocationOnly_AuditsExcludedCategory(t *testing.T) {
	agencyID := uuid.New()
	geofenceID := uuid.New()

	existing := &dbmodel.Geofence{
		MutableModel:          dbmodel.DefaultMutableModel(),
		AgencyID:              agencyID,
		Name:                  "Downtown Office",
		Type:                  dbmodel.GeofenceTypeCircle,
		GeoJSON:               `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`,
		Status:                dbmodel.GeofenceStatusActive,
		ExcludeFromColocation: false,
		CreatedBy:             uuid.New(),
	}
	existing.ID = geofenceID
	existing.Version = 1

	datasource := &fakeGeofenceDbAdapter{
		findByID: func(context.Context, uuid.UUID, uuid.UUID) (*dbmodel.Geofence, error) {
			cp := *existing
			return &cp, nil
		},
		update: func(context.Context, *dbmodel.Geofence, ...any) error { return nil },
	}
	activityLog := &fakeActivityLogger{}
	ctrl := newTestController(datasource, activityLog)

	body := `{"exclude_from_colocation":true,"user_id":"` + uuid.New().String() + `","version":1}`
	c, w := newTestGinContext(t, http.MethodPatch, "/agencies/"+agencyID.String()+"/geofences/"+geofenceID.String(), body,
		gin.Params{{Key: "geofence_id", Value: geofenceID.String()}}, agencyID)

	ctrl.UpdateGeofence(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if len(activityLog.reqs) != 1 {
		t.Fatalf("Log was called %d times, want 1 (%v)", len(activityLog.reqs), activityLog.reqs)
	}

	req := activityLog.reqs[0]
	if req.GetCategory() != pb.EventCategory_AGENCY_KNOWN_LOCATION_EXCLUDED {
		t.Errorf("Category = %v, want AGENCY_KNOWN_LOCATION_EXCLUDED", req.GetCategory())
	}
	wantMessage := "Agency Configuration: Known Location Downtown Office Excluded from Co-Location Report."
	if req.GetMessage() != wantMessage {
		t.Errorf("Message = %q, want %q", req.GetMessage(), wantMessage)
	}
}

// TestCreateGeofence_AuditsAddedCategory and
// TestDeleteGeofence_AuditsDeletedCategory are smoke tests confirming the
// same fake-adapter/fake-logger harness wires Create/Delete's audit calls
// correctly too, now that building it was already required for Update's
// regression tests above.
func TestCreateGeofence_AuditsAddedCategory(t *testing.T) {
	agencyID := uuid.New()

	datasource := &fakeGeofenceDbAdapter{
		create: func(_ context.Context, g *dbmodel.Geofence) error { return nil },
	}
	activityLog := &fakeActivityLogger{}
	ctrl := newTestController(datasource, activityLog)

	body := `{"name":"New Depot","geo_json":{"type":"Circle","coordinates":[-84.388,33.749],"radius":100},"status":"active","user_id":"` + uuid.New().String() + `"}`
	c, w := newTestGinContext(t, http.MethodPost, "/agencies/"+agencyID.String()+"/geofences", body, nil, agencyID)

	ctrl.CreateGeofence(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	if len(activityLog.reqs) != 1 {
		t.Fatalf("Log was called %d times, want 1", len(activityLog.reqs))
	}
	if got := activityLog.reqs[0].GetCategory(); got != pb.EventCategory_AGENCY_KNOWN_LOCATION_ADDED {
		t.Errorf("Category = %v, want AGENCY_KNOWN_LOCATION_ADDED", got)
	}
}

func TestDeleteGeofence_AuditsDeletedCategory(t *testing.T) {
	agencyID := uuid.New()
	geofenceID := uuid.New()
	userID := uuid.New()

	existing := &dbmodel.Geofence{Name: "Old Depot", AgencyID: agencyID}
	existing.ID = geofenceID

	datasource := &fakeGeofenceDbAdapter{
		findByID: func(context.Context, uuid.UUID, uuid.UUID) (*dbmodel.Geofence, error) {
			cp := *existing
			return &cp, nil
		},
		delete_: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error { return nil },
	}
	activityLog := &fakeActivityLogger{}
	ctrl := newTestController(datasource, activityLog)

	target := "/agencies/" + agencyID.String() + "/geofences/" + geofenceID.String() + "?user_id=" + userID.String()
	c, w := newTestGinContext(t, http.MethodDelete, target, "",
		gin.Params{{Key: "geofence_id", Value: geofenceID.String()}}, agencyID)

	ctrl.DeleteGeofence(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if len(activityLog.reqs) != 1 {
		t.Fatalf("Log was called %d times, want 1", len(activityLog.reqs))
	}
	if got := activityLog.reqs[0].GetCategory(); got != pb.EventCategory_AGENCY_KNOWN_LOCATION_DELETED {
		t.Errorf("Category = %v, want AGENCY_KNOWN_LOCATION_DELETED", got)
	}
}

// listResponse mirrors ListGeofences' JSON envelope (pkg.DataKey: items,
// "meta": {...}) — just enough of it for these tests to assert on names,
// ordering, and the reported total.
type listResponse struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data"`
	Meta struct {
		Total int `json:"total"`
		Page  int `json:"page"`
		Limit int `json:"limit"`
		Pages int `json:"pages"`
	} `json:"meta"`
}

func decodeListResponse(t *testing.T, w *httptest.ResponseRecorder) listResponse {
	t.Helper()
	var resp listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body = %s", err, w.Body.String())
	}
	return resp
}

func namedGeofence(agencyID uuid.UUID, name string) *dbmodel.Geofence {
	g := &dbmodel.Geofence{
		MutableModel: dbmodel.DefaultMutableModel(),
		AgencyID:     agencyID,
		Name:         name,
		Type:         dbmodel.GeofenceTypeCircle,
		GeoJSON:      `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500}`,
		Status:       dbmodel.GeofenceStatusActive,
	}
	g.ID = uuid.New()
	return g
}

// TestListGeofences_FiltersByAgencyClientScopeAndSearch confirms
// FindByAgencyAndClient is invoked with only the structural filters
// (agency/client/status/agencyIDs — no page/sort/query, which are now
// applied entirely in memory afterward, see ListGeofences), and that the
// free-text query param actually narrows the response to matching rows.
func TestListGeofences_FiltersByAgencyClientScopeAndSearch(t *testing.T) {
	agencyID := uuid.New()
	clientID := uuid.New()

	match := namedGeofence(agencyID, "Downtown Office")
	noMatch := namedGeofence(agencyID, "Uptown Depot")

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(_ context.Context, gotAgencyID uuid.UUID, gotClientID *uuid.UUID, gotStatus *dbmodel.GeofenceStatus, gotAgencyIDs []uuid.UUID) ([]*dbmodel.Geofence, error) {
			if gotAgencyID != agencyID {
				t.Fatalf("agencyID = %v, want %v", gotAgencyID, agencyID)
			}
			if gotClientID == nil || *gotClientID != clientID {
				t.Fatalf("clientID = %v, want %v", gotClientID, clientID)
			}
			if gotStatus != nil {
				t.Fatalf("status = %v, want nil", gotStatus)
			}
			if len(gotAgencyIDs) != 1 || gotAgencyIDs[0] != agencyID {
				t.Fatalf("agencyIDs = %v, want [%v]", gotAgencyIDs, agencyID)
			}
			return []*dbmodel.Geofence{match, noMatch}, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet,
		"/agencies/"+agencyID.String()+"/geofences?q=Downtown&client_id="+clientID.String(),
		"",
		nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	resp := decodeListResponse(t, w)
	if resp.Meta.Total != 1 || len(resp.Data) != 1 || resp.Data[0].Name != "Downtown Office" {
		t.Fatalf("got %+v, want exactly one match named %q", resp, "Downtown Office")
	}
}

// TestListGeofences_ShortQueryMatchesNothing mirrors sso-agency-service's
// buildSearchCriteria: a query shorter than search.MinQueryLen (3) is an
// invalid search — a 200 with zero results, not a match-everything
// fallback and not a 400.
func TestListGeofences_ShortQueryMatchesNothing(t *testing.T) {
	agencyID := uuid.New()
	match := namedGeofence(agencyID, "Downtown Office")

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return []*dbmodel.Geofence{match}, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences?q=Do", "", nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	resp := decodeListResponse(t, w)
	if resp.Meta.Total != 0 || len(resp.Data) != 0 {
		t.Fatalf("got %+v, want zero results for a query shorter than MinQueryLen", resp)
	}
}

// TestListGeofences_SearchMatchesExcludeFromColocation confirms
// exclude_from_colocation is searchable as plain "true"/"false" text (see
// search.MatchesQuery), not just sortable/filterable.
func TestListGeofences_SearchMatchesExcludeFromColocation(t *testing.T) {
	agencyID := uuid.New()
	excluded := namedGeofence(agencyID, "Parole Office")
	excluded.ExcludeFromColocation = true
	included := namedGeofence(agencyID, "Downtown Office")
	included.ExcludeFromColocation = false

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return []*dbmodel.Geofence{excluded, included}, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences?q=true", "", nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	resp := decodeListResponse(t, w)
	if resp.Meta.Total != 1 || len(resp.Data) != 1 || resp.Data[0].Name != "Parole Office" {
		t.Fatalf("got %+v, want exactly one match named %q", resp, "Parole Office")
	}
}

// TestListGeofences_IgnoresAgencyIDsParam guards the removed agency_ids
// param: it used to replace the agency scope as-is, letting a viewer of
// one agency list another's geofences. The scope must now always be the
// server-resolved one (here, just the path agency), whatever the request
// sends — via the query string or alongside a body.
func TestListGeofences_IgnoresAgencyIDsParam(t *testing.T) {
	agencyID := uuid.New()
	otherAgencyID := uuid.New()

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ *dbmodel.GeofenceStatus, gotAgencyIDs []uuid.UUID) ([]*dbmodel.Geofence, error) {
			if len(gotAgencyIDs) != 1 || gotAgencyIDs[0] != agencyID {
				t.Fatalf("agencyIDs = %v, want only the path agency [%v]", gotAgencyIDs, agencyID)
			}
			return nil, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	for _, tc := range []struct{ name, target, body string }{
		{name: "query", target: "/agencies/" + agencyID.String() + "/geofences?agency_ids=" + otherAgencyID.String()},
		{name: "query, malformed value", target: "/agencies/" + agencyID.String() + "/geofences?agency_ids=not-a-uuid"},
		{name: "alongside a body", target: "/agencies/" + agencyID.String() + "/geofences?agency_ids=" + otherAgencyID.String(), body: `{"search":{"name":"x"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestGinContext(t, http.MethodGet, tc.target, tc.body, nil, agencyID)

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestListGeofences_SubagencyWithoutServiceClaimsIsForbiddenOnly confirms
// that when resolveListAgencyIDs itself aborts with 403 (includeSubagency
// but no service claims), ListGeofences stops there — no query, and no
// geofence list appended to the 403 body.
func TestListGeofences_SubagencyWithoutServiceClaimsIsForbiddenOnly(t *testing.T) {
	agencyID := uuid.New()
	called := false
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			called = true
			return []*dbmodel.Geofence{namedGeofence(agencyID, "Leaked")}, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences?includeSubagency=true", "", nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("datasource should not be called after a 403")
	}
	if strings.Contains(w.Body.String(), "Leaked") || strings.Contains(w.Body.String(), `"data"`) {
		t.Fatalf("403 body = %s, want only the error", w.Body.String())
	}
}

// TestListGeofences_SortsByAddress confirms sort_by/order are applied in
// memory, over the DB layer's unsorted result set, rather than being passed
// through to it.
func TestListGeofences_SortsByAddress(t *testing.T) {
	agencyID := uuid.New()

	gA := namedGeofence(agencyID, "A")
	gA.GeoJSON = `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,"address":{"street1":"1 Alpha St"}}`
	gZ := namedGeofence(agencyID, "Z")
	gZ.GeoJSON = `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,"address":{"street1":"9 Zulu St"}}`

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return []*dbmodel.Geofence{gA, gZ}, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet,
		"/agencies/"+agencyID.String()+"/geofences?sort_by=address&order=DESC", "", nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	resp := decodeListResponse(t, w)
	if len(resp.Data) != 2 || resp.Data[0].Name != "Z" || resp.Data[1].Name != "A" {
		t.Fatalf("got order %+v, want [Z, A] (desc by address)", resp.Data)
	}
}

// TestListGeofences_RejectsSortByHomeAgency confirms home_agency is no
// longer a sort_by value (it's an agency UUID, with nothing meaningful to
// sort by).
func TestListGeofences_RejectsSortByHomeAgency(t *testing.T) {
	agencyID := uuid.New()
	ctrl := newTestController(&fakeGeofenceDbAdapter{}, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet,
		"/agencies/"+agencyID.String()+"/geofences?sort_by=home_agency", "", nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

// TestListGeofences_AcceptsBodyRequest exercises the LIST-body envelope
// (mirroring sso-user-service's UserListRequest): search/pagination/data
// all supplied in the JSON body instead of query params, on the same route
// that otherwise reads query strings. FindByAgencyAndClient only receives
// the structural filters (status, resolved agencyIDs) — search/sort/
// pagination from the body are all applied afterward in memory.
func TestListGeofences_AcceptsBodyRequest(t *testing.T) {
	agencyID := uuid.New()

	matchB := namedGeofence(agencyID, "Downtown Office B")
	matchA := namedGeofence(agencyID, "Downtown Office A")
	noMatch := namedGeofence(agencyID, "Uptown Depot")

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(_ context.Context, gotAgencyID uuid.UUID, gotClientID *uuid.UUID, gotStatus *dbmodel.GeofenceStatus, gotAgencyIDs []uuid.UUID) ([]*dbmodel.Geofence, error) {
			if gotAgencyID != agencyID {
				t.Fatalf("agencyID = %v, want %v", gotAgencyID, agencyID)
			}
			if gotClientID != nil {
				t.Fatalf("clientID = %v, want nil (not a body field)", gotClientID)
			}
			if gotStatus == nil || *gotStatus != dbmodel.GeofenceStatusActive {
				t.Fatalf("status = %v, want active", gotStatus)
			}
			// include_subagency isn't set in this request, so agencyIDs
			// resolves to just the path's own agency (no hierarchy lookup).
			if len(gotAgencyIDs) != 1 || gotAgencyIDs[0] != agencyID {
				t.Fatalf("agencyIDs = %v, want [%v]", gotAgencyIDs, agencyID)
			}
			return []*dbmodel.Geofence{matchB, matchA, noMatch}, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	body := `{
		"search": {"any": "Downtown Office"},
		"pagination": {"limit": 50, "page": 1},
		"data": {
			"status": "active",
			"sort_by": "name",
			"order": "asc"
		}
	}`
	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences", body, nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	resp := decodeListResponse(t, w)
	if resp.Meta.Total != 2 || len(resp.Data) != 2 ||
		resp.Data[0].Name != "Downtown Office A" || resp.Data[1].Name != "Downtown Office B" {
		t.Fatalf("got %+v, want [Downtown Office A, Downtown Office B] sorted asc, excluding Uptown Depot", resp.Data)
	}
}

// TestListGeofences_FallsBackToQueryWhenBodyLacksEnvelopeKeys confirms a
// JSON body that isn't the search/pagination/data envelope (e.g. some
// unrelated payload, or one sent by mistake) doesn't get treated as a list
// request body — ListGeofences still reads query params (and applies their
// sort) in that case.
func TestListGeofences_FallsBackToQueryWhenBodyLacksEnvelopeKeys(t *testing.T) {
	agencyID := uuid.New()

	gZ := namedGeofence(agencyID, "Zeta")
	gA := namedGeofence(agencyID, "Alpha")

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return []*dbmodel.Geofence{gZ, gA}, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet,
		"/agencies/"+agencyID.String()+"/geofences?sort_by=name", `{"unrelated":"payload"}`, nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	resp := decodeListResponse(t, w)
	if len(resp.Data) != 2 || resp.Data[0].Name != "Alpha" || resp.Data[1].Name != "Zeta" {
		t.Fatalf("got order %+v, want [Alpha, Zeta] (query fallback sort_by=name)", resp.Data)
	}
}

func TestListGeofences_RejectsInvalidBodySortBy(t *testing.T) {
	agencyID := uuid.New()
	called := false
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			called = true
			return nil, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	body := `{"data": {"sort_by": "bogus"}}`
	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences", body, nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("datasource should not be called for an invalid body sort_by")
	}
}

func TestListGeofences_RejectsMalformedBody(t *testing.T) {
	agencyID := uuid.New()
	called := false
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			called = true
			return nil, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	body := `{"data": `
	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences", body, nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("datasource should not be called for a malformed body")
	}
}

func TestListGeofences_RejectsInvalidSortBy(t *testing.T) {
	agencyID := uuid.New()
	called := false
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			called = true
			return nil, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences?sort_by=bogus", "", nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("datasource should not be called for an invalid sort_by")
	}
}

// fieldFilterFixtures returns three geofences that differ by name, address
// and exclude_from_colocation, for the per-field AND filter tests below.
// Each fixture belongs to a different (sub)agency, so home_agency — the
// owning agency_id — can tell them apart.
const (
	fixtureAgencyAtl  = "11111111-1111-1111-1111-111111111111"
	fixtureAgencyBos  = "22222222-2222-2222-2222-222222222222"
	fixtureAgencyCour = "33333333-3333-3333-3333-333333333333"
)

func fieldFilterFixtures(agencyID uuid.UUID) []*dbmodel.Geofence {
	paroleAtl := namedGeofence(agencyID, "Parole Office Atlanta")
	paroleAtl.GeoJSON = `{"type":"Circle","coordinates":[-84.388,33.749],"radius":500,"address":{"street1":"1 Peachtree St","city":"Atlanta"}}`
	paroleAtl.ExcludeFromColocation = true
	paroleAtl.AgencyID = uuid.MustParse(fixtureAgencyAtl)

	paroleBos := namedGeofence(agencyID, "Parole Office Boston")
	paroleBos.GeoJSON = `{"type":"Circle","coordinates":[-71.05,42.36],"radius":500,"address":{"street1":"2 Main St","city":"Boston"}}`
	paroleBos.ExcludeFromColocation = true
	paroleBos.AgencyID = uuid.MustParse(fixtureAgencyBos)

	courtAtl := namedGeofence(agencyID, "Courthouse")
	courtAtl.GeoJSON = `{"type":"Circle","coordinates":[-84.39,33.75],"radius":500,"address":{"street1":"3 Main St","city":"Atlanta"}}`
	courtAtl.ExcludeFromColocation = false
	courtAtl.AgencyID = uuid.MustParse(fixtureAgencyCour)

	return []*dbmodel.Geofence{paroleAtl, paroleBos, courtAtl}
}

// TestListGeofences_FieldFiltersAreANDed confirms name/address/
// exclude_from_colocation query params each narrow the result (AND),
// unlike q which matches any field.
func TestListGeofences_FieldFiltersAreANDed(t *testing.T) {
	agencyID := uuid.New()
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return fieldFilterFixtures(agencyID), nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{name: "name only", query: "name=parole", want: []string{"Parole Office Atlanta", "Parole Office Boston"}},
		{name: "name AND address", query: "name=parole&address=atlanta", want: []string{"Parole Office Atlanta"}},
		{name: "address AND not excluded", query: "address=main&exclude_from_colocation=false", want: []string{"Courthouse"}},
		{name: "field filter AND q", query: "q=boston&exclude_from_colocation=true", want: []string{"Parole Office Boston"}},
		{name: "no row satisfies all", query: "name=courthouse&exclude_from_colocation=true", want: nil},
		{name: "home_agency single agency", query: "home_agency=" + fixtureAgencyAtl + "", want: []string{"Parole Office Atlanta"}},
		{name: "home_agency comma list is OR", query: "home_agency=" + fixtureAgencyAtl + "," + fixtureAgencyCour + "", want: []string{"Courthouse", "Parole Office Atlanta"}},
		{name: "home_agency repeated is OR", query: "home_agency=" + fixtureAgencyBos + "&home_agency=" + fixtureAgencyCour + "", want: []string{"Courthouse", "Parole Office Boston"}},
		{name: "home_agency list AND name", query: "home_agency=" + fixtureAgencyAtl + "," + fixtureAgencyBos + "," + fixtureAgencyCour + "&name=parole", want: []string{"Parole Office Atlanta", "Parole Office Boston"}},
		{name: "home_agency outside the fetched rows matches nothing", query: "home_agency=" + uuid.NewString(), want: nil},
		{name: "exclude true,false matches both", query: "exclude_from_colocation=true,false", want: []string{"Courthouse", "Parole Office Atlanta", "Parole Office Boston"}},
		{name: "exclude repeated true and false", query: "exclude_from_colocation=true&exclude_from_colocation=false&address=atlanta", want: []string{"Courthouse", "Parole Office Atlanta"}},
		{name: "exclude is case-insensitive", query: "exclude_from_colocation=FALSE", want: []string{"Courthouse"}},
		{name: "mixed separators and blanks", query: "home_agency=" + fixtureAgencyAtl + ",,%20" + fixtureAgencyBos + "&home_agency=", want: []string{"Parole Office Atlanta", "Parole Office Boston"}},
		{name: "whitespace-only name is no filter", query: "name=%20%20", want: []string{"Courthouse", "Parole Office Atlanta", "Parole Office Boston"}},
		{name: "padded name is trimmed", query: "name=%20parole%20", want: []string{"Parole Office Atlanta", "Parole Office Boston"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestGinContext(t, http.MethodGet,
				"/agencies/"+agencyID.String()+"/geofences?sort_by=name&"+tc.query, "", nil, agencyID)

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			resp := decodeListResponse(t, w)
			got := make([]string, 0, len(resp.Data))
			for _, d := range resp.Data {
				got = append(got, d.Name)
			}
			if resp.Meta.Total != len(tc.want) || strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v (total %d), want %v", got, resp.Meta.Total, tc.want)
			}
		})
	}
}

// TestListGeofences_BodyFieldFiltersAreANDed is the JSON-body equivalent:
// search.name/address/exclude_from_colocation AND together with search.any.
func TestListGeofences_BodyFieldFiltersAreANDed(t *testing.T) {
	agencyID := uuid.New()
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return fieldFilterFixtures(agencyID), nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{name: "scalar values", body: `{"search":{"any":"office","address":"atlanta","exclude_from_colocation":true}}`, want: []string{"Parole Office Atlanta"}},
		{name: "scalar home_agency", body: `{"search":{"home_agency":"` + fixtureAgencyBos + `"}}`, want: []string{"Parole Office Boston"}},
		{name: "home_agency array is OR", body: `{"search":{"home_agency":["` + fixtureAgencyAtl + `","` + fixtureAgencyCour + `"]},"data":{"sort_by":"name"}}`, want: []string{"Courthouse", "Parole Office Atlanta"}},
		{name: "exclude array true and false", body: `{"search":{"address":"atlanta","exclude_from_colocation":[true,false]},"data":{"sort_by":"name"}}`, want: []string{"Courthouse", "Parole Office Atlanta"}},
		{name: "null home_agency and exclude are no filter", body: `{"search":{"home_agency":null,"exclude_from_colocation":null},"data":{"sort_by":"name"}}`, want: []string{"Courthouse", "Parole Office Atlanta", "Parole Office Boston"}},
		{name: "empty arrays are no filter", body: `{"search":{"home_agency":[],"exclude_from_colocation":[]},"data":{"sort_by":"name"}}`, want: []string{"Courthouse", "Parole Office Atlanta", "Parole Office Boston"}},
		{name: "blank home_agency entries are no filter", body: `{"search":{"home_agency":["", "  "]},"data":{"sort_by":"name"}}`, want: []string{"Courthouse", "Parole Office Atlanta", "Parole Office Boston"}},
		{name: "whitespace-only name is no filter", body: `{"search":{"name":"   "},"data":{"sort_by":"name"}}`, want: []string{"Courthouse", "Parole Office Atlanta", "Parole Office Boston"}},
		{name: "everything combined", body: `{"search":{"any":"office","home_agency":["` + fixtureAgencyAtl + `","` + fixtureAgencyBos + `"],"exclude_from_colocation":[true]},"data":{"sort_by":"name"}}`, want: []string{"Parole Office Atlanta", "Parole Office Boston"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences", tc.body, nil, agencyID)

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			resp := decodeListResponse(t, w)
			got := make([]string, 0, len(resp.Data))
			for _, d := range resp.Data {
				got = append(got, d.Name)
			}
			if resp.Meta.Total != len(tc.want) || strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v (total %d), want %v", got, resp.Meta.Total, tc.want)
			}
		})
	}
}

// TestListGeofences_RejectsWrongTypeBodySearchField confirms a body
// home_agency/exclude_from_colocation of the wrong JSON type, or a
// home_agency entry that isn't an agency UUID, is a 400.
func TestListGeofences_RejectsWrongTypeBodySearchField(t *testing.T) {
	agencyID := uuid.New()
	ctrl := newTestController(&fakeGeofenceDbAdapter{}, &fakeActivityLogger{})

	for _, body := range []string{
		`{"search":{"home_agency":123}}`,
		`{"search":{"exclude_from_colocation":"yes"}}`,
		`{"search":{"exclude_from_colocation":[true,"no"]}}`,
		`{"search":{"exclude_from_colocation":[true,null]}}`,
		`{"search":{"home_agency":["` + fixtureAgencyAtl + `",null]}}`,
		`{"search":{"home_agency":"SYN1"}}`,
		`{"search":{"home_agency":["` + fixtureAgencyAtl + `","not-a-uuid"]}}`,
	} {
		c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences", body, nil, agencyID)

		ctrl.ListGeofences(c)

		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; body = %s", body, w.Code, w.Body.String())
		}
	}
}

// TestListGeofences_RejectsInvalidExcludeFromColocation confirms only
// true/false are accepted in the query string — not strconv.ParseBool's
// 1/0/t/f shorthands, which the JSON body doesn't accept either.
func TestListGeofences_RejectsInvalidExcludeFromColocation(t *testing.T) {
	agencyID := uuid.New()
	called := false
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			called = true
			return nil, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	for _, value := range []string{"true,maybe", "1", "t", "F"} {
		c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences?exclude_from_colocation="+value, "", nil, agencyID)

		ctrl.ListGeofences(c)

		if w.Code != http.StatusBadRequest {
			t.Errorf("exclude_from_colocation=%s: status = %d, want 400; body = %s", value, w.Code, w.Body.String())
		}
	}
	if called {
		t.Fatal("datasource should not be called for an invalid exclude_from_colocation")
	}
}

func TestListGeofences_RejectsInvalidHomeAgencyQuery(t *testing.T) {
	agencyID := uuid.New()
	ctrl := newTestController(&fakeGeofenceDbAdapter{}, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet,
		"/agencies/"+agencyID.String()+"/geofences?home_agency="+fixtureAgencyAtl+",SYN1", "", nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "home_agency") {
		t.Fatalf("body = %s, want the error to name home_agency", w.Body.String())
	}
}

// TestListGeofences_BodyStillHonorsClientIDQuery pins the body-vs-query
// rule: with a body present, client_id (which has no body field) is still
// read from the query string, while every other query param — here name —
// is ignored in favor of the body.
func TestListGeofences_BodyStillHonorsClientIDQuery(t *testing.T) {
	agencyID := uuid.New()
	clientID := uuid.New()

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(_ context.Context, _ uuid.UUID, gotClientID *uuid.UUID, _ *dbmodel.GeofenceStatus, gotAgencyIDs []uuid.UUID) ([]*dbmodel.Geofence, error) {
			if gotClientID == nil || *gotClientID != clientID {
				t.Fatalf("clientID = %v, want %v from the query string", gotClientID, clientID)
			}
			if len(gotAgencyIDs) != 1 || gotAgencyIDs[0] != agencyID {
				t.Fatalf("agencyIDs = %v, want [%v]", gotAgencyIDs, agencyID)
			}
			return fieldFilterFixtures(agencyID), nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet,
		"/agencies/"+agencyID.String()+"/geofences?client_id="+clientID.String()+"&name=courthouse",
		`{"search":{"name":"parole"},"data":{"sort_by":"name"}}`, nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	resp := decodeListResponse(t, w)
	if len(resp.Data) != 2 || resp.Data[0].Name != "Parole Office Atlanta" || resp.Data[1].Name != "Parole Office Boston" {
		t.Fatalf("got %+v, want the body's name=parole matches (query name=courthouse ignored)", resp.Data)
	}
}

func TestListGeofences_BodyRejectsInvalidClientIDQuery(t *testing.T) {
	agencyID := uuid.New()
	ctrl := newTestController(&fakeGeofenceDbAdapter{}, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet,
		"/agencies/"+agencyID.String()+"/geofences?client_id=not-a-uuid", `{"search":{"name":"parole"}}`, nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}
