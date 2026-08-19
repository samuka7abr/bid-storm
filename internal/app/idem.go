package app

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/idem"
	"github.com/samuka7abr/bid-storm/internal/metrics"
)

// NewIdempotency builds the middleware for the strategy this process runs.
//
// It is assembled here, where the strategy name already lives, because that is
// the one label the middleware carries and the one thing it must not be able to
// discover for itself: above the switch, identical for the three engines, it
// cannot know which engine is behind it (decisão 37).
func NewIdempotency(client *redis.Client, reg prometheus.Registerer, strategy string, log *slog.Logger) gin.HandlerFunc {
	return idem.Middleware(idem.NewStore(client), metrics.NewIdempotency(reg, strategy), log)
}

// carrier hands the engine the key the middleware validated.
//
// The bid handler builds BidRequest and must not gain a line for idempotency —
// if it needed one, the middleware would be in the wrong place (RF07). So the
// key travels in the request context, and this is the single point that moves it
// into the contract, for all three engines at once. Without a key it writes "",
// which is what NULLIF turns into the NULL that keeps the row out of the partial
// unique index (decisão 15).
//
// It wraps the decorator rather than the engine, so bid_confirm_duration_seconds
// keeps timing exactly what it timed in etapa 1: the engine, and nothing above
// it.
type carrier struct {
	next bid.BidEngine
}

func (c carrier) PlaceBid(ctx context.Context, req bid.BidRequest) (bid.BidResult, error) {
	req.IdempotencyKey = idem.KeyFrom(ctx)
	return c.next.PlaceBid(ctx, req)
}
