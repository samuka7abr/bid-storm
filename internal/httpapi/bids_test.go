package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/httpapi"
)

// stubEngine answers with a fixed result, because what is under test here is the
// table mapping Outcome to a response — tying a pure switch to a container would
// make a slow test out of a fast one.
type stubEngine struct {
	res bid.BidResult
	err error
	got bid.BidRequest
}

func (s *stubEngine) PlaceBid(_ context.Context, req bid.BidRequest) (bid.BidResult, error) {
	s.got = req
	return s.res, s.err
}

// The state every answer about an existing auction carries.
func currentState() bid.AuctionState {
	return bid.AuctionState{
		Version:           4187,
		HighestBidCents:   918400,
		MinIncrementCents: 100,
		Status:            bid.StatusOpen,
		EndsAt:            time.Now().Add(time.Minute),
	}
}

func TestPlaceBidMapsEveryOutcome(t *testing.T) {
	tests := []struct {
		name      string
		res       bid.BidResult
		err       error
		wantCode  int
		wantError string
		wantRetry bool
		wantState bool
	}{
		{
			name:      "accepted",
			res:       bid.BidResult{Outcome: bid.Accepted, Seq: 4188, BidID: uuid.New(), Current: currentState()},
			wantCode:  http.StatusCreated,
			wantState: true,
		},
		{
			name:      "conflict",
			res:       bid.BidResult{Outcome: bid.Conflict, Current: currentState()},
			wantCode:  http.StatusConflict,
			wantError: "version_conflict",
			wantRetry: true,
			wantState: true,
		},
		{
			// Retryable like the 409: if it were terminal, the shard's VU would
			// send one request and give up while the optimistic VU sent ten, and
			// accepted/s would stop comparing anything.
			name:      "too low",
			res:       bid.BidResult{Outcome: bid.TooLow, Current: currentState()},
			wantCode:  http.StatusUnprocessableEntity,
			wantError: "too_low",
			wantRetry: true,
			wantState: true,
		},
		{
			name:      "closed",
			res:       bid.BidResult{Outcome: bid.Closed, Current: currentState()},
			wantCode:  http.StatusGone,
			wantError: "auction_closed",
			wantState: true,
		},
		{
			// No auction, no state: a 404 must not claim a nonexistent auction is
			// worth zero cents.
			name:      "not found",
			res:       bid.BidResult{Outcome: bid.NotFound},
			wantCode:  http.StatusNotFound,
			wantError: "auction_not_found",
		},
		{
			name:      "invalid",
			res:       bid.BidResult{Outcome: bid.Invalid},
			wantCode:  http.StatusBadRequest,
			wantError: "expected_version_required",
		},
		{
			name:      "infrastructure failure",
			err:       errors.New("connection refused"),
			wantCode:  http.StatusServiceUnavailable,
			wantError: "unavailable",
			wantRetry: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := &stubEngine{res: tc.res, err: tc.err}
			r := router(engine, nil)

			code, body := post(t, r, "/auctions/"+uuid.NewString()+"/bids", uuid.NewString(),
				`{"amountCents":918500,"expectedVersion":4187}`)

			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %v)", code, tc.wantCode, body)
			}
			if got, _ := body["error"].(string); got != tc.wantError {
				t.Errorf("error = %q, want %q", got, tc.wantError)
			}
			if got, _ := body["retryable"].(bool); got != tc.wantRetry {
				t.Errorf("retryable = %v, want %v", got, tc.wantRetry)
			}

			// Decisão 2 in the wire format: state on every answer that has one,
			// and on no answer that does not.
			for _, field := range []string{"currentVersion", "currentHighestBid", "minNextBid"} {
				if _, ok := body[field]; ok != tc.wantState {
					t.Errorf("field %q present = %v, want %v", field, ok, tc.wantState)
				}
			}
			if tc.wantState && body["minNextBid"] != float64(918500) {
				t.Errorf("minNextBid = %v, want 918500", body["minNextBid"])
			}
			if tc.wantCode == http.StatusCreated && body["seq"] != float64(4188) {
				t.Errorf("seq = %v, want 4188", body["seq"])
			}
		})
	}
}

