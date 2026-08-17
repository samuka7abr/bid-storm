// Package bid holds the contract the three engines implement, and nothing else.
//
// It imports neither Prometheus, nor Gin, nor pgxpool. That is not tidiness: the
// suite in enginetest imports this package and every engine imports it too, so
// anything that knows all three engines would close a cycle if it lived here.
// The factory therefore sits in internal/app, and this package stays free of the
// dependencies the single-writer engine will bring in etapa 3 — whoever imports
// bid just to read an Outcome does not drag goroutines and tickers along.
package bid

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Status mirrors the auction_status enum.
//
// The constants are StatusOpen and StatusClosed rather than Open and Closed
// because Closed is already an Outcome, and the two mean different things: one
// is a column, the other is what the engine decided.
type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

// BidEngine is the whole contract. One method, and rejecting a bid is a result
// rather than an error: "you lost" is a successful answer from the engine, so
// error is reserved for "I could not decide" — Postgres down, context canceled.
// A handler that mapped typed errors would need errors.As on the hottest path of
// the system and a new case per engine.
type BidEngine interface {
	// PlaceBid returns only once the bid is DURABLE. True of all three engines.
	PlaceBid(ctx context.Context, req BidRequest) (BidResult, error)
}

// BidRequest is one attempt.
type BidRequest struct {
	AuctionID       uuid.UUID
	UserID          uuid.UUID
	AmountCents     int64
	ExpectedVersion *int64 // nil = the engine ignores it (pessimistic and shard)
	IdempotencyKey  string // etapa 2; every row written in etapa 1 leaves it NULL
}

// AuctionState is the auction as the engine saw it.
type AuctionState struct {
	Version           int64
	HighestBidCents   int64
	MinIncrementCents int64
	Status            Status
	EndsAt            time.Time
}

// MinNextBid is the server's rule, never the client's: if every client picked
// its own increment, two VUs would compete under different rules and the
// comparison between strategies would inherit that asymmetry.
func (s AuctionState) MinNextBid() int64 {
	return s.HighestBidCents + s.MinIncrementCents
}

// IsClosed is the single closing rule, shared by the engine's classification and
// by GET /auctions/:id — if the read returned the raw status column instead, the
// two routes of the same API would disagree about the same auction until the
// closerd of etapa 4 starts writing it.
//
// now must come from Postgres, not from time.Now(). The accepting UPDATE decides
// with the database clock; classifying with the container's clock would turn a
// few milliseconds of skew into 410 auction_closed, retryable false, for a bid
// the database would have taken — biased error, exactly at the last second of
// the auction, which is the entire scenario of this project.
func (s AuctionState) IsClosed(now time.Time) bool {
	return s.Status == StatusClosed || !now.Before(s.EndsAt)
}

// BidResult is what the engine decided, plus the state that decision was made
// against.
//
// Current is filled whenever the auction exists — Accepted, Conflict, TooLow and
// Closed — and stays zeroed in NotFound and Invalid, where there is no state to
// report. Publishing currentHighestBid: 0 on a 404 would be the contract
// claiming a nonexistent auction is worth zero cents.
type BidResult struct {
	Outcome Outcome
	Seq     int64        // filled when Accepted
	BidID   uuid.UUID    // filled when Accepted
	Current AuctionState // empty in NotFound and Invalid
}
