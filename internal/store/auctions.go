// Package store holds the SQL that is not a bid.
//
// POST /auctions and GET /auctions/:id have to talk to the database without
// going through an engine — they create and they read, they do not bid. Putting
// this SQL inside internal/bid would make the contract of all three engines
// carry two routes none of them serves.
package store

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

// ErrNotFound keeps pgx out of the HTTP layer, which has no business knowing
// which driver produced the miss.
var ErrNotFound = errors.New("auction not found")

// Auction is one row plus the database clock that read it.
//
// The clock travels with the row because status is derived, never returned raw:
// the column stays 'open' on expired auctions until the closerd of etapa 4
// starts writing it, and if the read published it as-is then this route and the
// bid route would disagree about the same auction — the panel of etapa 5b would
// render a dead auction as live.
type Auction struct {
	ID    uuid.UUID
	Title string
	State bid.AuctionState
	Now   time.Time
}

// IsClosed applies the one closing rule, decided by the clock that read the row.
func (a Auction) IsClosed() bool { return a.State.IsClosed(a.Now) }

// NewAuction is what POST /auctions carries, already validated.
type NewAuction struct {
	Title             string
	StartingBidCents  int64
	MinIncrementCents int64
	EndsAt            time.Time
}

// Auctions reads and writes the auctions table.
type Auctions struct {
	pool *pgxpool.Pool
}

// New returns the store backed by pool.
func New(pool *pgxpool.Pool) *Auctions {
	return &Auctions{pool: pool}
}

const createAuction = `
INSERT INTO auctions (id, title, highest_bid_cents, min_increment_cents, ends_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING version, highest_bid_cents, min_increment_cents, status, ends_at, now()`

const getAuction = `
SELECT title, version, highest_bid_cents, min_increment_cents, status, ends_at, now()
  FROM auctions
 WHERE id = $1`

// Create inserts the auction and returns it as the database now sees it. The id
// is generated here rather than accepted from the client: nothing in the schema
// owns an auction, so nothing can be allowed to pick its identifier.
func (a *Auctions) Create(ctx context.Context, req NewAuction) (Auction, error) {
	auction := Auction{ID: uuid.New(), Title: req.Title}
	var status string

	if err := a.pool.QueryRow(ctx, createAuction,
		auction.ID, req.Title, req.StartingBidCents, req.MinIncrementCents, req.EndsAt,
	).Scan(
		&auction.State.Version,
		&auction.State.HighestBidCents,
		&auction.State.MinIncrementCents,
		&status,
		&auction.State.EndsAt,
		&auction.Now,
	); err != nil {
		return Auction{}, fmt.Errorf("create auction: %w", err)
	}

	auction.State.Status = bid.Status(status)
	return auction, nil
}

// Get returns the auction, or ErrNotFound.
func (a *Auctions) Get(ctx context.Context, id uuid.UUID) (Auction, error) {
	auction := Auction{ID: id}
	var status string

	err := a.pool.QueryRow(ctx, getAuction, id).Scan(
		&auction.Title,
		&auction.State.Version,
		&auction.State.HighestBidCents,
		&auction.State.MinIncrementCents,
		&status,
		&auction.State.EndsAt,
		&auction.Now,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Auction{}, ErrNotFound
	case err != nil:
		return Auction{}, fmt.Errorf("get auction: %w", err)
	}

	auction.State.Status = bid.Status(status)
	return auction, nil
}
