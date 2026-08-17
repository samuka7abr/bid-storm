// Package optimistic implements BidEngine with a conditional UPDATE.
//
// The conditional UPDATE is one line. What makes the metric worth anything is
// what happens when it affects nothing: zero rows can mean four incompatible
// things, and collapsing them into one conflict would make a R$ 5 bid on a
// R$ 9.000 auction count as a concurrency conflict — the central series of the
// thesis would become noise, and the client would retry forever a bid that can
// never pass. So the failing path pays one extra round-trip to classify, and the
// happy path pays nothing.
//
// Nothing here talks to Prometheus: the two bid series are observed by a
// decorator over BidEngine, so the three engines are timed at one boundary
// because only one boundary exists (decisão 23).
package optimistic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

// Engine places bids with optimistic concurrency control.
type Engine struct {
	pool *pgxpool.Pool
}

// New returns the engine reading and writing through pool.
func New(pool *pgxpool.Pool) *Engine {
	return &Engine{pool: pool}
}

// PlaceBid returns once the bid is durable, or once it is known to be rejected.
//
// The context is the request's, untouched: net/http cancels it when the client
// disconnects, so a bidder giving up on BID_DEADLINE releases its slot in the
// pool queue by itself and db_pool_canceled_acquire_total counts it. A server
// timeout here would be a fifth parameter to keep identical across three engines
// and would cut short the queue that is the phenomenon under measurement.
func (e *Engine) PlaceBid(ctx context.Context, req bid.BidRequest) (bid.BidResult, error) {
	if req.ExpectedVersion == nil {
		return bid.BidResult{Outcome: bid.Invalid}, nil
	}

	bidID := uuid.New()
	var seq, minIncrement int64
	err := e.pool.QueryRow(ctx, placeBid,
		req.AmountCents, req.UserID, req.AuctionID, *req.ExpectedVersion, bidID,
	).Scan(&seq, &minIncrement)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return e.classify(ctx, req)
	case err != nil:
		return bid.BidResult{}, fmt.Errorf("place bid: %w", err)
	}

	// Built from the statement's own return, never from a second query. Status
	// and EndsAt stay zeroed because the 201 envelope does not publish them, and
	// the rule of this envelope is that it never carries a number nobody read.
	return bid.BidResult{
		Outcome: bid.Accepted,
		Seq:     seq,
		BidID:   bidID,
		Current: bid.AuctionState{
			Version:           seq,
			HighestBidCents:   req.AmountCents,
			MinIncrementCents: minIncrement,
		},
	}, nil
}

// classify names which of the four reasons stopped the UPDATE.
//
// The order is NotFound → Closed → Conflict → TooLow, and Conflict is also the
// default. estrategias.md checked the amount before the version, which under
// high contention empties the optimistic engine's key series: the bidder re-aims
// at minNextBid, somebody else commits while the request is in flight, and the
// rejection arrives with a stale version AND an amount below the new minimum —
// so almost every rejection would be labelled too_low and conflict would read
// zero in the very cell where the thesis needs it.
//
// With this order, Conflict means "the client's snapshot went stale" and TooLow
// means "the client was up to date and still sent too little". Falling through
// to Conflict covers the race where the state changed between the two
// statements: the version matched the snapshot this SELECT just read, so the
// only thing that can have stopped the UPDATE is a commit in between.
func (e *Engine) classify(ctx context.Context, req bid.BidRequest) (bid.BidResult, error) {
	var (
		st     bid.AuctionState
		status string
		dbNow  time.Time
	)
	err := e.pool.QueryRow(ctx, classify, req.AuctionID).Scan(
		&st.Version, &st.HighestBidCents, &st.MinIncrementCents, &status, &st.EndsAt, &dbNow,
	)
	st.Status = bid.Status(status)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No auction, no state: a 404 carrying currentHighestBid: 0 would be the
		// contract claiming a nonexistent auction is worth zero cents.
		return bid.BidResult{Outcome: bid.NotFound}, nil
	case err != nil:
		return bid.BidResult{}, fmt.Errorf("classify bid: %w", err)
	case st.IsClosed(dbNow):
		return bid.BidResult{Outcome: bid.Closed, Current: st}, nil
	case st.Version != *req.ExpectedVersion:
		return bid.BidResult{Outcome: bid.Conflict, Current: st}, nil
	case req.AmountCents < st.MinNextBid():
		return bid.BidResult{Outcome: bid.TooLow, Current: st}, nil
	default:
		return bid.BidResult{Outcome: bid.Conflict, Current: st}, nil
	}
}
