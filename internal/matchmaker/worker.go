package matchmaker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/realtime"
)

// Allocator is the slice of fleet.Manager the worker uses. The narrow
// interface keeps the worker test-injectable without dragging the whole
// Manager into unit tests.
type Allocator interface {
	Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error)
}

// Notifier pushes MatchReady envelopes back to matched players. *realtime.Hub
// is the production implementation.
type Notifier interface {
	Send(ctx context.Context, tenantID, endUserID int64, msg realtime.Message) error
}

// WorkerConfig controls bucket sizing and scan cadence.
type WorkerConfig struct {
	// BucketSize is how many tickets must accumulate in a (tenant, project,
	// region, game_mode) bucket before the worker allocates a server. 1 is
	// the practical default for single-player and solo-host modes.
	BucketSize int
	// Interval is the fallback scan cadence. The worker normally wakes
	// event-driven via Queue's Listener (Postgres LISTEN/NOTIFY); this
	// ticker catches anything missed during a listener reconnect or when
	// the Queue doesn't implement Listener at all. Zero defaults to 5s.
	Interval time.Duration
	Logger   *slog.Logger
}

// Worker consumes the queue, allocates servers, and notifies matched players.
type Worker struct {
	queue Queue
	alloc Allocator
	hub   Notifier
	cfg   WorkerConfig
	log   *slog.Logger
}

// NewWorker constructs a Worker. The queue + allocator are required;
// notifier may be nil to suppress WS pushes (useful in tests).
func NewWorker(q Queue, alloc Allocator, hub Notifier, cfg WorkerConfig) *Worker {
	if cfg.BucketSize <= 0 {
		cfg.BucketSize = 1
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{queue: q, alloc: alloc, hub: hub, cfg: cfg, log: cfg.Logger}
}

// Run drives the worker until ctx is cancelled. Three event sources
// converge in the select:
//
//  1. Listener-delivered Bucket events (Postgres LISTEN/NOTIFY) — the hot
//     path; wakes within milliseconds of a ticket being enqueued.
//  2. Fallback ticker — catches tickets queued during a listener reconnect
//     gap or when the Queue doesn't implement Listener at all.
//  3. ctx.Done — graceful shutdown.
//
// An initial drain runs before the loop so tickets queued before this
// process started don't have to wait for the first tick.
func (w *Worker) Run(ctx context.Context) {
	if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.log.Warn("matchmaker: initial drain failed", "err", err)
	}

	events := make(chan Bucket, 64)
	if l, ok := w.queue.(Listener); ok {
		go w.runListener(ctx, l, events)
	}

	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-events:
			if err := w.processBucket(ctx, b); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Warn("matchmaker: bucket failed (notify)",
					"tenant_id", b.TenantID, "project_id", b.ProjectID,
					"region", b.Region, "game_mode", b.GameMode, "err", err)
			}
		case <-t.C:
			if err := w.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Warn("matchmaker: fallback tick failed", "err", err)
			}
		}
	}
}

// runListener loops on Listener.Listen and forwards bucket events to out.
// On error it backs off briefly and reconnects; the fallback ticker covers
// the gap. Returns when ctx is cancelled.
func (w *Worker) runListener(ctx context.Context, l Listener, out chan<- Bucket) {
	for {
		err := l.Listen(ctx, func(b Bucket) {
			// Non-blocking send. If the buffer is full the worker is
			// already saturated; dropping the event is safe because the
			// fallback ticker will pick up the queued tickets on its
			// next sweep. Blocking here would stop pumping the NOTIFY
			// reader and Postgres would terminate the connection once
			// its 8KB per-backend NOTIFY queue overflows.
			select {
			case out <- b:
			case <-ctx.Done():
			default:
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.log.Warn("matchmaker: listener disconnected", "err", err)
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// Tick performs one scan + process pass. Exposed for tests.
func (w *Worker) Tick(ctx context.Context) error { return w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) error {
	buckets, err := w.queue.ListReadyBuckets(ctx, w.cfg.BucketSize)
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if err := w.processBucket(ctx, b); err != nil {
			w.log.Warn("matchmaker: bucket failed",
				"tenant_id", b.TenantID, "project_id", b.ProjectID,
				"region", b.Region, "game_mode", b.GameMode, "err", err)
		}
	}
	return nil
}

func (w *Worker) processBucket(ctx context.Context, b Bucket) error {
	tickets, err := w.queue.PopBucket(ctx, b, w.cfg.BucketSize)
	if err != nil {
		return fmt.Errorf("pop bucket: %w", err)
	}
	if len(tickets) == 0 {
		return nil
	}

	ids := make([]int64, len(tickets))
	for i, t := range tickets {
		ids[i] = t.ID
	}

	tenantCtx := db.WithTenant(ctx, b.TenantID)
	tenantCtx = db.WithProject(tenantCtx, b.ProjectID)
	alloc, err := w.alloc.Allocate(tenantCtx, fleet.AllocationRequest{
		TenantID:  b.TenantID,
		ProjectID: b.ProjectID,
		Region:    b.Region,
		GameMode:  b.GameMode,
		Capacity:  len(tickets),
	})
	if err != nil {
		if markErr := w.queue.MarkFailed(ctx, ids); markErr != nil {
			return fmt.Errorf("allocate failed (%w) and mark failed (%v)", err, markErr)
		}
		return fmt.Errorf("allocate: %w", err)
	}

	if err := w.queue.MarkMatched(ctx, ids, alloc.Address); err != nil {
		return fmt.Errorf("mark matched: %w", err)
	}

	if w.hub == nil {
		return nil
	}
	for _, t := range tickets {
		p, _ := json.Marshal(map[string]any{"address": alloc.Address, "ticket_id": t.ID})
		if sendErr := w.hub.Send(ctx, t.TenantID, t.EndUserID, realtime.Message{
			Type:    "match_ready",
			Payload: p,
		}); sendErr != nil && !errors.Is(sendErr, realtime.ErrNotConnected) {
			w.log.Warn("matchmaker: notify failed", "tenant_id", t.TenantID, "end_user_id", t.EndUserID, "err", sendErr)
		}
	}
	return nil
}
