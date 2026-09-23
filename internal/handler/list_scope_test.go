package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/auth0/go-jwt-middleware/v3/validator"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"
	dbmodel "github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/sentinelgo/synergy-geofence/internal/events"
	"github.com/spf13/viper"

	"github.com/sentinelgo/synergy-common/pkg/authz/api"
	"github.com/sentinelgo/synergy-common/pkg/authz/api/model/metadata"
	auth "github.com/sentinelgo/synergy-common/pkg/authz/resolver"
	"github.com/sentinelgo/synergy-common/pkg/config"
	jwtpkg "github.com/sentinelgo/synergy-common/pkg/http/jwt"
)

// fakeAgencyTree serves the agency service's m2m endpoints: the tree
// (LIST .../agencies/{root}/tree — root with the given children, flat) and
// single-agency lookups (GET .../agencies/{id}). details optionally gives
// agencies a synergy identifier/name. Setting down makes every request
// fail with 503.
func fakeAgencyTree(t *testing.T, root uuid.UUID, children []uuid.UUID, down bool, details ...agencyInfo) *httptest.Server {
	t.Helper()
	byID := make(map[uuid.UUID]agencyInfo)
	for _, d := range details {
		byID[d.ID] = d
	}
	node := func(id uuid.UUID) map[string]any {
		d := byID[id]
		return map[string]any{"id": id, "name": d.Name, "synergy_identifier": d.SynergyIdentifier, "children": []any{}}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		const prefix = "/api/v1/m2m/agencies/"
		switch {
		case r.Method == "LIST" && r.URL.Path == prefix+root.String()+"/tree":
			rootNode := node(root)
			kids := make([]any, 0, len(children))
			for _, id := range children {
				kids = append(kids, node(id))
			}
			rootNode["children"] = kids
			_ = json.NewEncoder(w).Encode(map[string]any{"data": rootNode})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix):
			id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, prefix))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": node(id)})
		default:
			t.Errorf("unexpected agency-service request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newScopeTestController(datasource *fakeGeofenceDbAdapter, server *httptest.Server) *GeofenceController {
	cfg := &config.Config{Common: config.Common{Agency: config.Agency{BaseUrl: server.URL}}}
	return NewGeofenceController(datasource, cfg, events.NoopPublisher{}, &fakeActivityLogger{}, server.Client())
}

func setServiceClaims(c *gin.Context, roles metadata.Role) {
	md := metadata.NewMetadataPublic()
	md.UserProfile.Roles = &roles
	svc := jwtpkg.NewServiceClaimsWithMetadata(md)
	c.Set(pkg.ClaimsKey(), auth.NewJwt(&validator.ValidatedClaims{CustomClaims: svc}, nil))
}

// TestListGeofences_IncludeSubagencyScope exercises resolveListAgencyIDs
// end to end against a fake agency service: the path agency always comes
// first and only once, uuid.Nil and duplicate hierarchy entries are
// dropped, sentinel admins see the whole hierarchy, other callers only
// agencies they're associated with, and the JWT-verification-disabled dev
// bypass sees everything.
func TestListGeofences_IncludeSubagencyScope(t *testing.T) {
	root, subA, subB := uuid.New(), uuid.New(), uuid.New()
	// Messy hierarchy on purpose: root repeated, a nil id, a duplicate.
	server := fakeAgencyTree(t, root, []uuid.UUID{subA, uuid.Nil, subB, subA, root}, false)

	for _, tc := range []struct {
		name                 string
		roles                *metadata.Role // nil = no claims at all
		verificationDisabled bool
		want                 []uuid.UUID
	}{
		{name: "global admin sees all", roles: &metadata.Role{api.Sentinel: api.GlobalAdmin}, want: []uuid.UUID{root, subA, subB}},
		{name: "system admin sees all", roles: &metadata.Role{api.Sentinel: api.SystemAdmin}, want: []uuid.UUID{root, subA, subB}},
		{name: "read-only admin sees all", roles: &metadata.Role{api.Sentinel: api.ReadOnlyAdmin}, want: []uuid.UUID{root, subA, subB}},
		{name: "agency user sees only associated subagencies", roles: &metadata.Role{root.String(): "agencyadmin", subB.String(): "agencyadmin"}, want: []uuid.UUID{root, subB}},
		{name: "verification disabled, no claims, sees all", verificationDisabled: true, want: []uuid.UUID{root, subA, subB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Set("common.jwt.verification.disabled", tc.verificationDisabled)
			t.Cleanup(func() { viper.Set("common.jwt.verification.disabled", false) })

			var got []uuid.UUID
			datasource := &fakeGeofenceDbAdapter{
				findByAgencyAndClient: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ *dbmodel.GeofenceStatus, agencyIDs []uuid.UUID) ([]*dbmodel.Geofence, error) {
					got = agencyIDs
					return nil, nil
				},
			}
			ctrl := newScopeTestController(datasource, server)

			c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+root.String()+"/geofences?includeSubagency=true", "", nil, root)
			if tc.roles != nil {
				setServiceClaims(c, *tc.roles)
			}

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			// The path agency must come first; subagency order follows the
			// hierarchy walk (map-based, so unordered) and doesn't matter to
			// the IN (...) query — compare the rest as a set.
			if len(got) != len(tc.want) || got[0] != tc.want[0] || !sameUUIDSet(got[1:], tc.want[1:]) {
				t.Fatalf("agency scope = %v, want %v (path agency first, rest in any order)", got, tc.want)
			}
		})
	}
}

