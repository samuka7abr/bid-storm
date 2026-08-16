package metrics_test

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/samuka7abr/auction-system/internal/metrics"
	"github.com/samuka7abr/auction-system/internal/testsupport"
)

// Without these five series the cost of the pessimistic engine is
// indistinguishable from an undersized pool, so their absence would not be a
// missing metric — it would be a missing conclusion.
func TestPoolCollectorExposesEverySeries(t *testing.T) {
	ctx := context.Background()
	pg := testsupport.Start(t)

	registry := metrics.NewRegistry()
	metrics.RegisterPool(registry, pg.Pool)

	// Force one acquisition, so the counters have something to report.
	if err := pg.Pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]*dto.MetricFamily{}
	for _, f := range families {
		got[f.GetName()] = f
	}

	for _, name := range []string{
		"db_pool_conns",
		"db_pool_acquire_total",
		"db_pool_acquire_duration_seconds_total",
		"db_pool_empty_acquire_total",
		"db_pool_canceled_acquire_total",
	} {
		if got[name] == nil {
			t.Errorf("series %s is missing", name)
		}
	}

	states := map[string]float64{}
	for _, m := range got["db_pool_conns"].GetMetric() {
		for _, label := range m.GetLabel() {
			if label.GetName() == "state" {
				states[label.GetValue()] = m.GetGauge().GetValue()
			}
		}
	}
	for _, state := range []string{"acquired", "idle", "max"} {
		if _, ok := states[state]; !ok {
			t.Errorf("db_pool_conns is missing state=%q", state)
		}
	}
	if want := float64(pg.Pool.Config().MaxConns); states["max"] != want {
		t.Errorf("db_pool_conns{state=\"max\"} = %v, want %v", states["max"], want)
	}

	if count := got["db_pool_acquire_total"].GetMetric()[0].GetCounter().GetValue(); count < 1 {
		t.Errorf("db_pool_acquire_total = %v after a ping, want at least 1", count)
	}

	// The bid metrics arrive with the engine that feeds them, in spec 02.
	for name := range got {
		if len(name) > 4 && name[:4] == "bid_" {
			t.Errorf("unexpected metric %s: bid metrics belong to spec 02", name)
		}
	}
}
