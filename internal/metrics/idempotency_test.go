package metrics_test

import (
	"slices"
	"testing"

	"github.com/samuka7abr/bid-storm/internal/metrics"
)

// Both children are bound at boot, like confirm's in etapa 1: the process runs
// one strategy at a time, so /metrics must never look like a series was
// forgotten when what happened was an absence of duplicates. Zero is a claim,
// silence is not.
func TestIdempotencyPublishesBothKindsBeforeAnyDuplicate(t *testing.T) {
	registry := metrics.NewRegistry()
	metrics.NewIdempotency(registry, "pessimistic")

	hits := gather(t, registry)["idempotency_hits_total"]
	if hits == nil {
		t.Fatal("idempotency_hits_total is missing")
	}
	if got := len(hits.GetMetric()); got != 2 {
		t.Fatalf("idempotency_hits_total has %d series, want 2", got)
	}

	kinds := map[string]float64{}
	for _, m := range hits.GetMetric() {
		got := labels(m)
		// The label exists for the same mechanical reason as on
		// bid_outcomes_total: without it the dashboard of etapa 5 cannot
		// overlay three curves that came from 36 separate runs (decisão 37).
		if got["strategy"] != "pessimistic" {
			t.Errorf("strategy = %q, want pessimistic", got["strategy"])
		}
		kinds[got["kind"]] = m.GetCounter().GetValue()
	}
	for _, kind := range []string{"replayed", "in_flight"} {
		value, published := kinds[kind]
		if !published {
			t.Errorf("kind=%q is missing: it has to read zero, not be absent", kind)
		}
		if value != 0 {
			t.Errorf("kind=%q = %v at boot, want 0", kind, value)
		}
	}
}

// MAX_RETRIES is 10 (decisão 18), so nothing can land above the last bucket,
// and +Inf becomes the free detector of a client that violated its own limit.
func TestAttemptsPerAcceptCountsOneToTen(t *testing.T) {
	registry := metrics.NewRegistry()
	m := metrics.NewIdempotency(registry, "optimistic")

	attempts := gather(t, registry)["bid_attempts_per_accept"]
	if attempts == nil {
		t.Fatal("bid_attempts_per_accept is missing")
	}
	if got := len(attempts.GetMetric()); got != 1 {
		t.Fatalf("bid_attempts_per_accept has %d series, want 1", got)
	}
	if got := labels(attempts.GetMetric()[0]); got["strategy"] != "optimistic" {
		t.Errorf("labels = %v, want strategy=optimistic", got)
	}
	if got := attempts.GetMetric()[0].GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("sample count = %d at boot, want 0", got)
	}

	want := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := bounds(attempts.GetMetric()[0].GetHistogram()); !slices.Equal(got, want) {
		t.Errorf("buckets = %v, want %v", got, want)
	}

	// One accept that took three attempts, which is the whole reading of this
	// series: requests that reached the engine under one key until one passed.
	m.Attempts.Observe(3)
	m.Replayed.Inc()
	m.InFlight.Inc()

	families := gather(t, registry)
	h := families["bid_attempts_per_accept"].GetMetric()[0].GetHistogram()
	if h.GetSampleCount() != 1 || h.GetSampleSum() != 3 {
		t.Errorf("count/sum = %d/%v, want 1/3", h.GetSampleCount(), h.GetSampleSum())
	}
	for _, series := range families["idempotency_hits_total"].GetMetric() {
		if got := series.GetCounter().GetValue(); got != 1 {
			t.Errorf("kind=%q = %v, want 1", labels(series)["kind"], got)
		}
	}
}
