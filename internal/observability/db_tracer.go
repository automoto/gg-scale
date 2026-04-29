// Package observability holds in-tree metrics adapters that need to hook
// into third-party packages (pgx tracer, redis hook) — orthogonal to the
// middleware in internal/middleware which lives at the HTTP layer.
package observability

import (
	"context"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
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

// ValkeyHook implements redis.Hook. Counts every Process call as either
// a hit (no error) or miss (error returned by the command).
type ValkeyHook struct {
	counter *prometheus.CounterVec
}

// NewValkeyHook registers the counter on reg.
func NewValkeyHook(reg prometheus.Registerer) *ValkeyHook {
	c := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_valkey_ops_total",
			Help: "Valkey commands executed, by op name and result.",
		},
		[]string{"op", "result"},
	)
	reg.MustRegister(c)
	return &ValkeyHook{counter: c}
}

// DialHook is unused — required to satisfy the redis.Hook interface.
func (h *ValkeyHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

// ProcessHook records single-command results.
func (h *ValkeyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		result := "ok"
		if err != nil && err != redis.Nil {
			result = "error"
		} else if err == redis.Nil {
			result = "miss"
		}
		h.counter.WithLabelValues(cmd.Name(), result).Inc()
		return err
	}
}

// ProcessPipelineHook records each command in a pipeline as a separate op.
func (h *ValkeyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			result := "ok"
			if e := cmd.Err(); e != nil && e != redis.Nil {
				result = "error"
			} else if e == redis.Nil {
				result = "miss"
			}
			h.counter.WithLabelValues(cmd.Name(), result).Inc()
		}
		return err
	}
}
