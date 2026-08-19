package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/idem"
	"github.com/samuka7abr/bid-storm/internal/store"
)

// The error codes the API publishes. They are the client's whole vocabulary for
// what went wrong, so they are spelled once.
const (
	codeVersionConflict         = "version_conflict"
	codeTooLow                  = "too_low"
	codeAuctionClosed           = "auction_closed"
	codeAuctionNotFound         = "auction_not_found"
	codeExpectedVersionRequired = "expected_version_required"
	codeInvalidUserID           = "invalid_user_id"
	codeInvalidAmount           = "invalid_amount"
	codeInvalidBody             = "invalid_body"
	codeInvalidTitle            = "invalid_title"
	codeInvalidEndsAt           = "invalid_ends_at"
	codeInvalidMinIncrement     = "invalid_min_increment"
	codeInvalidStartingBid      = "invalid_starting_bid"
	codeUnavailable             = "unavailable"
	// The two codes of the idempotency middleware. No handler here answers
	// them — the middleware sits above this package and cannot import it back —
	// so they are aliased rather than retyped: the client's whole vocabulary
	// stays listed in one place, with one spelling of each.
	codeInvalidIdempotencyKey = idem.CodeInvalidKey
	codeIdempotencyInFlight   = idem.CodeInFlight
)

// AuctionStateView is the state every answer about an existing auction carries.
//
// It is embedded rather than repeated, so the compiler enforces what the client
// depends on: no 201, 409, 422 or 410 body can be built without the auction's
// state, and the retry after a rejection stays a single POST instead of a GET
// followed by a POST.
type AuctionStateView struct {
	CurrentVersion    int64 `json:"currentVersion"`
	CurrentHighestBid int64 `json:"currentHighestBid"`
	MinNextBid        int64 `json:"minNextBid"`
}

func stateView(s bid.AuctionState) AuctionStateView {
	return AuctionStateView{
		CurrentVersion:    s.Version,
		CurrentHighestBid: s.HighestBidCents,
		MinNextBid:        s.MinNextBid(),
	}
}

// BidAccepted is the 201. It means durable, in all three engines.
type BidAccepted struct {
	AuctionStateView
	BidID uuid.UUID `json:"bidId"`
	// seq carries the same number as currentVersion, because seq is the
	// resulting version. Both are published because both are in the contract.
	Seq int64 `json:"seq"`
}

// BidRejected is the 409, 422 and 410. One envelope for the three: 409 and 422
// are the same event seen through different mechanisms, and the distinction
// exists for the metric rather than for the client's decision.
type BidRejected struct {
	AuctionStateView
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}

// ErrorResponse is what comes back when there is no auction state to report:
// 404, the 400s, and 503. It deliberately has no AuctionStateView — a
// nonexistent auction is not worth zero cents, it is worth nothing at all.
type ErrorResponse struct {
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}

// AuctionView is the body of POST /auctions and GET /auctions/:id.
type AuctionView struct {
	AuctionStateView
	ID     uuid.UUID `json:"id"`
	Title  string    `json:"title"`
	Status string    `json:"status"`
	EndsAt time.Time `json:"endsAt"`
}

// auctionView derives status instead of publishing the column, so that the two
// routes of this API can never disagree about the same auction.
func auctionView(a store.Auction) AuctionView {
	status := bid.StatusOpen
	if a.IsClosed() {
		status = bid.StatusClosed
	}
	return AuctionView{
		AuctionStateView: stateView(a.State),
		ID:               a.ID,
		Title:            a.Title,
		Status:           string(status),
		EndsAt:           a.State.EndsAt,
	}
}
