// Package metrics owns the Prometheus registry of the process.
//
// Only metrics with something to measure in this spec live here: the pool and
// the Go runtime. The bid metrics arrive with the engine that feeds them, in
// spec 02 — a dashboard of empty panels gives no way to tell "empty by design"
// from "empty because of a bug".
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry returns a registry owned by this process — never the default one,
// so a stray Register inside a dependency cannot add a series the benchmark
// never asked for.
func NewRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return r
}

// Handler serves r over HTTP.
func Handler(r *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(r, promhttp.HandlerOpts{Registry: r})
}
