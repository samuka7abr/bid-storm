package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/store"
)

// stubStore stands in for Postgres, because what these handlers own is
// validation and the derivation of status.
type stubStore struct {
	auction store.Auction
	err     error
	created *store.NewAuction
}

func (s *stubStore) Create(_ context.Context, req store.NewAuction) (store.Auction, error) {
	s.created = &req
	return s.auction, s.err
}

func (s *stubStore) Get(context.Context, uuid.UUID) (store.Auction, error) {
	return s.auction, s.err
}

func TestCreateAuctionRejectsBadInput(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{"empty title", `{"title":"","startingBidCents":0,"minIncrementCents":100,"endsAt":"` + future + `"}`, "invalid_title"},
		{"no endsAt", `{"title":"Item","startingBidCents":0,"minIncrementCents":100}`, "invalid_ends_at"},
		{"endsAt in the past", `{"title":"Item","startingBidCents":0,"minIncrementCents":100,"endsAt":"2020-01-01T00:00:00Z"}`, "invalid_ends_at"},
		{"increment not positive", `{"title":"Item","startingBidCents":0,"minIncrementCents":0,"endsAt":"` + future + `"}`, "invalid_min_increment"},
		{"negative starting bid", `{"title":"Item","startingBidCents":-1,"minIncrementCents":100,"endsAt":"` + future + `"}`, "invalid_starting_bid"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auctions := &stubStore{}
			code, body := post(t, router(nil, auctions), "/auctions", "", tc.body)

			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %v)", code, body)
			}
			if got, _ := body["error"].(string); got != tc.wantError {
				t.Errorf("error = %q, want %q", got, tc.wantError)
			}
			if auctions.created != nil {
				t.Errorf("the store was called with %+v, want no call at all", *auctions.created)
			}
			// A 400 has no auction to report state about.
			if _, ok := body["currentVersion"]; ok {
				t.Error("the 400 carries currentVersion, want no state at all")
			}
		})
	}
}

func TestCreateAuctionReturnsTheFreshState(t *testing.T) {
	endsAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	auctions := &stubStore{auction: store.Auction{
		ID:    uuid.New(),
		Title: "Item 1",
		State: bid.AuctionState{MinIncrementCents: 100, Status: bid.StatusOpen, EndsAt: endsAt},
		Now:   time.Now(),
	}}

	code, body := post(t, router(nil, auctions), "/auctions", "",
		`{"title":"Item 1","startingBidCents":0,"minIncrementCents":100,"endsAt":"`+endsAt.Format(time.RFC3339)+`"}`)

	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %v)", code, body)
	}
	if body["currentVersion"] != float64(0) {
		t.Errorf("currentVersion = %v, want 0", body["currentVersion"])
	}
	if body["minNextBid"] != float64(100) {
		t.Errorf("minNextBid = %v, want 100", body["minNextBid"])
	}
	if body["status"] != "open" {
		t.Errorf("status = %v, want open", body["status"])
	}
	// The id is the server's: nothing in the schema owns an auction, so nothing
	// gets to pick its identifier.
	if body["id"] != auctions.auction.ID.String() {
		t.Errorf("id = %v, want %s", body["id"], auctions.auction.ID)
	}
	if auctions.created == nil || auctions.created.Title != "Item 1" {
		t.Errorf("store received %+v, want the posted auction", auctions.created)
	}
}

// status is derived from the same rule the engine uses, against the clock the
// query itself returned. Publishing the raw column would make this route and the
// bid route disagree about the same auction until etapa 4 starts writing it.
func TestGetAuctionDerivesStatus(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		auction store.Auction
		want    string
	}{
		{
			name: "running",
			auction: store.Auction{
				State: bid.AuctionState{Status: bid.StatusOpen, EndsAt: now.Add(time.Minute)},
				Now:   now,
			},
			want: "open",
		},
		{
			// The column still says open, and will until the closerd of etapa 4.
			name: "past ends_at with the column still open",
			auction: store.Auction{
				State: bid.AuctionState{Status: bid.StatusOpen, EndsAt: now.Add(-time.Minute)},
				Now:   now,
			},
			want: "closed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.auction.ID = uuid.New()
			code, body := getJSON(t, router(nil, &stubStore{auction: tc.auction}), "/auctions/"+tc.auction.ID.String())

			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if body["status"] != tc.want {
				t.Errorf("status = %v, want %q", body["status"], tc.want)
			}
		})
	}
}

func TestGetAuctionNotFound(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		store *stubStore
	}{
		{"unknown id", "/auctions/" + uuid.NewString(), &stubStore{err: store.ErrNotFound}},
		{"id is not a uuid", "/auctions/not-a-uuid", &stubStore{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getJSON(t, router(nil, tc.store), tc.path)
			if code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %v)", code, body)
			}
			if body["error"] != "auction_not_found" {
				t.Errorf("error = %v, want auction_not_found", body["error"])
			}
		})
	}
}

func getJSON(t *testing.T, r http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	decoded := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %s body %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, decoded
}
