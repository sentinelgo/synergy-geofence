package handler

import (
	"net/http"
	"time"

	jwtmiddleware "github.com/auth0/go-jwt-middleware/v3"
	"github.com/auth0/go-jwt-middleware/v3/validator"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"
	localerrors "github.com/sentinelgo/synergy-geofence/internal/errors"

	"github.com/sentinelgo/synergy-common/pkg/authz/api/model/enums/roleenum"
	auth "github.com/sentinelgo/synergy-common/pkg/authz/resolver"
	"github.com/sentinelgo/synergy-common/pkg/http/token"
	"github.com/sentinelgo/synergy-common/pkg/log"
	"github.com/spf13/viper"
)

// claimsValidationMiddleware resolves the caller's JWT/role claims (if any)
// once per request and stashes them on the gin context, mirroring
// sso-agency-service's own claimsValidationMiddleware. It never aborts the
// request itself — routes that require an authenticated/authorized caller
// enforce that via requireAgencyAdmin/requireAgencyViewer.
func claimsValidationMiddleware(authzClient auth.AuthzClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		if sc := claimsFromContext(c, authzClient); sc != nil {
			c.Set(pkg.ClaimsKey(), sc)
		}
		c.Next()
	}
}

// claimsFromContext extracts and validates the caller's identity from the
// incoming request, in priority order: claims already resolved earlier in
// the chain, claims attached by the token verification middleware, claims
// attached by the auth0 JWT middleware, a local-development subject override
// (viper "common.jwt.subject"), and finally a service-to-service fallback for
// signed-but-unauthenticated (m2m) requests. Returns nil if none apply,
// which happens whenever JWT verification is disabled for this environment.
func claimsFromContext(c *gin.Context, authClient auth.AuthzClient) *auth.Jwt {
	l := log.LoggerFromContext(c)

	if authJwt := pkg.Claims(c); authJwt != nil {
		return authJwt
	}

	if tclaims, ok := token.ClaimsFromContext(c.Request.Context()); ok && tclaims != nil {
		if claims, ok := tclaims.Raw.(*validator.ValidatedClaims); ok && claims != nil {
			l.With(pkg.ClaimsKey(), claims).Debug("incoming claims")
			if authJwt, err := populateAuthJwtFromValidatedClaims(c, authClient, claims); err != nil {
				l.With(pkg.ErrorKey, err).Error("unable to list user roles with this claim")
			} else {
				return authJwt
			}
		} else if len(tclaims.Subject) > 0 {
			claims := &validator.ValidatedClaims{
				RegisteredClaims: validator.RegisteredClaims{
					Issuer:   tclaims.Issuer,
					Subject:  tclaims.Subject,
					Audience: tclaims.Aud,
				},
			}
			l.With(pkg.ClaimsKey(), tclaims).Debug("incoming token claims")
			if authJwt, err := populateAuthJwtFromValidatedClaims(c, authClient, claims); err != nil {
				l.With(pkg.ErrorKey, err).Error("unable to list user roles with this claim")
			} else {
				return authJwt
			}
		}
	}

	if claims, err := jwtmiddleware.GetClaims[*validator.ValidatedClaims](c.Request.Context()); err == nil {
		l.With(pkg.ClaimsKey(), claims).Debug("incoming claims")
		if authJwt, perr := populateAuthJwtFromValidatedClaims(c, authClient, claims); perr != nil {
			l.With(pkg.ErrorKey, perr).Error("unable to list user roles with this claim")
		} else {
			return authJwt
		}
	}

	if viper.GetString("common.jwt.subject") != "" {
		claims := &validator.ValidatedClaims{
			RegisteredClaims: validator.RegisteredClaims{
				Issuer:   viper.GetString("common.jwt.issuer"),
				Subject:  viper.GetString("common.jwt.subject"),
				Audience: viper.GetStringSlice("common.jwt.audience"),
				Expiry:   time.Now().Add(5 * time.Hour).UnixMilli(),
				ID:       uuid.NewString(),
			},
		}
		if authJwt, err := populateAuthJwtFromValidatedClaims(c, authClient, claims); err != nil {
			l.With(pkg.ErrorKey, err).Error("unable to list user roles with this claim")
		} else {
			return authJwt
		}
	}

	if authJwt, err := auth.PopulateServiceToServiceClaims(c, nil); err != nil {
		l.With(pkg.ErrorKey, err).Error("unable to create a service to service claim")
	} else if authJwt != nil {
		l.With(pkg.ClaimsKey(), authJwt).Debug("created a service to service claim")
		return authJwt
	}

	return nil
}

func populateAuthJwtFromValidatedClaims(c *gin.Context, authClient auth.AuthzClient, claims *validator.ValidatedClaims) (*auth.Jwt, error) {
	if claims.CustomClaims != nil {
		return authClient.PopulateRolesFromClaims(c, claims)
	}

	roles, err := authClient.ListUserRoles(c, claims.RegisteredClaims.Subject)
	if err != nil {
		return nil, err
	}

	return auth.NewJwt(claims, roles[claims.RegisteredClaims.Subject]), nil
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
	// No real IDP (and no reachable authz sidecar) in an environment that
	// disables JWT verification, so there's no legitimate way to resolve a
	// caller's actual role here. Skip the gate rather than reject everyone.
	if viper.GetBool("common.jwt.verification.disabled") {
		c.Next()
		return
	}

	sc := pkg.Claims(c)
	if sc == nil {
		abortUnauthorized(c)
		return
	}

	if sc.IsGlobalAdmin() || sc.IsSystemAdmin() {
		c.Next()
		return
	}

	if isAgencyAdmin(sc, pkg.AgencyID(c)) {
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

	if sc.IsGlobalAdmin() || sc.IsSystemAdmin() || sc.IsReadOnlyAdmin() {
		c.Next()
		return
	}

	if isAgencyAdmin(sc, pkg.AgencyID(c)) {
		c.Next()
		return
	}

	abortForbidden(c, sc, pkg.AgencyID(c))
}

// isAgencyAdmin reports whether the claims hold the Agency Admin role at
// the given agency specifically (not a sentinel role, and not merely any
// role at the agency — Manager/Operator/Standard/Limited/ReadOnly are all
// "Non-Admin" for this purpose).
func isAgencyAdmin(sc *auth.Jwt, agencyID uuid.UUID) bool {
	id, ok := sc.HasAssociationWith(agencyID.String())
	if !ok || id == nil {
		return false
	}

	tid, err := roleenum.TemplateIdString(id.Template)
	return err == nil && tid == roleenum.AgencyAdmin
}

func abortUnauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrAuth, localerrors.Claims)})
}

func abortForbidden(c *gin.Context, sc *auth.Jwt, agencyID uuid.UUID) {
	log.LoggerFromContext(c).With(pkg.AgencyIDKey(), agencyID, "subject", sc.Subject()).Warn("caller lacks the required admin role at the requested agency")
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrAuth, localerrors.AgencyId)})
}
