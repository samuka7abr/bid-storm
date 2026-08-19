// Package idem turns a bid away when it has already been made.
//
// It sits above the strategy switch: one component, identical for the three
// engines, that does not know and cannot know which engine is behind it. That
// is why it may carry the strategy label without breaking decisão 23 — it is
// not each engine measuring itself, it is a second single boundary.
//
// The key names the LOGICAL BID, not the request (decisão 31). The bidder of
// decisão 5 re-aims after every rejection, so attempt 2 carries, by
// construction, a different body from attempt 1: under request semantics the
// client would either change key on every attempt, leaving nothing to correlate
// the three, or keep it and get its own retry refused as key reuse. So there is
// no body fingerprint here, and what tells a duplicate from a retry is time: a
// retry only leaves after the previous answer arrived, while a duplicate is
// concurrent with the original or later than a bid already finished. The busy
// mark is not a race optimisation — it is the definition of a duplicate in this
// API.
package idem

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The two headers of this contract. Replayed goes in a header and not in a
// field of the body because the body has to stay identical byte for byte, and
// one extra field would be exactly the difference decisão 34 forbids.
const (
	HeaderKey      = "X-Idempotency-Key"
	HeaderReplayed = "X-Idempotency-Replayed"
)

// The two error codes this middleware publishes. They are named here because
// this is the component that answers them, and internal/httpapi spells them
// once by pointing at these.
const (
	CodeInvalidKey  = "invalid_idempotency_key"
	CodeInFlight    = "idempotency_in_flight"
	codeUnavailable = "unavailable"
)

// The content type gin writes for a JSON body. The replay reproduces the
// stored 201 down to this header, or the two answers would not be identical.
const contentTypeJSON = "application/json; charset=utf-8"

// errorBody has the shape of httpapi.ErrorResponse, and is declared here
// because this package cannot import the one that sits below it. It is the
// envelope for answers that carry no auction state — a 425 consulted no engine,
// so there is nothing to report about the auction.
type errorBody struct {
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}

// Middleware guards one route: POST /auctions/:id/bids, after RequireUserID.
//
// Without the header it is a pass-through and Redis is never touched, which is
// what makes the "no idempotency" control cell a choice of the load generator
// rather than an environment variable (decisão 41).
func Middleware(store *Store, m Metrics, log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader(HeaderKey)
		if key == "" {
			c.Next()
			return
		}

		// Without a body fingerprint, the only defence against two honest
		// clients picking the same key is the key space; a UUID settles that
		// and caps the size of what gets written to Redis for free. The raw
		// header travels on, unchanged, so the key in Redis and the key in
		// bids.idempotency_key are always the same string.
		if _, err := uuid.Parse(key); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, errorBody{Error: CodeInvalidKey})
			return
		}

		verdict, err := store.Claim(c.Request.Context(), key)
		if err != nil {
			// Failing closed. Letting the bid through unguarded would be the
			// system making a weaker promise in silence, and it would not even
			// avoid the error: the concurrent duplicates would reach the
			// engines, both would write, and the partial unique index would
			// refuse the second one with a 503 anyway — after the work was
			// spent (decisão 36). One log line, the same exception the handler
			// already makes for its own 503.
			log.Error("idempotency claim failed", "error", err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, errorBody{Error: codeUnavailable, Retryable: true})
			return
		}

		switch verdict.Kind {
		case Replay:
			m.Replayed.Inc()
			c.Abort()
			c.Header(HeaderReplayed, "true")
			// Verbatim. A replay that rebuilt the body from the state of now
			// would answer with today's minNextBid inside the response to a bid
			// from before, and the client would have no way of knowing which of
			// the two numbers it read.
			c.Data(http.StatusCreated, contentTypeJSON, verdict.Body)
			return
		case Busy:
			m.InFlight.Inc()
			// 425 and not 409: that code already means version_conflict here,
			// and the k6 bidder counts every 409 in bids_conflict — the most
			// central series of the thesis would end up mixing "somebody got
			// ahead of you" with "you sent the same thing twice". And it
			// answers now rather than waiting: waiting would make the cost of
			// idempotency scale with each engine's latency, putting this
			// middleware inside the comparison through the back door
			// (decisão 33).
			c.AbortWithStatusJSON(http.StatusTooEarly, errorBody{Error: CodeInFlight, Retryable: true})
			return
		}

		// The engine writes what the client sent. The key reaches it through
		// the request context because the bid handler is a mapping table and
		// does not get to learn that idempotency exists.
		c.Request = c.Request.WithContext(WithKey(c.Request.Context(), key))

		captured := &capture{ResponseWriter: c.Writer}
		c.Writer = captured

		// In a defer, so a handler that panics does not leave the key taken for
		// the whole 30s TTL: the gin.Recovery() outside only recovers after
		// this has run.
		defer func() {
			accepted := c.Writer.Status() == http.StatusCreated
			if accepted {
				// One sample per accept, and only there. A bid that runs out of
				// retries is counted by bids_exhausted, and mixing the two
				// would give a number that is neither amplification nor
				// abandonment rate.
				m.Attempts.Observe(float64(verdict.Attempts))
			}
			// The answer is already on the wire, and releasing the mark must
			// not depend on the bidder still being connected: with the
			// request's own context, a client that hung up would leave its key
			// busy for the full TTL.
			if err := store.Finish(context.WithoutCancel(c.Request.Context()), key, accepted, captured.body); err != nil {
				log.Error("idempotency finish failed", "error", err)
			}
		}()

		c.Next()
	}
}

// capture keeps the bytes of a 201 on their way out, and nothing else: a
// rejection is idempotent by nature and is never stored, so buffering it would
// be memory spent to give back what the engine would give back anyway.
type capture struct {
	gin.ResponseWriter
	body []byte
}

func (w *capture) Write(b []byte) (int, error) {
	if w.Status() == http.StatusCreated {
		w.body = append(w.body, b...)
	}
	return w.ResponseWriter.Write(b)
}

func (w *capture) WriteString(s string) (int, error) {
	if w.Status() == http.StatusCreated {
		w.body = append(w.body, s...)
	}
	return w.ResponseWriter.WriteString(s)
}

// The context key is a private type, so nothing outside this package can plant
// a value under it.
type contextKey struct{}

// WithKey carries the validated key from the middleware to whoever fills the
// engine's request. It exists because the bid handler must not gain a line for
// idempotency: if it needed one, the middleware would be in the wrong place.
func WithKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, contextKey{}, key)
}

// KeyFrom returns the key of the request being served, or "" when the bid
// arrived without one — which is the value that keeps the row out of the
// partial unique index.
func KeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(contextKey{}).(string)
	return key
}
