// Package app is the only place in the code that knows the strategy names.
//
// The contract in internal/bid cannot import the engines: the conformance suite
// imports the contract and every engine imports it too, so whatever knows all
// three would close an import cycle if it lived there. Choosing here makes that
// cycle impossible and keeps internal/bid free of what the single-writer engine
// brings in etapa 3.
package app

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/bid/optimistic"
	"github.com/samuka7abr/bid-storm/internal/metrics"
)

// The three values BID_STRATEGY accepts across the project's lifetime. Two of
// them are not implemented yet, and they are named here anyway so the failure
// says which etapa they arrive in.
const (
	StrategyOptimistic  = "optimistic"
	StrategyPessimistic = "pessimistic"
	StrategyShard       = "shard"
)

// NewEngine returns the engine BID_STRATEGY selects, already wrapped in the
// metrics decorator — the wrapping happens here so that no engine can be built
// unmeasured, and so all three are timed at one boundary.
//
// Unimplemented and unknown strategies fail at boot rather than at the first
// bid: a process that comes up and only then discovers it has no engine turns a
// typo into a benchmark cell full of 503s.
func NewEngine(strategy string, pool *pgxpool.Pool, reg prometheus.Registerer) (bid.BidEngine, error) {
	switch strategy {
	case StrategyOptimistic:
		return metrics.Instrument(optimistic.New(pool), reg, strategy), nil
	case StrategyPessimistic:
		return nil, fmt.Errorf("BID_STRATEGY=%s: the pessimistic engine arrives in etapa 2", strategy)
	case StrategyShard:
		return nil, fmt.Errorf("BID_STRATEGY=%s: the single-writer engine arrives in etapa 3", strategy)
	default:
		return nil, fmt.Errorf("BID_STRATEGY=%q is not a strategy: want %s, %s or %s",
			strategy, StrategyOptimistic, StrategyPessimistic, StrategyShard)
	}
}