// The engine must receive what the request carried, or the mapping table above
// is testing a stub against itself.
func TestPlaceBidForwardsTheRequest(t *testing.T) {
	engine := &stubEngine{res: bid.BidResult{Outcome: bid.Accepted, Current: currentState()}}
	r := router(engine, nil)

	auction, user := uuid.NewString(), uuid.NewString()
	if code, body := post(t, r, "/auctions/"+auction+"/bids", user,
		`{"amountCents":918500,"expectedVersion":4187}`); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %v)", code, body)
	}

	if engine.got.AuctionID.String() != auction {
		t.Errorf("auction id = %s, want %s", engine.got.AuctionID, auction)
	}
	if engine.got.UserID.String() != user {
		t.Errorf("user id = %s, want %s", engine.got.UserID, user)
	}
	if engine.got.AmountCents != 918500 {
		t.Errorf("amount = %d, want 918500", engine.got.AmountCents)
	}
	if engine.got.ExpectedVersion == nil || *engine.got.ExpectedVersion != 4187 {
		t.Errorf("expected version = %v, want 4187", engine.got.ExpectedVersion)
	}
	// Idempotency arrives in etapa 2: every row written here leaves the column
	// NULL.
	if engine.got.IdempotencyKey != "" {
		t.Errorf("idempotency key = %q, want empty in etapa 1", engine.got.IdempotencyKey)
	}
}

// Requests the engine must never see: they would cost a round-trip to learn
// something already known, and the amount would turn a client error into a 503
// by way of CHECK (amount_cents > 0).
func TestPlaceBidRejectsBeforeTheEngine(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		user      string
		body      string
		wantCode  int
		wantError string
	}{
		{"no identity", "/auctions/" + uuid.NewString() + "/bids", "", `{"amountCents":900,"expectedVersion":1}`, http.StatusBadRequest, "invalid_user_id"},
		{"identity is not a uuid", "/auctions/" + uuid.NewString() + "/bids", "bidder-7", `{"amountCents":900,"expectedVersion":1}`, http.StatusBadRequest, "invalid_user_id"},
		{"auction id is not a uuid", "/auctions/not-a-uuid/bids", uuid.NewString(), `{"amountCents":900,"expectedVersion":1}`, http.StatusNotFound, "auction_not_found"},
		{"amount missing", "/auctions/" + uuid.NewString() + "/bids", uuid.NewString(), `{"expectedVersion":1}`, http.StatusBadRequest, "invalid_amount"},
		{"amount not positive", "/auctions/" + uuid.NewString() + "/bids", uuid.NewString(), `{"amountCents":-5,"expectedVersion":1}`, http.StatusBadRequest, "invalid_amount"},
		{"body is not json", "/auctions/" + uuid.NewString() + "/bids", uuid.NewString(), `{`, http.StatusBadRequest, "invalid_amount"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := &stubEngine{res: bid.BidResult{Outcome: bid.Accepted}}
			r := router(engine, nil)

			code, body := post(t, r, tc.path, tc.user, tc.body)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %v)", code, tc.wantCode, body)
			}
			if got, _ := body["error"].(string); got != tc.wantError {
				t.Errorf("error = %q, want %q", got, tc.wantError)
			}
			if engine.got != (bid.BidRequest{}) {
				t.Errorf("the engine was called with %+v, want no call at all", engine.got)
			}
		})
	}
}

func router(engine bid.BidEngine, auctions httpapi.AuctionStore) http.Handler {
	// No readiness conditions and no idempotency middleware: what is under test
	// here is the table that maps an Outcome to a response, and the bid handler
	// does not know either of them exists.
	return httpapi.New(httpapi.Deps{
		Metrics:  http.NotFoundHandler(),
		Engine:   engine,
		Auctions: auctions,
	})
}

func post(t *testing.T, r http.Handler, path, user, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-User-Id", user)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	decoded := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %s body %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, decoded
}
