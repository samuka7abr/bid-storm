package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

func TestSeedWritesEveryAuctionAndAMatchingManifest(t *testing.T) {
	ctx := context.Background()
	pg := testsupport.Start(t)

	const want = 1000
	opts := options{
		auctions:     want,
		endsIn:       5 * time.Minute,
		minIncrement: 100,
		startingBid:  0,
		truncate:     true,
	}

	refs, err := seed(ctx, pg.Pool, opts)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	var rows, distinct int
	if err := pg.Pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT id) FROM auctions`).Scan(&rows, &distinct); err != nil {
		t.Fatalf("count auctions: %v", err)
	}
	if rows != want || distinct != want {
		t.Errorf("count/distinct = %d/%d, want %d/%d", rows, distinct, want, want)
	}

	path := filepath.Join(t.TempDir(), "auctions.json")
	if err := writeManifest(path, refs); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest []auctionRef
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest) != want {
		t.Fatalf("manifest holds %d entries, want %d", len(manifest), want)
	}

	// The k6 setup() reads this file instead of doing a GET per auction, so
	// every entry has to be usable on its own.
	seen := make(map[uuid.UUID]bool, want)
	for i, ref := range manifest {
		if ref.ID == uuid.Nil {
			t.Fatalf("entry %d has no id", i)
		}
		if seen[ref.ID] {
			t.Fatalf("entry %d repeats id %s", i, ref.ID)
		}
		seen[ref.ID] = true
		if ref.Version != 0 {
			t.Errorf("entry %d version = %d, want 0", i, ref.Version)
		}
		if ref.MinNextBid != opts.startingBid+opts.minIncrement {
			t.Errorf("entry %d minNextBid = %d, want %d", i, ref.MinNextBid, opts.startingBid+opts.minIncrement)
		}
	}

	// Every auction in the manifest must exist in the database with the state
	// the manifest promises.
	var open int
	if err := pg.Pool.QueryRow(ctx, `
		SELECT count(*) FROM auctions
		WHERE version = 0 AND highest_bid_cents = 0 AND min_increment_cents = $1
		  AND status = 'open' AND now() < ends_at`, opts.minIncrement).Scan(&open); err != nil {
		t.Fatalf("verify auctions: %v", err)
	}
	if open != want {
		t.Errorf("%d auctions match the seeded state, want %d", open, want)
	}

	// -truncate is the first step of every benchmark cell.
	if _, err := seed(ctx, pg.Pool, options{auctions: 1, endsIn: time.Minute, minIncrement: 100, truncate: true}); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if err := pg.Pool.QueryRow(ctx, `SELECT count(*) FROM auctions`).Scan(&rows); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if rows != 1 {
		t.Errorf("after truncate + seed 1, count = %d, want 1", rows)
	}
}
