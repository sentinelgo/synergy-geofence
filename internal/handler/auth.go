package handler

import (
	"net/http"

	"github.com/auth0/go-jwt-middleware/v3/validator"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"
	localerrors "github.com/sentinelgo/synergy-geofence/internal/errors"

	"github.com/sentinelgo/synergy-common/pkg/authz/api"
	auth "github.com/sentinelgo/synergy-common/pkg/authz/resolver"
	jwtpkg "github.com/sentinelgo/synergy-common/pkg/http/jwt"
	"github.com/sentinelgo/synergy-common/pkg/http/token"
	"github.com/sentinelgo/synergy-common/pkg/log"
	"github.com/spf13/viper"
)

// claimsValidationMiddleware resolves the caller's JWT claims (if any) once
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
// by the token verification middleware. This service has no authz sidecar to
// consult, so requireAgencyAdmin/requireAgencyViewer/isAgencyAdmin resolve
// role/agency data entirely from the JWT's own custom ("meta") claims below —
// nothing here makes a network call. Returns nil if no claims are present,
// which happens whenever JWT verification is disabled for this environment.
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

// serviceClaims safely extracts the JWT's ServiceClaims — the "meta"/"scope"
// custom claims carrying the caller's sentinel and per-agency role
// assignments, embedded directly in the token at issuance — or nil if the
// caller's claims don't carry any (e.g. a token with no custom claims at
// all).
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
