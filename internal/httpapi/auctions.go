package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/store"
)

// AuctionStore is the slice of internal/store this layer needs. Declared here so
// the handlers can be exercised against a fake: what they own is validation and
// the derivation of status, neither of which needs a container.
type AuctionStore interface {
	Create(ctx context.Context, req store.NewAuction) (store.Auction, error)
	Get(ctx context.Context, id uuid.UUID) (store.Auction, error)
}

type createAuctionBody struct {
	Title             string     `json:"title"`
	StartingBidCents  int64      `json:"startingBidCents"`
	MinIncrementCents int64      `json:"minIncrementCents"`
	EndsAt            *time.Time `json:"endsAt"`
}

// CreateAuction serves POST /auctions.
//
// Neither this route nor GET is used by k6, in the hot path or in setup:
// cmd/seed creates the auctions straight in the database, because with 1000
// auctions in a low-contention cell a thousand preparation calls would be pure
// noise on top of the measurement. They exist for interactive use and for the
// panel of etapa 5b.
func CreateAuction(auctions AuctionStore, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body createAuctionBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: codeInvalidBody})
			return
		}

		// Input validation, not the closing rule: the process clock is good
		// enough to refuse an auction that ends in the past, while the clock that
		// decides whether a running auction is closed is always Postgres's.
		switch {
		case body.Title == "":
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: codeInvalidTitle})
			return
		case body.EndsAt == nil || !body.EndsAt.After(time.Now()):
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: codeInvalidEndsAt})
			return
		case body.MinIncrementCents <= 0:
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: codeInvalidMinIncrement})
			return
		case body.StartingBidCents < 0:
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: codeInvalidStartingBid})
			return
		}

		auction, err := auctions.Create(c.Request.Context(), store.NewAuction{
			Title:             body.Title,
			StartingBidCents:  body.StartingBidCents,
			MinIncrementCents: body.MinIncrementCents,
			EndsAt:            *body.EndsAt,
		})
		if err != nil {
			log.Error("create auction failed", "error", err)
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: codeUnavailable, Retryable: true})
			return
		}

		c.JSON(http.StatusCreated, auctionView(auction))
	}
}

// GetAuction serves GET /auctions/:id, with status derived rather than read.
func GetAuction(auctions AuctionStore, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, ErrorResponse{Error: codeAuctionNotFound})
			return
		}

		auction, err := auctions.Get(c.Request.Context(), id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			c.JSON(http.StatusNotFound, ErrorResponse{Error: codeAuctionNotFound})
			return
		case err != nil:
			log.Error("get auction failed", "auction_id", id, "error", err)
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: codeUnavailable, Retryable: true})
			return
		}

		c.JSON(http.StatusOK, auctionView(auction))
	}
}
