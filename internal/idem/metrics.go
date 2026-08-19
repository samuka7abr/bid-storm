package idem

import "github.com/prometheus/client_golang/prometheus"

// Metrics is what the middleware feeds, and it is declared here rather than in
// internal/metrics so the dependency points one way only: internal/metrics
// imports this package to build the struct, exactly as it imports internal/bid
// to wrap an engine. The middleware receives observers that are already bound
// and never learns a series name — the same arrangement the pessimistic engine
// has with prometheus.Observer (decisão 28).
//
// The two counters arrive as children rather than as a CounterVec because the
// middleware must not be able to invent a third kind: replayed and in_flight
// are the only two ways a duplicate gets turned away, and a typo in a label
// value would publish a series nobody would notice was wrong.
type Metrics struct {
	// Replayed counts duplicates answered from the stored 201.
	Replayed prometheus.Counter
	// InFlight counts duplicates that arrived while the original was running.
	InFlight prometheus.Counter
	// Attempts takes one sample per accept: how many requests reached the
	// engine under that key. A bid that runs out of retries produces no sample.
	Attempts prometheus.Observer
}
