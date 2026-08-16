package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// poolCollector reports pgxpool.Stat at scrape time.
//
// pgxpool keeps cumulative counters and no per-acquisition duration, so there
// is nothing to sample between scrapes and no collection goroutine to run. A
// histogram would mean wrapping every acquisition in application code, which
// adds cost to the hot path in order to measure the hot path.
//
// What these five series buy: db_pool_empty_acquire_total grows if and only if
// somebody waited for a connection, which is what separates "the pessimistic
// engine waits on locks" from "the pool was undersized". Without them the two
// are indistinguishable in the results.
type poolCollector struct {
	pool *pgxpool.Pool

	conns           *prometheus.Desc
	acquire         *prometheus.Desc
	acquireDuration *prometheus.Desc
	emptyAcquire    *prometheus.Desc
	canceledAcquire *prometheus.Desc
}

// RegisterPool wires the statistics of pool into r.
func RegisterPool(r prometheus.Registerer, pool *pgxpool.Pool) {
	r.MustRegister(&poolCollector{
		pool: pool,
		conns: prometheus.NewDesc(
			"db_pool_conns",
			"Connections in the pgx pool, by state.",
			[]string{"state"}, nil),
		acquire: prometheus.NewDesc(
			"db_pool_acquire_total",
			"Connection acquisitions completed since boot.",
			nil, nil),
		acquireDuration: prometheus.NewDesc(
			"db_pool_acquire_duration_seconds_total",
			"Time spent acquiring connections since boot. Divided by db_pool_acquire_total, the mean wait per acquisition.",
			nil, nil),
		emptyAcquire: prometheus.NewDesc(
			"db_pool_empty_acquire_total",
			"Acquisitions that had to wait because the pool was empty.",
			nil, nil),
		canceledAcquire: prometheus.NewDesc(
			"db_pool_canceled_acquire_total",
			"Acquisitions abandoned because their context was canceled.",
			nil, nil),
	})
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.conns
	ch <- c.acquire
	ch <- c.acquireDuration
	ch <- c.emptyAcquire
	ch <- c.canceledAcquire
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()

	conns := func(state string, v float64) {
		ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, v, state)
	}
	conns("acquired", float64(s.AcquiredConns()))
	conns("idle", float64(s.IdleConns()))
	conns("max", float64(s.MaxConns()))

	total := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	total(c.acquire, float64(s.AcquireCount()))
	total(c.acquireDuration, s.AcquireDuration().Seconds())
	total(c.emptyAcquire, float64(s.EmptyAcquireCount()))
	total(c.canceledAcquire, float64(s.CanceledAcquireCount()))
}
