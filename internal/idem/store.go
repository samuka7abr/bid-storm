package idem

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// The entry lives at idem:<uuid> and is a hash with three fields: attempts,
// busy and done. No user scope in the key: X-User-Id is not authenticated, so
// scoping by an identity the client picks isolates nothing against whoever is
// hostile, and against whoever is honest the UUID already did (decisão 32).
const keyPrefix = "idem:"

// The two TTLs answer different questions.
//
// inFlight is the ceiling on the damage of a process that dies between the
// claim and the finish: the key stays taken for 30s instead of forever. It is
// well above BID_DEADLINE (2s), so no live request is ever hit by it.
//
// terminal is longer than the longest scenario in the project (ramp, 2min), so
// inside a cell no terminal entry expires and no duplicate escapes by expiry.
const (
	inFlightTTL = 30 * time.Second
	terminalTTL = 5 * time.Minute
)

// Kind is what the claim found.
type Kind string

const (
	// Pass: first attempt, or a re-aimed retry. It goes to the engine.
	Pass Kind = "go"
	// Replay: this key already produced a 201, and it is stored.
	Replay Kind = "replay"
	// Busy: another request under this key is executing right now.
	Busy Kind = "busy"
)

// Verdict is the answer to one claim. Attempts is filled on Pass and Body on
// Replay; neither is filled on Busy, because a duplicate turned away in flight
// consulted no engine and is not an attempt at anything.
type Verdict struct {
	Kind     Kind
	Attempts int64
	Body     []byte
}

// claim does four indivisible things: look for a stored response, look for a
// request in flight, count the attempt and take the mark.
//
// Composing plain commands — SET NX, GET when the NX fails, INCR — was the
// alternative. It costs two to three round-trips and puts the INCR on the wrong
// side of the decision: it would also count the duplicates that were turned
// away, which never reached the engine and are therefore attempts at nothing.
// Counting the attempt in the same atomic step that grants passage is what makes
// bid_attempts_per_accept mean "requests that reached the engine under this key"
// (decisão 35).
var claim = redis.NewScript(`
local done = redis.call('HGET', KEYS[1], 'done')
if done then return {'replay', done} end
if redis.call('HGET', KEYS[1], 'busy') then return {'busy'} end
local n = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
redis.call('HSET', KEYS[1], 'busy', '1')
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return {'go', n}`)

// Store is the entry in Redis, and the only place in the process that speaks
// Redis at all. Two methods, five commands, none of them KEYS, SCAN or FLUSHDB.
type Store struct {
	client *redis.Client
}

// NewStore returns the store backed by client.
func NewStore(client *redis.Client) *Store {
	return &Store{client: client}
}

// Claim is the entry: one EVALSHA, one round-trip, atomic by construction —
// Redis runs the whole script without interleaving anybody's command.
func (s *Store) Claim(ctx context.Context, key string) (Verdict, error) {
	raw, err := claim.Run(ctx, s.client, []string{keyPrefix + key}, inFlightTTL.Milliseconds()).Slice()
	if err != nil {
		return Verdict{}, fmt.Errorf("idempotency claim: %w", err)
	}
	if len(raw) == 0 {
		return Verdict{}, fmt.Errorf("idempotency claim: empty reply")
	}

	kind, _ := raw[0].(string)
	switch Kind(kind) {
	case Replay:
		body, ok := second[string](raw)
		if !ok {
			return Verdict{}, fmt.Errorf("idempotency claim: replay without a body")
		}
		return Verdict{Kind: Replay, Body: []byte(body)}, nil
	case Busy:
		return Verdict{Kind: Busy}, nil
	case Pass:
		attempts, ok := second[int64](raw)
		if !ok {
			return Verdict{}, fmt.Errorf("idempotency claim: go without an attempt count")
		}
		return Verdict{Kind: Pass, Attempts: attempts}, nil
	default:
		return Verdict{}, fmt.Errorf("idempotency claim: unknown verdict %q", kind)
	}
}

// Finish is the exit: one pipeline, one round-trip.
//
// Only the 201 is stored, because rejection is idempotent by nature — a 422
// resent stays 422, since minNextBid only grows, and what idempotency exists to
// prevent, the second row in bids, can only be born of an accept (decisão 34).
//
// What a rejection does have to do is release busy. Without it the re-aimed
// retry arriving 40ms later would find the key taken and get a 425, leaving the
// bidder stuck against the middleware. attempts survives, and it is what makes
// the next attempt count as n+1.
func (s *Store) Finish(ctx context.Context, key string, accepted bool, body []byte) error {
	k := keyPrefix + key
	_, err := s.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		if accepted {
			p.HSet(ctx, k, "done", body)
			p.HDel(ctx, k, "busy")
			p.PExpire(ctx, k, terminalTTL)
			return nil
		}
		p.HDel(ctx, k, "busy")
		return nil
	})
	if err != nil {
		return fmt.Errorf("idempotency finish: %w", err)
	}
	return nil
}

// second reads the payload the script returned next to the verdict. Lua gives
// back a bulk string for the body and an integer for the count, and a reply
// shaped differently from what the script above returns is a bug worth an error
// rather than a zero value.
func second[T any](raw []any) (T, bool) {
	var zero T
	if len(raw) < 2 {
		return zero, false
	}
	v, ok := raw[1].(T)
	return v, ok
}
