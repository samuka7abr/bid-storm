package metrics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/samuka7abr/bid-storm/internal/bid"
	"github.com/samuka7abr/bid-storm/internal/metrics"
)

// fakeEngine answers whatever the case needs, so the decorator is exercised
// without a database — what is under test is the boundary, not an engine.
type fakeEngine struct {
	outcome bid.Outcome
	err     error
}

func (f *fakeEngine) PlaceBid(context.Context, bid.BidRequest) (bid.BidResult, error) {
	return bid.BidResult{Outcome: f.outcome}, f.err
}

func TestInstrumentObservesDurationAndOutcome(t *testing.T) {
	registry := metrics.NewRegistry()
	fake := &fakeEngine{outcome: bid.TooLow}
	engine := metrics.Instrument(fake, registry, "optimistic")

	for range 3 {
		if _, err := engine.PlaceBid(context.Background(), bid.BidRequest{}); err != nil {
			t.Fatalf("PlaceBid: %v", err)
		}
	}

	families := gather(t, registry)

	counter := families["bid_outcomes_total"]
	if counter == nil {
		t.Fatal("bid_outcomes_total is missing")
	}
	if got := len(counter.GetMetric()); got != 1 {
		t.Fatalf("bid_outcomes_total has %d series, want 1", got)
	}
	if got := labels(counter.GetMetric()[0]); got["strategy"] != "optimistic" || got["outcome"] != "too_low" {
		t.Errorf("labels = %v, want strategy=optimistic outcome=too_low", got)
	}
	if got := counter.GetMetric()[0].GetCounter().GetValue(); got != 3 {
		t.Errorf("bid_outcomes_total = %v, want 3", got)
	}

	histogram := families["bid_confirm_duration_seconds"]
	if histogram == nil {
		t.Fatal("bid_confirm_duration_seconds is missing")
	}
	h := histogram.GetMetric()[0].GetHistogram()
	if h.GetSampleCount() != 3 {
		t.Errorf("histogram count = %d, want 3", h.GetSampleCount())
	}
	// The tail of the optimistic engine under contention is the point of the
	// project, and a top bucket of 1s would cut it off.
	if top := h.GetBucket()[len(h.GetBucket())-1].GetUpperBound(); top < 8 {
		t.Errorf("top bucket = %v seconds, want at least 8", top)
	}
}

// One series per decision, all from the same boundary. bid_conflicts_total does
// not exist: a conflict is a label.
func TestInstrumentLabelsEveryOutcome(t *testing.T) {
	registry := metrics.NewRegistry()
	fake := &fakeEngine{}
	engine := metrics.Instrument(fake, registry, "optimistic")

	want := map[string]bool{}
	for _, outcome := range []bid.Outcome{bid.Accepted, bid.Conflict, bid.TooLow, bid.Closed, bid.NotFound, bid.Invalid} {
		fake.outcome = outcome
		if _, err := engine.PlaceBid(context.Background(), bid.BidRequest{}); err != nil {
			t.Fatalf("PlaceBid: %v", err)
		}
		want[outcome.String()] = true
	}

	got := map[string]bool{}
	for _, m := range gather(t, registry)["bid_outcomes_total"].GetMetric() {
		got[labels(m)["outcome"]] = true
	}
	for outcome := range want {
		if !got[outcome] {
			t.Errorf("no series for outcome=%q", outcome)
		}
	}
}

// Infrastructure that fell over is not bid latency: it is counted, never timed.
func TestInstrumentKeepsErrorsOutOfTheHistogram(t *testing.T) {
	registry := metrics.NewRegistry()
	engine := metrics.Instrument(&fakeEngine{err: errors.New("connection refused")}, registry, "optimistic")

	if _, err := engine.PlaceBid(context.Background(), bid.BidRequest{}); err == nil {
		t.Fatal("PlaceBid returned nil, want the engine's error")
	}

	families := gather(t, registry)
	if got := labels(families["bid_outcomes_total"].GetMetric()[0])["outcome"]; got != "error" {
		t.Errorf("outcome label = %q, want %q", got, "error")
	}
	if got := families["bid_confirm_duration_seconds"].GetMetric()[0].GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("histogram count = %d, want 0", got)
	}
}

func gather(t *testing.T, registry *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]*dto.MetricFamily{}
	for _, f := range families {
		got[f.GetName()] = f
	}
	return got
}

func labels(m *dto.Metric) map[string]string {
	got := map[string]string{}
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	return got
}
