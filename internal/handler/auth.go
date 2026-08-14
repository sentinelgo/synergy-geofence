package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/auth0/go-jwt-middleware/v3/validator"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"
	localerrors "github.com/sentinelgo/synergy-geofence/internal/errors"

	"github.com/sentinelgo/synergy-common/pkg/authz/api"
	"github.com/sentinelgo/synergy-common/pkg/authz/api/model/metadata"
	auth "github.com/sentinelgo/synergy-common/pkg/authz/resolver"
	jwtpkg "github.com/sentinelgo/synergy-common/pkg/http/jwt"
	"github.com/sentinelgo/synergy-common/pkg/http/token"
	"github.com/sentinelgo/synergy-common/pkg/log"
	"github.com/spf13/viper"
)

type geofenceAuthorizationConfig struct {
	Enabled     bool   `mapstructure:"enabled"`
	Audience    string `mapstructure:"audience"`
	UserinfoURL string `mapstructure:"userinfo-url"`
}

type geofenceUserinfo struct {
	Subject         string                   `json:"sub"`
	HydratedSubject string                   `json:"subject"`
	Audience        []string                 `json:"aud"`
	Meta            *metadata.MetadataPublic `json:"meta"`
	TokenUse        string                   `json:"token_use"`
}

func geofenceAuthorizationConfigFromViper() (geofenceAuthorizationConfig, error) {
	var cfg geofenceAuthorizationConfig
	if err := viper.UnmarshalKey("common.geofence", &cfg); err != nil {
		return cfg, err
	}
	if cfg.Enabled && (cfg.Audience == "" || cfg.UserinfoURL == "") {
		return cfg, fmt.Errorf("enabled geofence authorization requires audience and userinfo-url")
	}
	return cfg, nil
}

// hydrateOpaqueGeofenceClaims validates opaque portal tokens through Hydra's
// public /userinfo endpoint. It runs before the shared JWT middleware because
// /userinfo performs opaque-token introspection internally.
func hydrateOpaqueGeofenceClaims(cfg geofenceAuthorizationConfig, client *http.Client) gin.HandlerFunc {
	if client == nil {
		client = http.DefaultClient
	}
	return func(c *gin.Context) {
		if !cfg.Enabled || !strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Next()
			return
		}

		authorization := c.GetHeader("Authorization")
		parts := strings.Fields(authorization)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.Next()
			return
		}
		if strings.Count(parts[1], ".") == 2 {
			// JWT requests retain the existing validation path. The portal path is
			// the opaque-token path and is hydrated below.
			c.Next()
			return
		}
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, cfg.UserinfoURL, nil)
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		req.Header.Set("Authorization", authorization)
		response, err := client.Do(req)
		if err != nil {
			log.LoggerFromContext(c).With("error", err).Error("could not hydrate geofence claims from userinfo")
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		var userinfo geofenceUserinfo
		if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&userinfo); err != nil {
			log.LoggerFromContext(c).With("error", err).Warn("userinfo returned invalid geofence claims")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		subject := userinfo.Subject
		if subject == "" {
			// Hydra 2.2 does not copy the JWT-bearer session subject into the
			// ID-token claims serialized by /userinfo. Its token hook cannot set
			// reserved claims, so sidecar supplies this bounded compatibility claim.
			subject = userinfo.HydratedSubject
		}
		if subject == "" || userinfo.TokenUse != "geofence" ||
			!geofenceAudience(userinfo.Audience, cfg.Audience) ||
			userinfo.Meta == nil || userinfo.Meta.UserProfile == nil {
			log.LoggerFromContext(c).Warn("userinfo returned invalid geofence claims")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		serviceClaims := jwtpkg.NewServiceClaimsWithMetadata(userinfo.Meta)
		validated := &validator.ValidatedClaims{
			CustomClaims: serviceClaims,
			RegisteredClaims: validator.RegisteredClaims{
				Subject:  subject,
				Audience: append([]string(nil), userinfo.Audience...),
			},
		}
		claims := &token.Claims{
			Subject: subject,
			Aud:     append([]string(nil), userinfo.Audience...),
			Extras:  map[string]any{"meta": userinfo.Meta.ToCompatible(), "token_use": userinfo.TokenUse},
			Raw:     validated,
		}
		c.Request = c.Request.WithContext(token.WithClaims(c.Request.Context(), claims))
		c.Request.Header.Del("Authorization")
		c.Next()
	}
}

func geofenceAudience(actual []string, resourceAudience string) bool {
	return len(actual) == 1 && actual[0] == resourceAudience
}

// claimsValidationMiddleware resolves the caller's validated claims (if any) once
// per request and stashes them on the gin context. It never aborts the
// request itself — routes that require an authenticated/authorized caller
// enforce that via requireAgencyAdmin/requireAgencyViewer.
func claimsValidationMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if sc := claimsFromContext(c); sc != nil {
			c.Set(pkg.ClaimsKey(), sc)
		}
		c.Next()
	}
}

