package metrics_test

import (
	"context"
	"slices"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/metrics"
)

// A test that only checked the series exists would let a later edit split the
// buckets in silence, and the comparison of decisão 26 would quietly become
// quantile interpolation — which is where a difference of ten percentage points
// hides. So the boundaries are asserted against confirm's, one by one.
func TestLockWaitSharesTheBucketsOfConfirm(t *testing.T) {
	registry := metrics.NewRegistry()
	lockWait := metrics.NewLockWait(registry)

	// The decorator is what publishes bid_confirm_duration_seconds, and the two
	// series living in the same registry is the arrangement under test.
	engine := metrics.Instrument(&fakeEngine{outcome: bid.Accepted}, registry, "pessimistic")
	if _, err := engine.PlaceBid(context.Background(), bid.BidRequest{}); err != nil {
		t.Fatalf("PlaceBid: %v", err)
	}
	lockWait.Observe(0.5)

	families := gather(t, registry)
	wait := families["lock_wait_duration_seconds"]
	if wait == nil {
		t.Fatal("lock_wait_duration_seconds is missing")
	}
	if got := len(wait.GetMetric()); got != 1 {
		t.Fatalf("lock_wait_duration_seconds has %d series, want 1", got)
	}

	// No strategy label: only one engine feeds this series, and a label with a
	// single value would read as the other two reporting zero when they report
	// nothing at all.
	if got := wait.GetMetric()[0].GetLabel(); len(got) != 0 {
		t.Errorf("labels = %v, want none", got)
	}

	h := wait.GetMetric()[0].GetHistogram()
	if h.GetSampleCount() != 1 {
		t.Errorf("sample count = %d, want 1", h.GetSampleCount())
	}
	if h.GetSampleSum() != 0.5 {
		t.Errorf("sample sum = %v, want 0.5", h.GetSampleSum())
	}

	confirm := families["bid_confirm_duration_seconds"]
	if confirm == nil {
		t.Fatal("bid_confirm_duration_seconds is missing")
	}
	got, want := bounds(h), bounds(confirm.GetMetric()[0].GetHistogram())
	if !slices.Equal(got, want) {
		t.Errorf("lock wait buckets = %v, want confirm's %v", got, want)
	}
}

func bounds(h *dto.Histogram) []float64 {
	got := make([]float64, 0, len(h.GetBucket()))
	for _, b := range h.GetBucket() {
		got = append(got, b.GetUpperBound())
	}
	return got
}
