package bid_test

import (
	"testing"
	"time"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

func TestMinNextBid(t *testing.T) {
	s := bid.AuctionState{HighestBidCents: 918500, MinIncrementCents: 100}
	if got := s.MinNextBid(); got != 918600 {
		t.Errorf("MinNextBid() = %d, want 918600", got)
	}
}

// The closing rule is one rule for two callers: the engine's classify and
// GET /auctions/:id. Both feed it the database clock.
func TestIsClosed(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		state bid.AuctionState
		want  bool
	}{
		{"open and running", bid.AuctionState{Status: bid.StatusOpen, EndsAt: now.Add(time.Minute)}, false},
		{"open but past ends_at", bid.AuctionState{Status: bid.StatusOpen, EndsAt: now.Add(-time.Minute)}, true},
		// The instant of ends_at is already closed: the accepting UPDATE guards
		// with now() < ends_at, and the two must not disagree on the boundary.
		{"open exactly at ends_at", bid.AuctionState{Status: bid.StatusOpen, EndsAt: now}, true},
		// status is only written from etapa 4 on, but IsClosed already covers it
		// so that no handler changes when the closerd arrives.
		{"materialised closed", bid.AuctionState{Status: bid.StatusClosed, EndsAt: now.Add(time.Hour)}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.IsClosed(now); got != tc.want {
				t.Errorf("IsClosed() = %v, want %v", got, tc.want)
			}
		})
	}
}

// These strings are the outcome label of bid_outcomes_total, so renaming one
// silently breaks every query in the report.
func TestOutcomeString(t *testing.T) {
	want := map[bid.Outcome]string{
		bid.Accepted: "accepted",
		bid.Conflict: "conflict",
		bid.TooLow:   "too_low",
		bid.Closed:   "closed",
		bid.NotFound: "not_found",
		bid.Invalid:  "invalid",
	}
	for outcome, name := range want {
		if got := outcome.String(); got != name {
			t.Errorf("Outcome(%d).String() = %q, want %q", outcome, got, name)
		}
	}
	if got := bid.Outcome(200).String(); got != "unknown" {
		t.Errorf("unnamed outcome String() = %q, want %q", got, "unknown")
	}
}
