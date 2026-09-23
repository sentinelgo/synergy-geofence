package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/auth0/go-jwt-middleware/v3/validator"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"

	"github.com/sentinelgo/synergy-common/pkg/authz/api"
	"github.com/sentinelgo/synergy-common/pkg/authz/api/model/metadata"
	auth "github.com/sentinelgo/synergy-common/pkg/authz/resolver"
	jwtpkg "github.com/sentinelgo/synergy-common/pkg/http/jwt"
)

// requireAgencyAdminTestRouter wires up just enough context (agency id,
// resolved claims) for requireAgencyAdmin to run as it would after
// withAgencyId and claimsValidationMiddleware, then reports whatever
// pkg.ResolvedRole ends up set to.
func requireAgencyAdminTestRouter(agencyID uuid.UUID, roles metadata.Role) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(pkg.AgencyIDKey(), agencyID)
		md := metadata.NewMetadataPublic()
		md.UserProfile.Roles = &roles
		svc := jwtpkg.NewServiceClaimsWithMetadata(md)
		c.Set(pkg.ClaimsKey(), auth.NewJwt(&validator.ValidatedClaims{CustomClaims: svc}, nil))
		c.Next()
	})
	router.Use(requireAgencyAdmin)
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, pkg.ResolvedRole(c))
	})
	return router
}

// TestRequireAgencyAdmin_ResolvesRole guards this PR's fix: requireAgencyAdmin
// must stamp pkg.ResolvedRoleKey with whichever admin role actually let the
// caller through, so audit.Actor.Role (see internal/audit/events.go) can
// attribute the mutation without a second RPC.
func TestRequireAgencyAdmin_ResolvesRole(t *testing.T) {
	agencyID := uuid.New()
	tests := []struct {
		name      string
		roles     metadata.Role
		wantRole  string
		wantAllow bool
	}{
		{name: "global admin", roles: metadata.Role{api.Sentinel: "globaladmin"}, wantRole: api.GlobalAdmin, wantAllow: true},
		{name: "system admin", roles: metadata.Role{api.Sentinel: "systemadmin"}, wantRole: api.SystemAdmin, wantAllow: true},
		{name: "agency admin at target agency", roles: metadata.Role{agencyID.String(): "agencyadmin"}, wantRole: api.AgencyAdmin, wantAllow: true},
		{name: "agency admin at a different agency doesn't count", roles: metadata.Role{uuid.New().String(): "agencyadmin"}, wantRole: "", wantAllow: false},
		{name: "no role at all", roles: metadata.Role{}, wantRole: "", wantAllow: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := requireAgencyAdminTestRouter(agencyID, tt.roles)
			request := httptest.NewRequest(http.MethodGet, "/test", nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if tt.wantAllow {
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
				}
				if got := response.Body.String(); got != tt.wantRole {
					t.Errorf("resolved role = %q, want %q", got, tt.wantRole)
				}
				return
			}
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", response.Code)
			}
		})
	}
}
