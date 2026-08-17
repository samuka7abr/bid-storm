package bid

// Outcome is what the engine decided, including "I was not asked properly".
type Outcome uint8

const (
	Accepted Outcome = iota // 201
	Conflict                // 409 — stale version (only the optimistic engine produces it)
	TooLow                  // 422 — somebody got ahead of me
	Closed                  // 410 — past ends_at, or status already materialised
	NotFound                // 404
	// Invalid is 400: expectedVersion is required by the optimistic engine and
	// ignored by the other two, but no handler is allowed to branch on the
	// strategy — a switch there would make the benchmark measure the handler as
	// well. So the engine reports "I cannot decide this request" as an outcome,
	// the interface keeps its single method, and error keeps meaning
	// infrastructure failure. Side benefit: outcome="invalid" above zero during a
	// run denounces a misconfigured k6, for free.
	Invalid
)

// String is the value of the outcome label on bid_outcomes_total.
func (o Outcome) String() string {
	switch o {
	case Accepted:
		return "accepted"
	case Conflict:
		return "conflict"
	case TooLow:
		return "too_low"
	case Closed:
		return "closed"
	case NotFound:
		return "not_found"
	case Invalid:
		return "invalid"
	default:
		return "unknown"
	}
}
