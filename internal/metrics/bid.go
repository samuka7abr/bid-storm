package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/bid"
)

// Buckets from 1ms to 8.192s. The hypothesis puts the optimistic p99 in the
// hundreds of milliseconds under high contention, and a top bucket of 1s would
// truncate precisely the tail the project exists to show.
var confirmBuckets = prometheus.ExponentialBuckets(0.001, 2, 14)

// Instrument wraps any BidEngine in the two series of etapa 1.
//
// The engines do not measure themselves. If each one did, nothing in the
// compiler would stop the shard of etapa 3 from starting its clock at a more
// generous point than the optimistic engine — the silent bias the four method
// invariants exist to prevent, and the first thing a serious reviewer would look
// for. Wrapped, the three are timed at the same boundary because only one
// boundary exists.
//
// The one recorded exception is bid_accept_duration_seconds, the instant of the
// in-memory decision: it does not exist outside the shard, so the shard
// instruments that series from within, in etapa 3. confirm keeps coming from
// here for all three, which is what makes the gap of decisão 8 an honest
// comparison.
func Instrument(next bid.BidEngine, reg prometheus.Registerer, strategy string) bid.BidEngine {
	outcomes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "bid_outcomes_total",
		Help: "Bid attempts by engine decision. There is no bid_conflicts_total: a conflict is outcome=\"conflict\".",
	}, []string{"strategy", "outcome"})

	confirm := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "bid_confirm_duration_seconds",
		Help:    "Time from entering the engine to a durable decision.",
		Buckets: confirmBuckets,
	}, []string{"strategy"})

	reg.MustRegister(outcomes, confirm)

	return &instrumented{
		next:     next,
		strategy: strategy,
		outcomes: outcomes,
		// Bound now, not on the first bid: the process runs one strategy at a
		// time, so the child exists from boot and /metrics never looks like the
		// histogram was forgotten.
		confirm: confirm.WithLabelValues(strategy),
	}
}

type instrumented struct {
	next     bid.BidEngine
	strategy string
	outcomes *prometheus.CounterVec
	confirm  prometheus.Observer
}

func (i *instrumented) PlaceBid(ctx context.Context, req bid.BidRequest) (bid.BidResult, error) {
	started := time.Now()
	res, err := i.next.PlaceBid(ctx, req)

	// A failed call is counted but not timed: the latency of downed
	// infrastructure is not the latency of a bid, and letting it into the
	// histogram would move the p99 of whichever strategy happened to be running
	// when Postgres blinked.
	if err != nil {
		i.outcomes.WithLabelValues(i.strategy, "error").Inc()
		return res, err
	}

	i.confirm.Observe(time.Since(started).Seconds())
	i.outcomes.WithLabelValues(i.strategy, res.Outcome.String()).Inc()
	return res, nil
}
