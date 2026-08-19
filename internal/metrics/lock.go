package metrics

import "github.com/prometheus/client_golang/prometheus"

// NewLockWait registers lock_wait_duration_seconds and returns the observer the
// pessimistic engine feeds.
//
// It is here rather than inside that engine so every series name this process
// publishes stays in one package — this is where a reviewer looks to find out
// what the process exposes. The engine receives a prometheus.Observer and never
// learns the name behind it (decisão 28).
//
// The buckets are confirmBuckets, literally the ones of
// bid_confirm_duration_seconds. The two series only answer the question that
// matters if they can be read against each other bucket by bucket: if lock wait
// dominates confirmation, the four round-trips of decisão 25 are noise inside
// the number, and the objection "you crippled the pessimistic engine with
// round-trips" dies on the graph instead of in prose. Separate buckets would
// push the comparison through quantile interpolation, which is exactly where a
// difference of ten percentage points hides (decisão 26).
//
// No strategy label: only one engine produces this series. A label with a single
// value would suggest the other two report zero, when they report nothing at all
// — and zero is a different claim from silence.
func NewLockWait(reg prometheus.Registerer) prometheus.Observer {
	lockWait := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "lock_wait_duration_seconds",
		// The whole statement, not the pure wait: extracting the wait alone would
		// mean sampling pg_locks, spending time on the hot path to measure the
		// hot path. On the 1000-auction cell, where nobody disputes the same row,
		// the floor of this histogram is the round-trip cost — the matrix
		// calibrates itself.
		Help:    "Time the pessimistic engine spends in SELECT ... FOR UPDATE: round-trip, planning and wait.",
		Buckets: confirmBuckets,
	})
	reg.MustRegister(lockWait)
	return lockWait
}
