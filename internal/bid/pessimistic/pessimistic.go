// Package pessimistic implements BidEngine with SELECT ... FOR UPDATE inside an
// explicit transaction.
//
// Four round-trips on the accept, three on the rejection, the connection held
// from Begin to Commit. There is a way to write this in a single statement, with
// FOR UPDATE inside a CTE, and it works — but what it implements is not
// pessimism: the lock is born and dies inside the statement, the decision moves
// back into the SQL, and what is left is the optimistic engine with another
// serialisation mechanism. The project does not ask which SQL is faster; it asks
// what happens when a client holds a lock while it decides, and that is the
// shape etapa 5 measures (decisão 25).
//
// ExpectedVersion is not read anywhere — not read and then forgiven. The lock
// already guarantees the state read is the current one, so the client's version
// has no role in the decision. This engine therefore never produces Conflict nor
// Invalid: the loser gets TooLow, which is the same event seen through another
// mechanism (decisão 9), and the k6 bidder treats 422 exactly as it treats 409.
//
// No deadlock retry loop, because a deadlock cannot happen here: each
// transaction locks one auctions row and never a second one, and the INSERT into
// bids takes FOR KEY SHARE on that same row, in the same transaction. Without
// two rows there is no acquisition order to invert, and without inversion there
// is no cycle (decisão 29). READ COMMITTED for a related reason: SERIALIZABLE
// would turn every dispute into 40001 plus a retry, which is not pessimism with
// more safety — it is the optimistic strategy with the retry moved inside the
// database, a fourth cell answering a question this project did not ask
// (decisão 30).
package pessimistic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

// Engine places bids with pessimistic concurrency control.
type Engine struct {
	pool *pgxpool.Pool
	// A one-method interface rather than internal/metrics: every series name
	// this process publishes lives in that package, and the engine never learns
	// which one it feeds (decisão 28).
	lockWait prometheus.Observer
}

// New returns the engine reading and writing through pool, reporting how long
// its locked read took to lockWait.
func New(pool *pgxpool.Pool, lockWait prometheus.Observer) *Engine {
	return &Engine{pool: pool, lockWait: lockWait}
}

// PlaceBid returns once the bid is durable, or once it is known to be rejected.
//
// The context is the request's, untouched, and there is no lock_timeout: the
// queue in front of the lock is the phenomenon this engine exists to expose, so
// it is measured rather than cut short. A bidder giving up on BID_DEADLINE
// cancels the context by itself, and db_pool_canceled_acquire_total counts it.
func (e *Engine) PlaceBid(ctx context.Context, req bid.BidRequest) (bid.BidResult, error) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return bid.BidResult{}, fmt.Errorf("begin bid: %w", err)
	}
	// Rolls back every rejection, and costs nothing after a successful Commit:
	// pgx marks the transaction closed and returns ErrTxClosed without touching
	// the network. Whoever counts statements in the Postgres log will find four.
	defer tx.Rollback(ctx)

	st, dbNow, err := e.lock(ctx, tx, req.AuctionID)

	// NotFound → Closed → TooLow, decided in Go over the row already locked. The
	// precedence of decisão 20 does not arise: without Conflict there are no two
	// outcomes disputing the same rejection. And none of these costs a
	// round-trip — the optimistic engine pays an extra SELECT to classify
	// because it finds out after trying, while this one looks before trying.
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No auction, no state: a 404 carrying currentHighestBid: 0 would be the
		// contract claiming a nonexistent auction is worth zero cents.
		return bid.BidResult{Outcome: bid.NotFound}, nil
	case err != nil:
		return bid.BidResult{}, err
	case st.IsClosed(dbNow):
		return bid.BidResult{Outcome: bid.Closed, Current: st}, nil
	case req.AmountCents < st.MinNextBid():
		return bid.BidResult{Outcome: bid.TooLow, Current: st}, nil
	}

	bidID := uuid.New()
	var version int64
	if err := tx.QueryRow(ctx, placeBid,
		req.AmountCents, req.UserID, req.AuctionID, bidID, req.IdempotencyKey,
	).Scan(&version); err != nil {
		return bid.BidResult{}, fmt.Errorf("place bid: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return bid.BidResult{}, fmt.Errorf("commit bid: %w", err)
	}

	// Built from the locked read plus the write's own return, never from a third
	// query: no field of this envelope carries a number the engine did not read.
	// Status and EndsAt stay zeroed because the 201 does not publish them.
	return bid.BidResult{
		Outcome: bid.Accepted,
		Seq:     version,
		BidID:   bidID,
		Current: bid.AuctionState{
			Version:           version,
			HighestBidCents:   req.AmountCents,
			MinIncrementCents: st.MinIncrementCents,
		},
	}, nil
}

// lock takes the row lock and reports the state the decision is made against.
//
// The observation covers exactly the locked read: it starts before the statement
// goes out and stops after the Scan. Begin and Commit stay outside, because
// waiting for a lock is not what happens in either of them.
func (e *Engine) lock(ctx context.Context, tx pgx.Tx, auction uuid.UUID) (bid.AuctionState, time.Time, error) {
	var (
		st     bid.AuctionState
		status string
		dbNow  time.Time
	)

	started := time.Now()
	err := tx.QueryRow(ctx, lockAuction, auction).Scan(
		&st.Version, &st.HighestBidCents, &st.MinIncrementCents, &status, &st.EndsAt, &dbNow,
	)
	elapsed := time.Since(started)

	// A downed database is not lock wait, and its latency would lift the floor of
	// the histogram that calibrates the round-trip cost against
	// bid_confirm_duration_seconds. ErrNoRows is not that: the statement ran to
	// completion and found no row, which is a genuine sample of a read that
	// waited for nobody.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return bid.AuctionState{}, time.Time{}, fmt.Errorf("lock auction: %w", err)
	}
	e.lockWait.Observe(elapsed.Seconds())
	if err != nil {
		return bid.AuctionState{}, time.Time{}, err
	}

	st.Status = bid.Status(status)
	return st, dbNow, nil
}
