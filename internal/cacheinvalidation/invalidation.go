// Package cacheinvalidation propagates process-local cache evictions through
// Postgres LISTEN/NOTIFY. Notifications are a fast path; each cache also keeps
// its TTL and the listener runs a slow full-eviction fallback.
package cacheinvalidation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	// Channel is the shared Postgres notification channel. It carries no
	// credentials or user data, only cache kinds and numeric IDs.
	Channel = "ggscale_cache_invalidation"
	// FallbackInterval bounds how long a notification lost during a reconnect
	// can leave an otherwise unexpired entry in a local cache.
	FallbackInterval = time.Minute
)

const (
	// KindAPIKey evicts an API-key tenant entry.
	KindAPIKey = "api_key"
	// KindRateLimitOverride evicts an override tenant entry.
	KindRateLimitOverride = "rate_limit_override"
	// KindPlayerEpoch evicts one player's epoch entry.
	KindPlayerEpoch = "player_epoch"
	// KindTenantEpochs evicts all epoch entries for a tenant.
	KindTenantEpochs = "tenant_epochs"
)

// Event identifies one cache invalidation. TenantID is used by API-key,
// override, and tenant-wide epoch events; PlayerID by a single-epoch event.
type Event struct {
	Kind     string `json:"kind"`
	TenantID int64  `json:"tenant_id,omitempty"`
	PlayerID int64  `json:"player_id,omitempty"`
}

// Notifier is implemented by db.Pool.
type Notifier interface {
	Notify(ctx context.Context, channel, payload string) error
}

// Listener is implemented by db.Pool.
type Listener interface {
	ListenChannel(ctx context.Context, channel string, fn func(payload string)) error
}

// Notify publishes an event after the associated database write has committed.
func Notify(ctx context.Context, notifier Notifier, event Event) error {
	if notifier == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("cache invalidation: encode event: %w", err)
	}
	if err := notifier.Notify(ctx, Channel, string(payload)); err != nil {
		return fmt.Errorf("cache invalidation: %s: %w", event.Kind, err)
	}
	return nil
}

// Handler routes decoded events to local-only eviction methods. Nil callbacks
// ignore that event kind. Unknown kinds are ignored for rolling upgrades.
type Handler struct {
	APIKeys      func(tenantID int64)
	Overrides    func(tenantID int64)
	PlayerEpochs func(playerID int64)
	TenantEpochs func(tenantID int64)
}

// Handle decodes and dispatches one notification payload.
func (h Handler) Handle(payload string) error {
	var event Event
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return fmt.Errorf("cache invalidation: decode event: %w", err)
	}
	switch event.Kind {
	case KindAPIKey:
		return dispatchTenantEvent(event, h.APIKeys)
	case KindRateLimitOverride:
		return dispatchTenantEvent(event, h.Overrides)
	case KindPlayerEpoch:
		if event.PlayerID <= 0 {
			return fmt.Errorf("cache invalidation: %s requires a positive player_id", event.Kind)
		}
		if h.PlayerEpochs != nil {
			h.PlayerEpochs(event.PlayerID)
		}
	case KindTenantEpochs:
		return dispatchTenantEvent(event, h.TenantEpochs)
	}
	return nil
}

func dispatchTenantEvent(event Event, fn func(int64)) error {
	if event.TenantID <= 0 {
		return fmt.Errorf("cache invalidation: %s requires a positive tenant_id", event.Kind)
	}
	if fn != nil {
		fn(event.TenantID)
	}
	return nil
}

// Run listens until ctx ends, reconnecting with bounded backoff. fallback runs
// once a minute to evict everything after a missed notification. The listener
// connection belongs to the primary pool because replicas do not deliver
// NOTIFY.
func Run(ctx context.Context, listener Listener, handler Handler, fallback func()) {
	run(ctx, listener, handler, fallback, FallbackInterval, time.Second)
}

func run(ctx context.Context, listener Listener, handler Handler, fallback func(), fallbackInterval, initialBackoff time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	if fallback != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runFallback(ctx, fallback, fallbackInterval)
		}()
	}

	backoff := initialBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := listener.ListenChannel(ctx, Channel, func(payload string) {
			if err := handler.Handle(payload); err != nil {
				slog.WarnContext(ctx, "cache invalidation notification rejected", "error", err)
			}
		})
		if ctx.Err() != nil {
			break
		}
		if time.Since(started) > 30*time.Second {
			backoff = initialBackoff
		}
		slog.WarnContext(ctx, "cache invalidation listener disconnected", "error", err, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	cancel()
	wg.Wait()
}

func runFallback(ctx context.Context, fallback func(), interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			fallback()
		case <-ctx.Done():
			return
		}
	}
}
