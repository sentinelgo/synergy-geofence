package handler

import (
	"context"
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

	findByID func(ctx context.Context, agencyID, id uuid.UUID) (*dbmodel.Geofence, error)
	update   func(ctx context.Context, g *dbmodel.Geofence, selectFields ...any) error
	create   func(ctx context.Context, g *dbmodel.Geofence) error
	delete_  func(ctx context.Context, agencyID, id, userID uuid.UUID) error
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
	return NewGeofenceController(datasource, &config.Config{}, events.NoopPublisher{}, activityLog)
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