func sameUUIDSet(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	byString := func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) }
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, byString)
	slices.SortFunc(b, byString)
	return slices.Equal(a, b)
}

// TestListGeofences_HomeAgencyNarrowsSubagencyScope confirms home_agency
// only narrows the resolved scope: picking one subagency returns just its
// rows, and naming an agency outside the scope returns nothing even though
// that agency has rows.
func TestListGeofences_HomeAgencyNarrowsSubagencyScope(t *testing.T) {
	root, sub, outside := uuid.New(), uuid.New(), uuid.New()
	server := fakeAgencyTree(t, root, []uuid.UUID{sub}, false)

	rootRow := namedGeofence(root, "Root Office")
	subRow := namedGeofence(sub, "Sub Office")
	outsideRow := namedGeofence(outside, "Outside Office")

	// Behaves like the real query: returns only rows whose agency is in scope.
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ *dbmodel.GeofenceStatus, agencyIDs []uuid.UUID) ([]*dbmodel.Geofence, error) {
			var rows []*dbmodel.Geofence
			for _, g := range []*dbmodel.Geofence{rootRow, subRow, outsideRow} {
				if slices.Contains(agencyIDs, g.AgencyID) {
					rows = append(rows, g)
				}
			}
			return rows, nil
		},
	}
	ctrl := newScopeTestController(datasource, server)

	for _, tc := range []struct {
		name       string
		homeAgency string
		want       []string
	}{
		{name: "one subagency", homeAgency: sub.String(), want: []string{"Sub Office"}},
		{name: "path agency and subagency", homeAgency: root.String() + "," + sub.String(), want: []string{"Root Office", "Sub Office"}},
		{name: "agency outside scope", homeAgency: outside.String(), want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestGinContext(t, http.MethodGet,
				"/agencies/"+root.String()+"/geofences?includeSubagency=true&sort_by=name&home_agency="+tc.homeAgency, "", nil, root)
			setServiceClaims(c, metadata.Role{api.Sentinel: api.GlobalAdmin})

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			resp := decodeListResponse(t, w)
			got := make([]string, 0, len(resp.Data))
			for _, d := range resp.Data {
				got = append(got, d.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestListGeofences_AgencyServiceDownIs502 confirms a failed hierarchy
// lookup is reported as an upstream failure, not as a 403 auth error.
func TestListGeofences_AgencyServiceDownIs502(t *testing.T) {
	root := uuid.New()
	server := fakeAgencyTree(t, root, nil, true)
	called := false
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			called = true
			return nil, nil
		},
	}
	ctrl := newScopeTestController(datasource, server)

	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+root.String()+"/geofences?includeSubagency=true", "", nil, root)
	setServiceClaims(c, metadata.Role{api.Sentinel: api.GlobalAdmin})

	ctrl.ListGeofences(c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ERR_AGENCY_SERVICE") {
		t.Fatalf("body = %s, want ERR_AGENCY_SERVICE", w.Body.String())
	}
	if called {
		t.Fatal("datasource should not be called when the scope can't be resolved")
	}
}

// TestListGeofences_BodyRejectsNonUUIDHomeAgency covers a well-typed but
// non-UUID search.home_agency: it's ERR_INVALID_QUERY on home_agency, not
// a body bind error.
func TestListGeofences_BodyRejectsNonUUIDHomeAgency(t *testing.T) {
	agencyID := uuid.New()
	ctrl := newTestController(&fakeGeofenceDbAdapter{}, &fakeActivityLogger{})

	c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences",
		`{"search":{"home_agency":["`+uuid.NewString()+`","SYN123"]}}`, nil, agencyID)

	ctrl.ListGeofences(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "ERR_INVALID_QUERY") || !strings.Contains(body, "home_agency") {
		t.Fatalf("body = %s, want ERR_INVALID_QUERY on home_agency", body)
	}
}

