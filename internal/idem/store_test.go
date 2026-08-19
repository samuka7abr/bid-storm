package idem_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/idem"
	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

// The layout the spec fixes: a hash at idem:<uuid>, 30s while a request is in
// flight, 5min once the entry is terminal. Spelled out here rather than read
// from the package, so that changing any of the three has to be deliberate —
// the 30s is what caps the damage of a process that dies mid-request, and the
// 5min is what makes a duplicate impossible to lose inside a cell.
const (
	keyPrefix   = "idem:"
	inFlightTTL = 30 * time.Second
	terminalTTL = 5 * time.Minute
)

func TestClaimGrantsPassageCountsAndTakesTheMark(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	store := idem.NewStore(rdb.Client)
	ctx := context.Background()
	key := uuid.NewString()

	verdict, err := store.Claim(ctx, key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if verdict.Kind != idem.Pass {
		t.Fatalf("kind = %q, want %q", verdict.Kind, idem.Pass)
	}
	if verdict.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", verdict.Attempts)
	}

	entry := hash(t, rdb, key)
	if entry["attempts"] != "1" {
		t.Errorf("attempts field = %q, want 1", entry["attempts"])
	}
	if entry["busy"] != "1" {
		t.Errorf("busy field = %q, want 1: the mark is what defines a duplicate", entry["busy"])
	}
	if _, stored := entry["done"]; stored {
		t.Error("done is set before the request finished")
	}

	if got := ttl(t, rdb, key); got <= 0 || got > inFlightTTL {
		t.Errorf("ttl = %v, want (0, %v]", got, inFlightTTL)
	}
}

// The second request under the same key is turned away without being counted:
// it never reached the engine, so it is not an attempt at anything, and letting
// it into attempts would make bid_attempts_per_accept measure the generator.
func TestClaimSaysBusyWhileTheFirstIsInFlight(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	store := idem.NewStore(rdb.Client)
	ctx := context.Background()
	key := uuid.NewString()

	if _, err := store.Claim(ctx, key); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	verdict, err := store.Claim(ctx, key)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if verdict.Kind != idem.Busy {
		t.Fatalf("kind = %q, want %q", verdict.Kind, idem.Busy)
	}
	if got := hash(t, rdb, key)["attempts"]; got != "1" {
		t.Errorf("attempts field = %q, want 1: a barred duplicate is not an attempt", got)
	}
}

// Rejection stores nothing and releases the mark. Without the release, the
// re-aimed retry arriving 40ms later would find the key taken and get a 425 —
// the bidder stuck against the middleware instead of against the auction.
func TestFinishOnRejectionReleasesTheMarkAndKeepsTheCount(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	store := idem.NewStore(rdb.Client)
	ctx := context.Background()
	key := uuid.NewString()

	if _, err := store.Claim(ctx, key); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Finish(ctx, key, false, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	entry := hash(t, rdb, key)
	if _, marked := entry["busy"]; marked {
		t.Error("busy survived a rejection")
	}
	if _, stored := entry["done"]; stored {
		t.Error("a rejection was stored, and only the 201 may be")
	}

	// The count is what makes the re-aimed retry attempt number two, which is
	// the whole point of the key naming the logical bid.
	verdict, err := store.Claim(ctx, key)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if verdict.Kind != idem.Pass {
		t.Fatalf("kind = %q, want %q", verdict.Kind, idem.Pass)
	}
	if verdict.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", verdict.Attempts)
	}
}

func TestFinishOnAcceptStoresTheBodyAndPromotesTheTTL(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	store := idem.NewStore(rdb.Client)
	ctx := context.Background()
	key := uuid.NewString()
	body := []byte(`{"bidId":"e7d0","seq":7,"currentVersion":7}`)

	if _, err := store.Claim(ctx, key); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Finish(ctx, key, true, body); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	entry := hash(t, rdb, key)
	if entry["done"] != string(body) {
		t.Errorf("done = %q, want %q", entry["done"], body)
	}
	if _, marked := entry["busy"]; marked {
		t.Error("busy survived the accept")
	}
	if got := ttl(t, rdb, key); got <= inFlightTTL || got > terminalTTL {
		t.Errorf("ttl = %v, want (%v, %v]", got, inFlightTTL, terminalTTL)
	}

	verdict, err := store.Claim(ctx, key)
	if err != nil {
		t.Fatalf("Claim after the accept: %v", err)
	}
	if verdict.Kind != idem.Replay {
		t.Fatalf("kind = %q, want %q", verdict.Kind, idem.Replay)
	}
	if string(verdict.Body) != string(body) {
		t.Errorf("body = %q, want %q", verdict.Body, body)
	}
}

func hash(t *testing.T, rdb *testsupport.Redis, key string) map[string]string {
	t.Helper()
	entry, err := rdb.Client.HGetAll(context.Background(), keyPrefix+key).Result()
	if err != nil {
		t.Fatalf("HGETALL: %v", err)
	}
	return entry
}

func ttl(t *testing.T, rdb *testsupport.Redis, key string) time.Duration {
	t.Helper()
	got, err := rdb.Client.PTTL(context.Background(), keyPrefix+key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	return got
}
