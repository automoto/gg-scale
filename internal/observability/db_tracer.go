// Package observability holds in-tree metrics adapters that need to hook
// into third-party packages (pgx tracer) — orthogonal to the middleware in
// internal/middleware which lives at the HTTP layer.
package observability

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

type pgxTracerKey struct{}

// PgxTracer implements pgx.QueryTracer. Mount via pgxpool.Config.ConnConfig.Tracer.
type PgxTracer struct {
	hist *prometheus.HistogramVec
}

// NewPgxTracer registers the histogram on reg.
func NewPgxTracer(reg prometheus.Registerer) *PgxTracer {
	hist := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ggscale_db_query_duration_seconds",
			Help:    "Postgres query latency.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	reg.MustRegister(hist)
	return &PgxTracer{hist: hist}
}

// TraceQueryStart records the start time on the context.
func (t *PgxTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, pgxTracerKey{}, time.Now())
}

// TraceQueryEnd records the elapsed time and bucketed result.
func (t *PgxTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	start, ok := ctx.Value(pgxTracerKey{}).(time.Time)
	if !ok {
		return
	}
	result := "ok"
	if data.Err != nil {
		result = "error"
	}
	t.hist.WithLabelValues(result).Observe(time.Since(start).Seconds())
}
