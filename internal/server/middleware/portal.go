package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/user"
)

// WithLegacyUser is an entrance gate, before any legacy GraphQL parsing,
// node lookup, playground bypass, management token or raw evidence handler.
func WithLegacyUser() gin.HandlerFunc { return WithLegacyUserResponder(nil) }
func WithLegacyUserResponder(responder ErrorResponder) gin.HandlerFunc {
	return func(c *gin.Context) {
		u, ok := contexts.GetUser(c.Request.Context())
		if !ok || u == nil || u.AccessMode == user.AccessModePortal {
			respondMiddlewareError(c, responder, http.StatusForbidden, errors.New("use the user portal"))
			return
		}
		c.Next()
	}
}
