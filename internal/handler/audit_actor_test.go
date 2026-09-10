package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/auth0/go-jwt-middleware/v3/validator"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"

	auth "github.com/sentinelgo/synergy-common/pkg/authz/resolver"
)

// testContextWithSubject builds a *gin.Context carrying claims for the
// given subject, as claimsValidationMiddleware would have set them. An
// empty subject mimics a request that never resolved any claims at all
// (e.g. common.jwt.verification.disabled).
func testContextWithSubject(subject string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if subject != "" {
		c.Set(pkg.ClaimsKey(), auth.NewJwt(&validator.ValidatedClaims{
			RegisteredClaims: validator.RegisteredClaims{Subject: subject},
		}, nil))
	}
	return c
}

func TestAuthenticatedUserID(t *testing.T) {
	subjectID := uuid.New()

	t.Run("valid UUID subject", func(t *testing.T) {
		c := testContextWithSubject(subjectID.String())
		got, ok := authenticatedUserID(c)
		if !ok || got != subjectID {
			t.Errorf("authenticatedUserID() = (%v, %v), want (%v, true)", got, ok, subjectID)
		}
	})

	t.Run("no claims at all", func(t *testing.T) {
		c := testContextWithSubject("")
		if _, ok := authenticatedUserID(c); ok {
			t.Error("authenticatedUserID() ok = true, want false with no claims present")
		}
	})

	t.Run("subject is not a UUID", func(t *testing.T) {
		c := testContextWithSubject("not-a-uuid")
		if _, ok := authenticatedUserID(c); ok {
			t.Error("authenticatedUserID() ok = true, want false for a non-UUID subject")
		}
	})
}

// TestAuditActorID_PrefersAuthenticatedSubject guards this PR's fix: the
// audit event's Actor must be attributed to the authenticated caller, not
// whatever client-supplied user_id the request body/query happened to
// carry (entity.CreatedBy on create, req.UserID on update, the user_id
// query param on delete — see DeleteGeofence's doc comment) — except in
// the dev-only bypass where no authenticated subject exists, where it
// falls back to that client-supplied value like the business record does.
func TestAuditActorID_PrefersAuthenticatedSubject(t *testing.T) {
	subjectID := uuid.New()
	clientSupplied := uuid.New()

	t.Run("authenticated subject wins over client-supplied", func(t *testing.T) {
		c := testContextWithSubject(subjectID.String())
		if got := auditActorID(c, clientSupplied); got != subjectID {
			t.Errorf("auditActorID() = %v, want authenticated subject %v", got, subjectID)
		}
	})

	t.Run("falls back to client-supplied with no authenticated subject", func(t *testing.T) {
		c := testContextWithSubject("")
		if got := auditActorID(c, clientSupplied); got != clientSupplied {
			t.Errorf("auditActorID() = %v, want client-supplied fallback %v", got, clientSupplied)
		}
	})
}
