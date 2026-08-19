package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/idem"
)

// Buckets 1 to 10, linear. MAX_RETRIES is 10 (decisão 18), so nothing can land
// above the last one, and +Inf becomes, for free, the detector of a client that
// violated its own limit.
var attemptBuckets = prometheus.LinearBuckets(1, 1, 10)

// NewIdempotency registers the two series of the middleware and returns the
// observers it feeds.
//
// Both carry strategy, and that does not break decisão 23: the middleware is
// above the strategy switch, one component identical for the three engines that
// cannot know which one is behind it. The label is there for the same mechanical
// reason as bid_outcomes_total{strategy} — without it the dashboard of etapa 5
// cannot overlay three curves that came from 36 separate runs (decisão 37).
//
// kind rather than two series: replayed and in_flight are duplicates turned away
// by different paths, and whoever wanted the total would have to add two series
// and would forget one. Same move by which bid_conflicts_total does not exist.
func NewIdempotency(reg prometheus.Registerer, strategy string) idem.Metrics {
	hits := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "idempotency_hits_total",
		Help: "Duplicate bids the middleware turned away, by the path that turned them away.",
	}, []string{"strategy", "kind"})

	attempts := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "bid_attempts_per_accept",
		// The server's twin of the k6 series, which is renamed to
		// client_attempts_per_accept rather than deleted: the client measures
		// how many attempts it made, this one how many arrived, and the gap
		// between the two curves is the bidder that gave up on BID_DEADLINE
		// while the server still held its request (decisão 38).
		Help:    "Requests that reached the engine under one idempotency key until one was accepted.",
		Buckets: attemptBuckets,
	}, []string{"strategy"})

	reg.MustRegister(hits, attempts)

	// Bound now, not on the first duplicate: the process runs one strategy at a
	// time, so /metrics never looks like the series was forgotten when what
	// happened was an absence of duplicates — and zero is a different claim from
	// silence.
	return idem.Metrics{
		Replayed: hits.WithLabelValues(strategy, "replayed"),
		InFlight: hits.WithLabelValues(strategy, "in_flight"),
		Attempts: attempts.WithLabelValues(strategy),
	}
}
