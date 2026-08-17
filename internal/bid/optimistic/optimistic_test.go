package optimistic_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/bid/enginetest"
	"github.com/samuka7abr/bid-storm/internal/bid/optimistic"
	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

func TestConformance(t *testing.T) {
	enginetest.RunConformance(t, func(pool *pgxpool.Pool) bid.BidEngine {
		return optimistic.New(pool)
	})
}

// Conflict and Invalid stay out of the conformance suite on purpose: both depend
// on ExpectedVersion, which the pessimistic engine and the shard ignore by
// contract, so they would fail these two by design.
func TestStaleVersionIsConflictEvenWhenTheAmountIsEnough(t *testing.T) {
	pg := testsupport.Start(t)
	engine := optimistic.New(pg.Pool)
	auction := seedAuction(t, pg.Pool)

	first, err := engine.PlaceBid(context.Background(), request(auction, 100, ptr(int64(0))))
	if err != nil || first.Outcome != bid.Accepted {
		t.Fatalf("first bid: outcome %v, err %v", first.Outcome, err)
	}

	// Version 0 is stale now, but 900 clears the minimum of 200 comfortably. The
	// ordering of decisão 20 is exactly what makes this a conflict: under
	// contention the amount is almost always stale too, and checking it first
	// would send bid_outcomes_total{outcome="conflict"} to zero.
	res, err := engine.PlaceBid(context.Background(), request(auction, 900, ptr(int64(0))))
	if err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	if res.Outcome != bid.Conflict {
		t.Fatalf("outcome = %v, want conflict", res.Outcome)
	}
	if res.Current.Version != 1 || res.Current.MinNextBid() != 200 {
		t.Errorf("current = %+v, want version 1 and minNextBid 200", res.Current)
	}
}

func TestUpToDateVersionWithTooLittleIsTooLow(t *testing.T) {
	pg := testsupport.Start(t)
	engine := optimistic.New(pg.Pool)
	auction := seedAuction(t, pg.Pool)

	if _, err := engine.PlaceBid(context.Background(), request(auction, 100, ptr(int64(0)))); err != nil {
		t.Fatalf("first bid: %v", err)
	}

	res, err := engine.PlaceBid(context.Background(), request(auction, 150, ptr(int64(1))))
	if err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	if res.Outcome != bid.TooLow {
		t.Fatalf("outcome = %v, want too_low", res.Outcome)
	}
}

// The optimistic engine requires expectedVersion; the other two ignore it. No
// handler is allowed to branch on the strategy, so "you did not give me one"
// has to come back as an outcome — and without touching the database.
func TestMissingExpectedVersionIsInvalid(t *testing.T) {
	pg := testsupport.Start(t)
	engine := optimistic.New(pg.Pool)
	auction := seedAuction(t, pg.Pool)

	res, err := engine.PlaceBid(context.Background(), request(auction, 100, nil))
	if err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	if res.Outcome != bid.Invalid {
		t.Fatalf("outcome = %v, want invalid", res.Outcome)
	}
	if res.Current != (bid.AuctionState{}) {
		t.Errorf("current = %+v, want the zero state: nothing was read", res.Current)
	}
}

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
		 VALUES ($1, $2, 100, now() + make_interval(secs => $3))`,
		id, t.Name(), time.Minute.Seconds(),
	); err != nil {
		t.Fatalf("seed auction: %v", err)
	}
	return id
}

func ptr(v int64) *int64 { return &v }