// TestListGeofences_Pagination covers page_size / pagination.limit: 0
// returns every row as a single page (page ignored), above maxPageSize is
// capped at maxPageSize rather than silently reset to the default, and
// absent/negative/non-numeric falls back to defaultPageSize.
func TestListGeofences_Pagination(t *testing.T) {
	agencyID := uuid.New()
	rows := make([]*dbmodel.Geofence, 0, 250)
	for i := 0; i < 250; i++ {
		rows = append(rows, namedGeofence(agencyID, "Location "+uuid.NewString()))
	}
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return rows, nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})
	base := "/agencies/" + agencyID.String() + "/geofences"

	for _, tc := range []struct {
		name                         string
		query, body                  string
		wantItems, wantLimit         int
		wantPage, wantPages, wantTot int
	}{
		{name: "default", query: "", wantItems: 25, wantLimit: 25, wantPage: 1, wantPages: 10, wantTot: 250},
		{name: "page_size=0 returns all", query: "?page_size=0", wantItems: 250, wantLimit: 0, wantPage: 1, wantPages: 1, wantTot: 250},
		{name: "page_size=0 ignores page", query: "?page_size=0&page=3", wantItems: 250, wantLimit: 0, wantPage: 1, wantPages: 1, wantTot: 250},
		{name: "page_size above max is capped, not reset", query: "?page_size=1000", wantItems: 200, wantLimit: 200, wantPage: 1, wantPages: 2, wantTot: 250},
		{name: "capped size, second page", query: "?page_size=1000&page=2", wantItems: 50, wantLimit: 200, wantPage: 2, wantPages: 2, wantTot: 250},
		{name: "negative falls back to default", query: "?page_size=-5", wantItems: 25, wantLimit: 25, wantPage: 1, wantPages: 10, wantTot: 250},
		{name: "non-numeric falls back to default", query: "?page_size=abc", wantItems: 25, wantLimit: 25, wantPage: 1, wantPages: 10, wantTot: 250},
		{name: "body limit 0 returns all", body: `{"pagination":{"limit":0,"page":4}}`, wantItems: 250, wantLimit: 0, wantPage: 1, wantPages: 1, wantTot: 250},
		{name: "body limit above max is capped", body: `{"pagination":{"limit":5000}}`, wantItems: 200, wantLimit: 200, wantPage: 1, wantPages: 2, wantTot: 250},
		{name: "all + filter", query: "?page_size=0&name=nomatch", wantItems: 0, wantLimit: 0, wantPage: 1, wantPages: 0, wantTot: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestGinContext(t, http.MethodGet, base+tc.query, tc.body, nil, agencyID)

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			resp := decodeListResponse(t, w)
			if len(resp.Data) != tc.wantItems || resp.Meta.Limit != tc.wantLimit || resp.Meta.Page != tc.wantPage ||
				resp.Meta.Pages != tc.wantPages || resp.Meta.Total != tc.wantTot {
				t.Fatalf("got items=%d meta=%+v, want items=%d limit=%d page=%d pages=%d total=%d",
					len(resp.Data), resp.Meta, tc.wantItems, tc.wantLimit, tc.wantPage, tc.wantPages, tc.wantTot)
			}
		})
	}
}

// listHomeAgencyResponse is decodeListResponse plus each item's
// home_agency object.
type listHomeAgencyResponse struct {
	Data []struct {
		Name       string `json:"name"`
		HomeAgency *struct {
			ID                uuid.UUID `json:"id"`
			SynergyIdentifier string    `json:"synergy_identifier"`
			Name              string    `json:"name"`
		} `json:"home_agency"`
	} `json:"data"`
}

