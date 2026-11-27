// Package cacheinvalidation distributes Postgres cache-invalidation events to
// the process-local API-key, rate-limit override, and player-epoch caches.
package cacheinvalidation

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Channel is the single primary-database channel used for cache invalidation.
const Channel = "ggscale_cache_invalidation"

const maxRetryDelay = 5 * time.Second

// Relay is implemented by *db.Pool. Always use the primary pool: replicas do
// not deliver LISTEN/NOTIFY events.
type Relay interface {
	ListenChannelReady(ctx context.Context, channel string, ready func(), fn func(payload string)) error
}

// KeyCache is the subset of tenant.KeyCache used by the listener.
type KeyCache interface {
	Invalidate(tenantID int64)
	InvalidateAll()
}

// OverrideCache is the subset of ratelimit.CachedOverrideStore it manages.
type OverrideCache interface {
	Invalidate(tenantID int64)
	SetHealthy(healthy bool)
}

// EpochCache is the subset of playerauth.CachedEpochValidator it manages.
type EpochCache interface {
	Invalidate(playerID int64)
	SetHealthy(healthy bool)
}

type event struct {
	Kind     string `json:"kind"`
	TenantID int64  `json:"tenant_id,omitempty"`
	PlayerID int64  `json:"player_id,omitempty"`
}

type metrics struct {
	ready         prometheus.Gauge
	notifications *prometheus.CounterVec
	reconnects    prometheus.Counter
}

// Listener owns one dedicated LISTEN connection and the health state used by
// caches. Run must be called on every application host.
type Listener struct {
	relay     Relay
	keys      KeyCache
	overrides OverrideCache
	epochs    EpochCache
	healthy   atomic.Bool
	metrics   metrics
	retryMin  time.Duration
}

// New builds a cache-invalidation listener. It starts unhealthy; the first
// successful LISTEN flushes all caches before marking them healthy.
func New(relay Relay, keys KeyCache, overrides OverrideCache, epochs EpochCache, reg prometheus.Registerer) *Listener {
	return &Listener{
		relay: relay, keys: keys, overrides: overrides, epochs: epochs,
		metrics: newMetrics(reg), retryMin: 100 * time.Millisecond,
	}
}

// Healthy reports whether LISTEN is currently established.
func (l *Listener) Healthy() bool {
	return l.healthy.Load()
}

// Run listens and reconnects with backoff until ctx is cancelled. Caches are
// flushed both when a connection drops and when a new one becomes ready, so a
// notification missed during the gap cannot leave stale entries behind.
func (l *Listener) Run(ctx context.Context) {
	defer l.setDown()
	backoff := l.retryMin
	for {
		l.setDown()
		started := time.Now()
		err := l.relay.ListenChannelReady(ctx, Channel, l.setReady, l.handle)
		l.setDown()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > maxRetryDelay {
			backoff = l.retryMin
		}
		if l.metrics.reconnects != nil {
			l.metrics.reconnects.Inc()
		}
		slog.WarnContext(ctx, "cache invalidation listener disconnected", "err", err, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, maxRetryDelay)
	}
}

func (l *Listener) setReady() {
	// Epoch and override SetHealthy(true) flush their own caches before either
	// accepts cached reads. API keys retain their ordinary TTL while down.
	if l.keys != nil {
		l.keys.InvalidateAll()
	}
	if l.overrides != nil {
		l.overrides.SetHealthy(true)
	}
	if l.epochs != nil {
		l.epochs.SetHealthy(true)
	}
	l.healthy.Store(true)
	if l.metrics.ready != nil {
		l.metrics.ready.Set(1)
	}
}

func (l *Listener) setDown() {
	l.healthy.Store(false)
	if l.metrics.ready != nil {
		l.metrics.ready.Set(0)
	}
	if l.keys != nil {
		l.keys.InvalidateAll()
	}
	if l.overrides != nil {
		l.overrides.SetHealthy(false)
	}
	if l.epochs != nil {
		l.epochs.SetHealthy(false)
	}
}

func (l *Listener) handle(payload string) {
	var e event
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		l.record("unknown", "malformed")
		slog.Warn("cache invalidation: malformed payload", "err", err)
		return
	}
	switch {
	case e.Kind == "api_key" && e.TenantID > 0 && l.keys != nil:
		l.keys.Invalidate(e.TenantID)
	case e.Kind == "override" && e.TenantID > 0 && l.overrides != nil:
		l.overrides.Invalidate(e.TenantID)
	case e.Kind == "session_epoch" && e.PlayerID > 0 && l.epochs != nil:
		l.epochs.Invalidate(e.PlayerID)
	default:
		l.record(e.Kind, "ignored")
		return
	}
	l.record(e.Kind, "applied")
}

func (l *Listener) record(kind, result string) {
	if l.metrics.notifications != nil {
		l.metrics.notifications.WithLabelValues(kind, result).Inc()
	}
}

func newMetrics(reg prometheus.Registerer) metrics {
	if reg == nil {
		return metrics{}
	}
	ready := registerCollector(reg, prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ggscale_cache_invalidation_ready",
		Help: "1 while the primary-DB cache-invalidation LISTEN connection is ready.",
	}))
	notifications := registerCollector(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ggscale_cache_invalidation_notifications_total",
		Help: "Cache invalidation notifications processed by kind and result.",
	}, []string{"kind", "result"}))
	reconnects := registerCollector(reg, prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ggscale_cache_invalidation_reconnects_total",
		Help: "Cache invalidation listener disconnects followed by a reconnect attempt.",
	}))
	return metrics{ready: ready, notifications: notifications, reconnects: reconnects}
}

func registerCollector[T prometheus.Collector](reg prometheus.Registerer, collector T) T {
	if err := reg.Register(collector); err != nil {
		if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return already.ExistingCollector.(T)
		}
		panic(err)
	}
	return collector
}
