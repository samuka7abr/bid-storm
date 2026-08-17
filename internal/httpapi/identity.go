package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const userIDKey = "bid-storm/user-id"

// RequireUserID resolves X-User-Id, and is mounted on the bid route only.
//
// No JWT: authentication is a constant, identical cost across the three engines,
// so it moves no curve on the graph and the scope rule keeps it out. When it
// arrives it arrives here, and nothing below it changes.
//
// It guards the bid route and not POST /auctions because the schema has no owner
// of an auction — asking for an identity that nothing would be checked against
// would be ceremony.
func RequireUserID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.GetHeader("X-User-Id"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, ErrorResponse{Error: codeInvalidUserID})
			return
		}
		c.Set(userIDKey, id)
	}
}

func userID(c *gin.Context) uuid.UUID {
	id, _ := c.Get(userIDKey)
	parsed, _ := id.(uuid.UUID)
	return parsed
}
