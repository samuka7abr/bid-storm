// Package httpapi wires the HTTP surface of auctiond.
package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Deps is what the router needs from the rest of the process. The readiness
// conditions arrive as functions, so the HTTP layer never learns that there is
// a pgx pool behind them and the tests can fail one condition at a time.
type Deps struct {
	Ping    Probe
	Schema  Probe
	Metrics http.Handler
}

// New builds the router. Bid routes are deliberately absent: they arrive with
// the engine, in spec 02.
//
// gin.New instead of gin.Default: the default logger writes a line per request,
// which under 1000 VUs is measurable cost inside the thing being measured.
func New(deps Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/healthz", Healthz)
	r.GET("/readyz", Readyz(deps.Ping, deps.Schema))
	r.GET("/metrics", gin.WrapH(deps.Metrics))

	return r
}
