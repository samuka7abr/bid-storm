// Package enginetest is the executable contract of BidEngine.
//
// It is a normal package rather than a _test.go file because every engine
// imports it: written now, against the interface, etapa 2 and etapa 3 cost two
// lines of test each — the new engine passes the suite or it is wrong. Written
// later, each engine gets a bespoke test and the project loses the guarantee
// that the three honour the same contract, which is the premise of comparing
// them.
//
// Nothing here asserts SQL. The single-writer engine of etapa 3 decides in
// memory, and it has to be validated by this same suite; the queries below only
// read the history back in order to replay it.
package enginetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

// Auction parameters shared by every case, so that a failure message never
// leaves the reader guessing what the minimum next bid was.
const (
	startingBid  = 0
	minIncrement = 100
)

// The concurrency case: enough workers to make every attempt collide, few
// enough that the whole suite stays under the 30 seconds the spec allows.
const (
	workers  = 16
	attempts = 20
)

// RunConformance drives newEngine through the behaviour all three engines owe
// their callers. newEngine receives the pool of an ephemeral Postgres.
func RunConformance(t *testing.T, newEngine func(*pgxpool.Pool) bid.BidEngine) {
	t.Helper()

	pg := testsupport.Start(t)
	engine := newEngine(pg.Pool)

	t.Run("first bid is accepted at seq 1", func(t *testing.T) {
		auction := seedAuction(t, pg.Pool, time.Minute)

		res := place(t, engine, auction, ptr(int64(0)), minIncrement)
		if res.Outcome != bid.Accepted {
			t.Fatalf("outcome = %v, want accepted", res.Outcome)
		}
		if res.Seq != 1 {
			t.Errorf("seq = %d, want 1", res.Seq)
		}
		if res.Current.Version != 1 {
			t.Errorf("current version = %d, want 1", res.Current.Version)
		}
		if res.BidID == uuid.Nil {
			t.Error("bidId is the nil UUID, want a generated one")
		}
		if res.Current.HighestBidCents != minIncrement {
			t.Errorf("current highest bid = %d, want %d", res.Current.HighestBidCents, minIncrement)
		}
	})

	t.Run("below the minimum next bid is too low", func(t *testing.T) {
		auction := seedAuction(t, pg.Pool, time.Minute)

		res := place(t, engine, auction, ptr(int64(0)), minIncrement-1)
		if res.Outcome != bid.TooLow {
			t.Fatalf("outcome = %v, want too_low", res.Outcome)
		}
		// The envelope of a rejection is what lets the bidder retry in a single
		// round-trip, so an empty Current here would cost a GET per attempt.
		if res.Current.MinNextBid() != minIncrement {
			t.Errorf("minNextBid = %d, want %d", res.Current.MinNextBid(), minIncrement)
		}
	})

	t.Run("unknown auction is not found", func(t *testing.T) {
		res := place(t, engine, uuid.New(), ptr(int64(0)), minIncrement)
		if res.Outcome != bid.NotFound {
			t.Fatalf("outcome = %v, want not_found", res.Outcome)
		}
	})

	t.Run("auction past ends_at is closed", func(t *testing.T) {
		auction := seedAuction(t, pg.Pool, -time.Minute)

		res := place(t, engine, auction, ptr(int64(0)), minIncrement)
		if res.Outcome != bid.Closed {
			t.Fatalf("outcome = %v, want closed", res.Outcome)
		}
	})

	// Persisting rejected attempts would make the optimistic engine write many
	// times more than the others under high contention, for a reason that is not
	// concurrency control — the benchmark would lie in favour of its own
	// hypothesis.
	t.Run("a rejection writes no row", func(t *testing.T) {
		auction := seedAuction(t, pg.Pool, time.Minute)

		if res := place(t, engine, auction, ptr(int64(0)), minIncrement-1); res.Outcome == bid.Accepted {
			t.Fatalf("outcome = accepted, want a rejection")
		}
		if got := countBids(t, pg.Pool, auction); got != 0 {
			t.Errorf("bids rows = %d, want 0", got)
		}
	})

	t.Run("minNextBid of a rejection lands the next attempt", func(t *testing.T) {
		auction := seedAuction(t, pg.Pool, time.Minute)
		place(t, engine, auction, ptr(int64(0)), minIncrement)

		rejected := place(t, engine, auction, ptr(int64(1)), minIncrement)
		if rejected.Outcome == bid.Accepted {
			t.Fatalf("outcome = accepted, want a rejection")
		}

		// Re-aiming with nothing but the rejection body, which is exactly what
		// the k6 bidder does.
		retried := place(t, engine, auction, &rejected.Current.Version, rejected.Current.MinNextBid())
		if retried.Outcome != bid.Accepted {
			t.Fatalf("retry outcome = %v, want accepted", retried.Outcome)
		}
	})

	t.Run("concurrent bids leave a replayable history", func(t *testing.T) {
		auction := seedAuction(t, pg.Pool, time.Minute)

		var accepted atomic.Int64
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()

				// Every worker opens on the same state, so the first round is a
				// pile-up by construction, and re-aims from the previous answer
				// from then on — the bidder of decisão 5.
				version, amount := int64(0), int64(startingBid+minIncrement)
				for range attempts {
					res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
						AuctionID:       auction,
						UserID:          uuid.New(),
						AmountCents:     amount,
						ExpectedVersion: &version,
					})
					if err != nil {
						t.Errorf("PlaceBid: %v", err)
						return
					}
					if res.Outcome == bid.Accepted {
						accepted.Add(1)
					}
					version, amount = res.Current.Version, res.Current.MinNextBid()
				}
			}()
		}
		wg.Wait()

		replay(t, pg.Pool, auction, accepted.Load())
	})
}

