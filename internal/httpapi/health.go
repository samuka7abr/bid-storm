package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Probe reports whether one readiness condition currently holds.
type Probe func(context.Context) error

// Healthz is liveness. It answers 200 without touching the database, so that a
// schema divergence is never mistaken for a dead process and restarted — the
// restart would not fix it, and the real cause would disappear into a
// crashloop.
func Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Readyz is readiness: 200 only when the pool answers and the schema is the
// expected one. The body names which of the two failed, because "cannot reach
// Postgres" and "Postgres has the wrong schema" call for opposite reactions.
func Readyz(ping, schema Probe) gin.HandlerFunc {
	return func(c *gin.Context) {
		checks := []struct {
			name string
			run  Probe
		}{
			{"database", ping},
			{"schema", schema},
		}

		for _, check := range checks {
			if err := check.run(c.Request.Context()); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"status": "unready",
					"check":  check.name,
					"error":  err.Error(),
				})
				return
			}
		}

		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}
