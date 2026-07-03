package observability

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// RegisterPoolStats exposes pgxpool health as scrape-time gauges. Each gauge is
// a GaugeFunc, so stat() is called on every scrape and always reflects the live
// pool — no background sampler. Pass db.Pool.Stat as stat.
//
// Saturation (acquired approaching max) is the launch-relevant signal: it means
// requests are about to queue on connection acquisition.
func RegisterPoolStats(reg prometheus.Registerer, stat func() *pgxpool.Stat) {
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ggscale_db_pool_total_conns",
			Help: "Total connections in the pool (in use + idle).",
		}, func() float64 { return float64(stat().TotalConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ggscale_db_pool_acquired_conns",
			Help: "Connections currently checked out of the pool.",
		}, func() float64 { return float64(stat().AcquiredConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ggscale_db_pool_idle_conns",
			Help: "Idle connections available in the pool.",
		}, func() float64 { return float64(stat().IdleConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ggscale_db_pool_max_conns",
			Help: "Configured maximum pool size.",
		}, func() float64 { return float64(stat().MaxConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ggscale_db_pool_empty_acquire_total",
			Help: "Cumulative acquires that had to wait for a connection (pool was empty).",
		}, func() float64 { return float64(stat().EmptyAcquireCount()) }),
	)
}
