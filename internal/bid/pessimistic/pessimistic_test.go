package pessimistic_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/bid/enginetest"
	"github.com/samuka7abr/bid-storm/internal/bid/pessimistic"
	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

// The whole cost of the second engine, in test terms. The suite was written
// against BidEngine and never against SQL precisely so this would be two lines
// (decisão 11); if it had needed one edit to accommodate this engine, it was
// never a contract, it was the optimistic engine in disguise.
func TestConformance(t *testing.T) {
	enginetest.RunConformance(t, func(pool *pgxpool.Pool) bid.BidEngine {
		return pessimistic.New(pool, &countingObserver{})
	})
}

// The scenario that produces Conflict on the optimistic engine, and the proof
// that ExpectedVersion takes no part in the decision here: version 0 went stale
// with the first bid, and 900 clears the minimum of 200 comfortably.
func TestStaleVersionWithEnoughIsAccepted(t *testing.T) {
	pg := testsupport.Start(t)
	engine := pessimistic.New(pg.Pool, &countingObserver{})
	auction := seedAuction(t, pg.Pool)

	first, err := engine.PlaceBid(context.Background(), request(auction, 100, ptr(int64(0))))
	if err != nil || first.Outcome != bid.Accepted {
		t.Fatalf("first bid: outcome %v, err %v", first.Outcome, err)
	}

	res, err := engine.PlaceBid(context.Background(), request(auction, 900, ptr(int64(0))))
	if err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	if res.Outcome != bid.Accepted {
		t.Fatalf("outcome = %v, want accepted", res.Outcome)
	}
	if res.Current.Version != 2 || res.Current.MinNextBid() != 1000 {
		t.Errorf("current = %+v, want version 2 and minNextBid 1000", res.Current)
	}
}

// Invalid on the optimistic engine, accepted here: the field does not exist as
// far as this engine is concerned, and no handler is allowed to know that.
func TestMissingExpectedVersionIsAccepted(t *testing.T) {
	pg := testsupport.Start(t)
	engine := pessimistic.New(pg.Pool, &countingObserver{})
	auction := seedAuction(t, pg.Pool)

	res, err := engine.PlaceBid(context.Background(), request(auction, 100, nil))
	if err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	if res.Outcome != bid.Accepted {
		t.Fatalf("outcome = %v, want accepted", res.Outcome)
	}
	if res.Seq != 1 {
		t.Errorf("seq = %d, want 1", res.Seq)
	}
}

// The assertion is over the distribution of outcomes, not over the database: the
// replay of the history is already the conformance suite's job. What only this
// engine can prove is the absence of two labels, which is also what the I6
// invariant of cmd/checker turns into a free detector of an engine that went
// back to reading ExpectedVersion.
func TestConcurrencyProducesNeitherConflictNorInvalid(t *testing.T) {
	const (
		workers  = 8
		attempts = 10
	)

	pg := testsupport.Start(t)
	lockWait := &countingObserver{}
	engine := pessimistic.New(pg.Pool, lockWait)
	auction := seedAuction(t, pg.Pool)

	outcomes := make(chan bid.Outcome, workers*attempts)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Everybody opens on the same state, so the first round collides by
			// construction, and re-aims from the previous answer from then on —
			// the bidder of decisão 5, minus the version it has no use for.
			amount := int64(minIncrement)
			for range attempts {
				res, err := engine.PlaceBid(context.Background(), request(auction, amount, nil))
				if err != nil {
					t.Errorf("PlaceBid: %v", err)
					return
				}
				outcomes <- res.Outcome
				amount = res.Current.MinNextBid()
			}
		}()
	}
	wg.Wait()
	close(outcomes)

	counts := map[bid.Outcome]int{}
	for outcome := range outcomes {
		counts[outcome]++
	}
	if counts[bid.Conflict] != 0 || counts[bid.Invalid] != 0 {
		t.Errorf("conflict = %d, invalid = %d, want zero of each: this engine reads no version",
			counts[bid.Conflict], counts[bid.Invalid])
	}
	if counts[bid.Accepted] == 0 {
		t.Fatal("nothing was accepted: the case proved nothing")
	}
	if counts[bid.TooLow] == 0 {
		t.Error("nothing was too low: the workers never collided, so the case proved nothing")
	}
	// One observation per locked read, and never more: the histogram covers the
	// SELECT ... FOR UPDATE alone, not Begin and not Commit.
	if got, want := lockWait.count(), workers*attempts; got != want {
		t.Errorf("lock wait observations = %d, want %d", got, want)
	}
}

// countingObserver stands in for lock_wait_duration_seconds. The conformance
// factory hands over the pool and nothing else, and none of these cases is about
// the series itself — that one is tested in internal/metrics.
type countingObserver struct {
	mu sync.Mutex
	n  int
}

func (o *countingObserver) Observe(float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n++
}

func (o *countingObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n
}

const minIncrement = 100

func request(auction uuid.UUID, amount int64, version *int64) bid.BidRequest {
	return bid.BidRequest{
		AuctionID:       auction,
		UserID:          uuid.New(),
		AmountCents:     amount,
		ExpectedVersion: version,
	}
}

func seedAuction(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO auctions (id, title, min_increment_cents, ends_at)
		 VALUES ($1, $2, $3, now() + make_interval(secs => $4))`,
		id, t.Name(), minIncrement, time.Minute.Seconds(),
	); err != nil {
		t.Fatalf("seed auction: %v", err)
	}
	return id
}

func ptr(v int64) *int64 { return &v }
