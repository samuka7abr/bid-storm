package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Probe reports whether one readiness condition currently holds.
type Probe func(context.Context) error

// Condition is a probe with the name /readyz publishes when it fails.
//
// Named and variadic rather than one parameter per dependency: the third
// condition arrives in this spec and a fourth arrives with the Streams of etapa
// 4, and a positional signature that grows every time is a signature every call
// site can get wrong in silence.
type Condition struct {
	Name  string
	Probe Probe
}

// Healthz is liveness. It answers 200 without touching the database or Redis, so
// that a schema divergence — or a Redis that went away — is never mistaken for a
// dead process and restarted: the restart would not fix it, and the real cause
// would disappear into a crashloop.
func Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Readyz is readiness: 200 only when every condition holds. The body names the
// first one that failed, because "cannot reach Postgres", "Postgres has the
// wrong schema" and "Redis is down" call for opposite reactions.
func Readyz(conditions ...Condition) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, condition := range conditions {
			if err := condition.Probe(c.Request.Context()); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"status": "unready",
					"check":  condition.Name,
					"error":  err.Error(),
				})
				return
			}
		}

		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}