// replay rebuilds the auction from its bids and checks the rule at every step.
//
// Asserting only that seq is unique and increasing would prove the
// UNIQUE (auction_id, seq) constraint, not the engine. The optimistic engine
// carries the increment rule inside its WHERE clause, so Postgres stops it from
// accepting an invalid bid; the shard decides in memory with no WHERE at all,
// and a race there would slip one under-priced bid into the middle of an
// otherwise perfect sequence.
func replay(t *testing.T, pool *pgxpool.Pool, auction uuid.UUID, accepted int64) {
	t.Helper()
	ctx := context.Background()

	rows, err := pool.Query(ctx,
		`SELECT seq, amount_cents FROM bids WHERE auction_id = $1 ORDER BY seq`, auction)
	if err != nil {
		t.Fatalf("read bids: %v", err)
	}
	defer rows.Close()

	var (
		count int64
		prev  int64 = startingBid
	)
	for rows.Next() {
		var seq, amount int64
		if err := rows.Scan(&seq, &amount); err != nil {
			t.Fatalf("scan bid: %v", err)
		}
		count++
		if seq != count {
			t.Fatalf("seq = %d at position %d: the sequence has a hole", seq, count)
		}
		if amount < prev+minIncrement {
			t.Fatalf("bid %d is %d cents, below %d: the increment rule broke mid-history",
				seq, amount, prev+minIncrement)
		}
		prev = amount
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read bids: %v", err)
	}

	if count != accepted {
		t.Errorf("bids rows = %d, but the engine reported %d accepted", count, accepted)
	}
	if count == 0 {
		t.Fatal("no bid was accepted: the concurrency case proved nothing")
	}

	var version, highest int64
	if err := pool.QueryRow(ctx,
		`SELECT version, highest_bid_cents FROM auctions WHERE id = $1`, auction,
	).Scan(&version, &highest); err != nil {
		t.Fatalf("read auction: %v", err)
	}
	if version != count {
		t.Errorf("auction version = %d, want %d", version, count)
	}
	if highest != prev {
		t.Errorf("auction highest_bid_cents = %d, want %d", highest, prev)
	}
}

func place(t *testing.T, engine bid.BidEngine, auction uuid.UUID, version *int64, amount int64) bid.BidResult {
	t.Helper()
	res, err := engine.PlaceBid(context.Background(), bid.BidRequest{
		AuctionID:       auction,
		UserID:          uuid.New(),
		AmountCents:     amount,
		ExpectedVersion: version,
	})
	// Every rejection is a nil error, without exception.
	if err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	return res
}

func seedAuction(t *testing.T, pool *pgxpool.Pool, endsIn time.Duration) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		// ends_at is anchored to the database clock, which is the one that
		// decides whether the auction is closed.
		`INSERT INTO auctions (id, title, highest_bid_cents, min_increment_cents, ends_at)
		 VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`,
		id, t.Name(), startingBid, minIncrement, endsIn.Seconds(),
	); err != nil {
		t.Fatalf("seed auction: %v", err)
	}
	return id
}

func countBids(t *testing.T, pool *pgxpool.Pool, auction uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bids WHERE auction_id = $1`, auction).Scan(&n); err != nil {
		t.Fatalf("count bids: %v", err)
	}
	return n
}

func ptr(v int64) *int64 { return &v }