// TestListGeofences_SortsByHomeAgencySynergyIdentifier confirms
// sort_by=home_agency orders by each owning agency's synergy identifier —
// taken from the hierarchy call includeSubagency already makes, not
// stored — and that each item carries its home_agency details.
func TestListGeofences_SortsByHomeAgencySynergyIdentifier(t *testing.T) {
	root, subA, subC := uuid.New(), uuid.New(), uuid.New()
	// Deliberately not in UUID or name order: root is "SYN-B".
	server := fakeAgencyTree(t, root, []uuid.UUID{subA, subC}, false,
		agencyInfo{ID: root, SynergyIdentifier: "SYN-B", Name: "Root Agency"},
		agencyInfo{ID: subA, SynergyIdentifier: "SYN-A", Name: "Sub A"},
		agencyInfo{ID: subC, SynergyIdentifier: "SYN-C", Name: "Sub C"},
	)
	rows := []*dbmodel.Geofence{
		namedGeofence(subC, "In C"),
		namedGeofence(root, "In Root"),
		namedGeofence(subA, "In A"),
	}
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return slices.Clone(rows), nil
		},
	}
	ctrl := newScopeTestController(datasource, server)

	for _, tc := range []struct {
		order string
		want  []string
	}{
		{order: "asc", want: []string{"In A", "In Root", "In C"}},
		{order: "desc", want: []string{"In C", "In Root", "In A"}},
	} {
		t.Run(tc.order, func(t *testing.T) {
			c, w := newTestGinContext(t, http.MethodGet,
				"/agencies/"+root.String()+"/geofences?includeSubagency=true&sort_by=home_agency&order="+tc.order, "", nil, root)
			setServiceClaims(c, metadata.Role{api.Sentinel: api.GlobalAdmin})

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			var resp listHomeAgencyResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := make([]string, 0, len(resp.Data))
			for _, d := range resp.Data {
				got = append(got, d.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
			first := resp.Data[0].HomeAgency
			if first == nil || first.SynergyIdentifier == "" || first.Name == "" {
				t.Fatalf("home_agency = %+v, want id, synergy_identifier and name", first)
			}
		})
	}
}

// TestListGeofences_SingleAgencyHomeAgencyDetails covers a plain list
// (no includeSubagency): home_agency details come from a single-agency
// lookup, and if the agency service is down the list still succeeds, with
// home_agency reduced to its id.
func TestListGeofences_SingleAgencyHomeAgencyDetails(t *testing.T) {
	agencyID := uuid.New()
	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return []*dbmodel.Geofence{namedGeofence(agencyID, "Office")}, nil
		},
	}

	for _, tc := range []struct {
		name      string
		down      bool
		wantSynID string
	}{
		{name: "agency service up", wantSynID: "SYN-1"},
		{name: "agency service down", down: true, wantSynID: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := fakeAgencyTree(t, agencyID, nil, tc.down, agencyInfo{ID: agencyID, SynergyIdentifier: "SYN-1", Name: "Agency One"})
			ctrl := newScopeTestController(datasource, server)

			c, w := newTestGinContext(t, http.MethodGet, "/agencies/"+agencyID.String()+"/geofences?sort_by=home_agency", "", nil, agencyID)

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			var resp listHomeAgencyResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(resp.Data) != 1 || resp.Data[0].HomeAgency == nil {
				t.Fatalf("data = %+v, want one item with home_agency", resp.Data)
			}
			ha := resp.Data[0].HomeAgency
			if ha.ID != agencyID || ha.SynergyIdentifier != tc.wantSynID {
				t.Fatalf("home_agency = %+v, want id %v, synergy_identifier %q", ha, agencyID, tc.wantSynID)
			}
		})
	}
}

// TestListGeofences_SortsByRadius confirms sort_by=radius orders Circle
// geofences by their geo_json radius, with non-Circle shapes (no radius)
// last in both directions, via the query string and the body.
func TestListGeofences_SortsByRadius(t *testing.T) {
	agencyID := uuid.New()
	circle := func(name string, radius string) *dbmodel.Geofence {
		g := namedGeofence(agencyID, name)
		g.GeoJSON = `{"type":"Circle","coordinates":[-84.388,33.749],"radius":` + radius + `}`
		return g
	}
	polygon := namedGeofence(agencyID, "Polygon")
	polygon.Type = dbmodel.GeofenceTypePolygon
	polygon.GeoJSON = `{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]}`
	rows := []*dbmodel.Geofence{circle("R750", "750"), polygon, circle("R100", "100"), circle("R2000", "2000")}

	datasource := &fakeGeofenceDbAdapter{
		findByAgencyAndClient: func(context.Context, uuid.UUID, *uuid.UUID, *dbmodel.GeofenceStatus, []uuid.UUID) ([]*dbmodel.Geofence, error) {
			return slices.Clone(rows), nil
		},
	}
	ctrl := newTestController(datasource, &fakeActivityLogger{})
	base := "/agencies/" + agencyID.String() + "/geofences"

	for _, tc := range []struct {
		name, query, body string
		want              []string
	}{
		{name: "asc", query: "?sort_by=radius", want: []string{"R100", "R750", "R2000", "Polygon"}},
		{name: "desc", query: "?sort_by=radius&order=desc", want: []string{"R2000", "R750", "R100", "Polygon"}},
		{name: "body desc", body: `{"data":{"sort_by":"radius","order":"desc"}}`, want: []string{"R2000", "R750", "R100", "Polygon"}},
		{name: "numeric, not lexical", query: "?sort_by=radius&name=R", want: []string{"R100", "R750", "R2000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestGinContext(t, http.MethodGet, base+tc.query, tc.body, nil, agencyID)

			ctrl.ListGeofences(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			resp := decodeListResponse(t, w)
			got := make([]string, 0, len(resp.Data))
			for _, d := range resp.Data {
				got = append(got, d.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}
}
