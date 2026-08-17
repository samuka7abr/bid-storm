// Package httpapi wires the HTTP surface of auctiond.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

// Deps is what the router needs from the rest of the process. The readiness
// conditions arrive as functions, so the HTTP layer never learns that there is
// a pgx pool behind them and the tests can fail one condition at a time. Engine
// arrives as the interface for the same reason and a stronger one: no handler
// may know which strategy is running.
type Deps struct {
	Ping     Probe
	Schema   Probe
	Metrics  http.Handler
	Engine   bid.BidEngine
	Auctions AuctionStore
	Log      *slog.Logger
}

// New builds the router.
//
// gin.New instead of gin.Default: the default logger writes a line per request,
// which under 1000 VUs is measurable cost inside the thing being measured.
func New(deps Deps) *gin.Engine {
	// A caller that exercises only part of the surface — the health tests build a
	// router out of nothing but the probes — must not have to invent a logger.
	if deps.Log == nil {
		deps.Log = slog.Default()
	}

	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/healthz", Healthz)
	r.GET("/readyz", Readyz(deps.Ping, deps.Schema))
	r.GET("/metrics", gin.WrapH(deps.Metrics))

	r.POST("/auctions", CreateAuction(deps.Auctions, deps.Log))
	r.GET("/auctions/:id", GetAuction(deps.Auctions, deps.Log))
	// Identity guards the bid route alone: nothing in the schema owns an
	// auction, so creating one asks for no identity.
	r.POST("/auctions/:id/bids", RequireUserID(), PlaceBid(deps.Engine, deps.Log))

	return r
}