// claimsFromContext resolves the caller's identity from the claims attached
// by the token verification middleware. JWT claims arrive directly from the
// validator; opaque portal-token claims have already been augmented from
// Hydra /userinfo by hydrateOpaqueGeofenceClaims. This function itself makes
// no network call. It returns nil if no claims are present.
func claimsFromContext(c *gin.Context) *auth.Jwt {
	l := log.LoggerFromContext(c)

	tclaims, ok := token.ClaimsFromContext(c.Request.Context())
	if !ok || tclaims == nil {
		return nil
	}

	claims, ok := tclaims.Raw.(*validator.ValidatedClaims)
	if !ok || claims == nil {
		claims = &validator.ValidatedClaims{
			RegisteredClaims: validator.RegisteredClaims{
				Issuer:   tclaims.Issuer,
				Subject:  tclaims.Subject,
				Audience: tclaims.Aud,
			},
		}
	}

	l.With(pkg.ClaimsKey(), claims).Debug("incoming claims")
	return auth.NewJwt(claims, nil)
}

// serviceClaims safely extracts ServiceClaims — the stable "meta"/"scope"
// contract carrying the caller's sentinel and per-agency role assignments.
// These are decoded from a JWT or hydrated from /userinfo for an opaque token.
func serviceClaims(sc *auth.Jwt) *jwtpkg.ServiceClaims {
	if sc == nil {
		return nil
	}
	svc, ok := sc.CustomClaims.(*jwtpkg.ServiceClaims)
	if !ok {
		return nil
	}
	return svc
}

// Known Locations Management is admin-only: "Menu item shall be visible
// only to users with Agency Admin or System Administrator or Global
// Administrator role[s]. Non-Admin users shall NOT have access to this
// page. Read-Only Administrators shall be able to view but not edit known
// locations." Agency Admin is an agency-scoped role; System/Global/
// Read-Only Administrator are platform-wide (sentinel) roles.

// requireAgencyAdmin gates mutating routes (create/update/delete): the
// caller must be a sentinel System/Global Administrator, or hold the
// Agency Admin role at the target agency. It must run after withAgencyId
// and claimsValidationMiddleware.
func requireAgencyAdmin(c *gin.Context) {
	// No real IDP (and no authz sidecar exists for this service) in an
	// environment that disables JWT verification, so there's no legitimate
	// way to resolve a caller's actual role here. Skip the gate rather than
	// reject everyone.
	if viper.GetBool("common.jwt.verification.disabled") {
		c.Next()
		return
	}

	sc := pkg.Claims(c)
	if sc == nil {
		abortUnauthorized(c)
		return
	}

	svc := serviceClaims(sc)
	if svc == nil {
		abortForbidden(c, sc, pkg.AgencyID(c))
		return
	}

	if svc.IsGlobalAdmin() || svc.IsSystemAdmin() {
		c.Next()
		return
	}

	if isAgencyAdmin(svc, pkg.AgencyID(c)) {
		c.Next()
		return
	}

	abortForbidden(c, sc, pkg.AgencyID(c))
}

// requireAgencyViewer gates read-only routes (list/get/lookup): everyone
// requireAgencyAdmin allows, plus sentinel Read-Only Administrators, may
// view. It must run after withAgencyId and claimsValidationMiddleware.
func requireAgencyViewer(c *gin.Context) {
	// See requireAgencyAdmin: same dev-only bypass, same reasoning.
	if viper.GetBool("common.jwt.verification.disabled") {
		c.Next()
		return
	}

	sc := pkg.Claims(c)
	if sc == nil {
		abortUnauthorized(c)
		return
	}

	svc := serviceClaims(sc)
	if svc == nil {
		abortForbidden(c, sc, pkg.AgencyID(c))
		return
	}

	if svc.IsGlobalAdmin() || svc.IsSystemAdmin() || svc.IsReadOnlyAdmin() {
		c.Next()
		return
	}

	if isAgencyAdmin(svc, pkg.AgencyID(c)) {
		c.Next()
		return
	}

	abortForbidden(c, sc, pkg.AgencyID(c))
}

// isAgencyAdmin reports whether the claims hold the Agency Admin role at
// the given agency specifically (not a sentinel role, and not merely any
// role at the agency — Manager/Operator/Standard/Limited/ReadOnly are all
// "Non-Admin" for this purpose), as embedded directly in the JWT's own
// claims.
func isAgencyAdmin(svc *jwtpkg.ServiceClaims, agencyID uuid.UUID) bool {
	return svc.MetadataPublic.HasRole(agencyID.String(), api.AgencyAdmin, nil)
}

func abortUnauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrAuth, localerrors.Claims)})
}

func abortForbidden(c *gin.Context, sc *auth.Jwt, agencyID uuid.UUID) {
	log.LoggerFromContext(c).With(pkg.AgencyIDKey(), agencyID, "subject", sc.Subject()).Warn("caller lacks the required admin role at the requested agency")
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrAuth, localerrors.AgencyId)})
}
