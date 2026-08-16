// Command seed fills the auctions table before a benchmark cell.
//
// Contention is defined by how many auctions exist, so seeding is part of the
// experiment rather than a chore around it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samuka7abr/auction-system/internal/config"
	"github.com/samuka7abr/auction-system/internal/db"
)

type options struct {
	auctions     int
	endsIn       time.Duration
	minIncrement int64
	startingBid  int64
	truncate     bool
}

// auctionRef is one entry of the manifest. It carries everything a virtual user
// needs in order to place its first bid, so that a cell with 1000 auctions does
// not open with 1000 preparation GETs — which at that scale is pure noise on
// top of the measurement.
type auctionRef struct {
	ID         uuid.UUID `json:"id"`
	Version    int64     `json:"version"`
	MinNextBid int64     `json:"minNextBid"`
}

func main() {
	var (
		opts options
		out  string
	)
	flag.IntVar(&opts.auctions, "auctions", 1, "how many auctions to seed")
	flag.DurationVar(&opts.endsIn, "ends-in", 5*time.Minute, "how far in the future every auction closes")
	flag.Int64Var(&opts.minIncrement, "min-increment", 100, "minimum bid increment, in cents")
	flag.Int64Var(&opts.startingBid, "starting-bid", 0, "initial highest bid, in cents")
	flag.BoolVar(&opts.truncate, "truncate", false, "wipe bids and auctions before seeding")
	flag.StringVar(&out, "out", "bench/auctions.json", "where to write the manifest")
	flag.Parse()

	if err := execute(opts, out); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func execute(opts options, out string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBPoolSize)
	if err != nil {
		return err
	}
	defer pool.Close()

	started := time.Now()
	refs, err := seed(ctx, pool, opts)
	if err != nil {
		return err
	}
	if err := writeManifest(out, refs); err != nil {
		return err
	}

	fmt.Printf("seeded %d auctions in %s, manifest at %s\n", len(refs), time.Since(started).Round(time.Millisecond), out)
	return nil
}

func seed(ctx context.Context, pool *pgxpool.Pool, opts options) ([]auctionRef, error) {
	// The first step of every benchmark cell: cell 36 must not write into a
	// table holding cell 1..35's rows and their stale statistics, or the last
	// strategy in the list looks worst for reasons of ordering.
	if opts.truncate {
		if _, err := pool.Exec(ctx, `TRUNCATE bids, auctions RESTART IDENTITY CASCADE`); err != nil {
			return nil, fmt.Errorf("truncate: %w", err)
		}
	}

	endsAt := time.Now().Add(opts.endsIn)
	refs := make([]auctionRef, opts.auctions)
	rows := make([][]any, opts.auctions)
	for i := range refs {
		id := uuid.New()
		// minNextBid is the auction's rule, not the bidder's: if every client
		// picked its own increment, two VUs would compete under different rules
		// and the comparison between strategies would inherit the asymmetry.
		refs[i] = auctionRef{ID: id, MinNextBid: opts.startingBid + opts.minIncrement}
		rows[i] = []any{id, fmt.Sprintf("auction-%d", i+1), opts.startingBid, opts.minIncrement, endsAt}
	}

	// CopyFrom, not a loop of INSERTs: preparation time that leaks into a
	// benchmark cell is measurement error.
	inserted, err := pool.CopyFrom(ctx,
		pgx.Identifier{"auctions"},
		[]string{"id", "title", "highest_bid_cents", "min_increment_cents", "ends_at"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return nil, fmt.Errorf("copy auctions: %w", err)
	}
	if int(inserted) != opts.auctions {
		return nil, fmt.Errorf("inserted %d auctions, wanted %d", inserted, opts.auctions)
	}

	return refs, nil
}

func writeManifest(path string, refs []auctionRef) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create manifest directory: %w", err)
		}
	}
	data, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}
