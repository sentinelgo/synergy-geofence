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

	agencyIdKey = "agency_id"
	clientIdKey = "client_id"
	claimsKey   = "claims"
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
