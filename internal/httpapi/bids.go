package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

type placeBidBody struct {
	AmountCents int64 `json:"amountCents"`
	// A pointer, so that "absent" is distinguishable from zero. The optimistic
	// engine turns nil into Invalid; the other two ignore the field.
	ExpectedVersion *int64 `json:"expectedVersion"`
}

// PlaceBid is a mapping table and nothing else.
//
// It never learns which engine is behind it: BID_STRATEGY swaps the
// implementation without touching a line here. That is not elegance, it is a
// condition of the experiment — a handler that branched per strategy would put
// itself inside the thing being compared.
func PlaceBid(engine bid.BidEngine, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		auctionID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			// Answered without touching the database: a malformed id cannot name
			// an auction, so there is nothing to look up.
			c.JSON(http.StatusNotFound, ErrorResponse{Error: codeAuctionNotFound})
			return
		}

		var body placeBidBody
		// The column carries CHECK (amount_cents > 0), and letting the database
		// refuse would turn a client error into a 503.
		if err := c.ShouldBindJSON(&body); err != nil || body.AmountCents <= 0 {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: codeInvalidAmount})
			return
		}

		// The request's own context, with no deadline of our own: net/http
		// cancels it when the client hangs up, so a bidder giving up frees its
		// slot in the pool queue and db_pool_canceled_acquire_total counts it. A
		// server-side timeout would be a fifth parameter to keep identical across
		// three engines, and would cut short the queue under measurement.
		res, err := engine.PlaceBid(c.Request.Context(), bid.BidRequest{
			AuctionID:       auctionID,
			UserID:          userID(c),
			AmountCents:     body.AmountCents,
			ExpectedVersion: body.ExpectedVersion,
		})
		if err != nil {
			// The only per-request log in the process: one line per request under
			// 1000 VUs is measurable cost inside the thing being measured, but a
			// 503 is rare and never explains itself without the auction.
			log.Error("place bid failed", "auction_id", auctionID, "error", err)
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: codeUnavailable, Retryable: true})
			return
		}

		switch res.Outcome {
		case bid.Accepted:
			c.JSON(http.StatusCreated, BidAccepted{
				AuctionStateView: stateView(res.Current),
				BidID:            res.BidID,
				Seq:              res.Seq,
			})
		case bid.Conflict:
			c.JSON(http.StatusConflict, BidRejected{
				AuctionStateView: stateView(res.Current),
				Error:            codeVersionConflict,
				Retryable:        true,
			})
		case bid.TooLow:
			// Retryable, like the 409: the pessimistic engine and the shard never
			// produce a conflict, and if too_low were terminal their VUs would
			// send one request and give up while the optimistic VU sent ten. The
			// three would carry different loads and accepted/s would stop
			// comparing anything.
			c.JSON(http.StatusUnprocessableEntity, BidRejected{
				AuctionStateView: stateView(res.Current),
				Error:            codeTooLow,
				Retryable:        true,
			})
		case bid.Closed:
			c.JSON(http.StatusGone, BidRejected{
				AuctionStateView: stateView(res.Current),
				Error:            codeAuctionClosed,
			})
		case bid.NotFound:
			c.JSON(http.StatusNotFound, ErrorResponse{Error: codeAuctionNotFound})
		case bid.Invalid:
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: codeExpectedVersionRequired})
		default:
			// An engine returned an outcome this table does not know. Without
			// this arm gin would answer 200 with an empty body, which is the
			// worst possible way to find out.
			log.Error("unmapped bid outcome", "auction_id", auctionID, "outcome", res.Outcome.String())
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: codeUnavailable, Retryable: true})
		}
	}
}
