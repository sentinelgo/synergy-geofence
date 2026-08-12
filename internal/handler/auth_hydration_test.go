package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/auth0/go-jwt-middleware/v3/validator"
	"github.com/gin-gonic/gin"
	"github.com/sentinelgo/synergy-common/pkg/authz/api/model/metadata"
	jwtpkg "github.com/sentinelgo/synergy-common/pkg/http/jwt"
	"github.com/sentinelgo/synergy-common/pkg/http/token"
)

func TestHydrateOpaqueGeofenceClaimsUsesUserinfoMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	md := metadata.NewMetadataPublic()
	roles := metadata.Role{"agency-a": "agencyadmin"}
	md.UserProfile.Roles = &roles
	userinfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ory_at_test" {
			t.Errorf("userinfo Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":       "user-id",
			"aud":       []string{"geofence.dev.sentinelsyn.com"},
			"meta":      md.ToCompatible(),
			"token_use": "geofence",
		})
	}))
	defer userinfo.Close()

	router := gin.New()
	router.Use(hydrateOpaqueGeofenceClaims(geofenceAuthorizationConfig{
		Enabled:     true,
		Audience:    "geofence.dev.sentinelsyn.com",
		UserinfoURL: userinfo.URL,
	}, userinfo.Client()))
	router.GET("/api/v1/test", func(c *gin.Context) {
		if got := c.GetHeader("Authorization"); got != "" {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		claims, _ := token.ClaimsFromContext(c.Request.Context())
		validated, ok := claims.Raw.(*validator.ValidatedClaims)
		if !ok {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		serviceClaims, ok := validated.CustomClaims.(*jwtpkg.ServiceClaims)
		if !ok || serviceClaims.MetadataPublic == nil || !serviceClaims.MetadataPublic.HasRole("agency-a", "agencyadmin", nil) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	request.Header.Set("Authorization", "Bearer ory_at_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestHydrateOpaqueGeofenceClaimsRejectsWrongAudience(t *testing.T) {
	gin.SetMode(gin.TestMode)
	md := metadata.NewMetadataPublic()
	userinfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":       "user-id",
			"aud":       []string{"another-service"},
			"meta":      md.ToCompatible(),
			"token_use": "geofence",
		})
	}))
	defer userinfo.Close()

	router := gin.New()
	router.Use(hydrateOpaqueGeofenceClaims(geofenceAuthorizationConfig{
		Enabled:     true,
		Audience:    "geofence.dev.sentinelsyn.com",
		UserinfoURL: userinfo.URL,
	}, userinfo.Client()))
	router.GET("/api/v1/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	request.Header.Set("Authorization", "Bearer ory_at_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}
