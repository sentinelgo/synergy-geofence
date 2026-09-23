package internal

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	auth "github.com/sentinelgo/synergy-common/pkg/authz/resolver"
)

const (
	ErrorKey   = "error"
	DataKey    = "data"
	SuccessKey = "success"
	Code       = "code"
	Message    = "message"
	Type       = "type"

	agencyIdKey     = "agency_id"
	clientIdKey     = "client_id"
	claimsKey       = "claims"
	resolvedRoleKey = "resolved_role"
)

// Claims returns the authenticated caller's resolved JWT/role claims set by
// the claimsValidationMiddleware, or nil if the request carries no
// verifiable identity (e.g. JWT verification is disabled in this
// environment).
func Claims(c *gin.Context) *auth.Jwt {
	if x, ok := c.Get(claimsKey); ok {
		return x.(*auth.Jwt)
	}

	return nil
}

func ClaimsKey() string {
	return claimsKey
}

// AgencyID returns the agency identity resolved by the withAgencyId middleware for the current request.
func AgencyID(c *gin.Context) uuid.UUID {
	if x, ok := c.Get(agencyIdKey); ok {
		return x.(uuid.UUID)
	}

	return uuid.Nil
}

func AgencyIDKey() string {
	return agencyIdKey
}

// ResolvedRole returns the role (one of the api.*Admin template constants,
// e.g. api.AgencyAdmin) requireAgencyAdmin resolved the caller as holding
// for this request, or "" if no admin role was resolved — e.g. JWT
// verification is disabled, or the route isn't gated by requireAgencyAdmin
// at all. Consumed by the audit package (see internal/audit.Actor.Role) so
// mutation audit events carry role attribution without a second RPC:
// requireAgencyAdmin already resolves this synchronously from JWT claims
// before the handler runs.
func ResolvedRole(c *gin.Context) string {
	if x, ok := c.Get(resolvedRoleKey); ok {
		return x.(string)
	}

	return ""
}

func ResolvedRoleKey() string {
	return resolvedRoleKey
}

// ClientID returns the optional client scoping identity resolved by the withClientId middleware, if present.
func ClientID(c *gin.Context) *uuid.UUID {
	if x, ok := c.Get(clientIdKey); ok {
		return x.(*uuid.UUID)
	}

	return nil
}

func ClientIDKey() string {
	return clientIdKey
}
