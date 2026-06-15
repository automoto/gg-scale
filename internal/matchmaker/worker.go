package matchmaker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/realtime"
)

// Allocator is the slice of fleet.Manager the worker uses. The narrow
// interface keeps the worker test-injectable without dragging the whole
// Manager into unit tests. Deallocate is invoked to clean up the orphan
// when CommitClaim loses a race to Cancel or the sweeper.
type Allocator interface {
	Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error)
	Deallocate(ctx context.Context, id fleet.AllocationID) error
}

// Notifier pushes MatchReady envelopes back to matched players. *realtime.Hub
// is the production implementation.
type Notifier interface {
	Send(ctx context.Context, tenantID, endUserID int64, msg realtime.Message) error
}

// Counter is the minimal Prometheus-ish interface the worker uses for the
// dropped-bucket-event metric. Avoids a hard dependency on
// prometheus/client_golang from this package; nil is valid.
type Counter interface {
	Inc()
}

// WorkerConfig controls bucket sizing, scan cadence, and the claim lifecycle.
type WorkerConfig struct {
	// BucketSize is how many tickets must accumulate in a bucket before the
	// worker allocates a server. 1 is the practical default for solo modes.
	BucketSize int
	// Interval is the fallback scan cadence. Defaults to 5s. The worker
	// normally wakes event-driven via Queue's Listener; the ticker catches
	// anything missed during a listener reconnect or when the Queue doesn't
	// implement Listener at all.
	Interval time.Duration
	// WorkerCount is the size of the bucket-processing fan-out pool. >1
	// lets Allocate calls run in parallel so a slow backend can't
	// back-pressure the LISTEN reader. Defaults to 4.
	WorkerCount int
	// ClaimTTL is how long a worker's claim stays valid before the sweeper
	// reclaims it. Defaults to 60s. Must be > the expected Allocate
	// latency by a safe margin.
	ClaimTTL time.Duration
	// MaxAttempts is how many allocate-failed releases a ticket survives
	// before flipping to 'failed'. Defaults to 3.
	MaxAttempts int
	// SweepInterval is how often the cleanup goroutine sweeps stale
	// claims. Defaults to 60s. Only consulted when the Queue implements
	// Sweeper.
	SweepInterval time.Duration
	// EventDropCounter is incremented when a bucket event is dropped
	// because the consumer pool is saturated. Optional; nil disables.
	EventDropCounter Counter
	Logger           *slog.Logger
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
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 4
	}
	if cfg.ClaimTTL <= 0 {
		cfg.ClaimTTL = 60 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{queue: q, alloc: alloc, hub: hub, cfg: cfg, log: cfg.Logger}
}

// eventsBuffer is the bucket-event channel depth. Larger than the previous
// 64 so a momentary backlog doesn't force a drop; the consumer pool drains
// it fast enough under normal load. Drops are logged + metered.
const eventsBuffer = 1024

// Run drives the worker until ctx is cancelled. Returns only after every
// internal goroutine (listener, sweeper, consumer pool) has exited so the
// caller can deterministically drain the worker on shutdown.
func (w *Worker) Run(ctx context.Context) {
	events := make(chan Bucket, eventsBuffer)
	var wg sync.WaitGroup

	if l, ok := w.queue.(Listener); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runListener(ctx, l, events)
		}()
	}

	if s, ok := w.queue.(Sweeper); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runSweeper(ctx, s)
		}()
	}

	for i := 0; i < w.cfg.WorkerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runConsumer(ctx, events)
		}()
	}

	w.drain(ctx, events)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			w.drain(ctx, events)
		}
	}
}

// Tick performs one scan + dispatch pass synchronously (the listener and
// consumer pool are bypassed). Exposed for tests that want deterministic
// single-goroutine behavior.
func (w *Worker) Tick(ctx context.Context) error {
	buckets, err := w.queue.ListReadyBuckets(ctx, w.cfg.BucketSize)
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if err := w.processBucket(ctx, b); err != nil && !errors.Is(err, context.Canceled) {
			w.log.Warn("matchmaker: bucket failed",
				"tenant_id", b.TenantID, "project_id", b.ProjectID,
				"region", b.Region, "game_mode", b.GameMode, "err", err)
		}
	}
	return nil
}

// drain enumerates ready buckets and hands each to the consumer pool. On a
// full events channel the bucket is dropped and metered — the next tick or
// notify will pick it up.
func (w *Worker) drain(ctx context.Context, events chan<- Bucket) {
	buckets, err := w.queue.ListReadyBuckets(ctx, w.cfg.BucketSize)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			w.log.Warn("matchmaker: drain list failed", "err", err)
		}
		return
	}
	for _, b := range buckets {
		select {
		case events <- b:
		case <-ctx.Done():
			return
		default:
			w.dropEvent(b, "drain")
		}
	}
}

// runListener forwards NOTIFY events to the consumer pool until ctx is
// cancelled. Drops on a full channel; the fallback ticker covers the gap.
func (w *Worker) runListener(ctx context.Context, l Listener, out chan<- Bucket) {
	for {
		err := l.Listen(ctx, func(b Bucket) {
			select {
			case out <- b:
			case <-ctx.Done():
			default:
				w.dropEvent(b, "listener")
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

// runConsumer pulls buckets from events and processes them sequentially.
// One goroutine per pool slot; processBucket runs the full
// claim → allocate → commit pipeline.
func (w *Worker) runConsumer(ctx context.Context, events <-chan Bucket) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-events:
			if err := w.processBucket(ctx, b); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Warn("matchmaker: bucket failed",
					"tenant_id", b.TenantID, "project_id", b.ProjectID,
					"region", b.Region, "game_mode", b.GameMode, "err", err)
			}
		}
	}
}

// runSweeper releases claims whose lease has expired (crashed worker
// recovery). Runs on context.Background() with a per-call timeout so a
// request-scoped ctx cancellation doesn't stop the sweep mid-flight.
func (w *Worker) runSweeper(ctx context.Context, s Sweeper) {
	t := time.NewTicker(w.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			n, err := s.SweepStaleClaims(sweepCtx, w.cfg.MaxAttempts)
			cancel()
			if err != nil {
				w.log.Warn("matchmaker: sweep stale claims failed", "err", err)
				continue
			}
			if n > 0 {
				w.log.Info("matchmaker: swept stale claims", "released", n)
			}
		}
	}
}

func (w *Worker) processBucket(ctx context.Context, b Bucket) error {
	claim, err := w.queue.ClaimBucket(ctx, b, w.cfg.BucketSize, w.cfg.ClaimTTL)
	if err != nil {
		return fmt.Errorf("claim bucket: %w", err)
	}
	if claim == nil {
		return nil
	}

	tenantCtx := db.WithTenant(ctx, b.TenantID)
	tenantCtx = db.WithProject(tenantCtx, b.ProjectID)
	alloc, err := w.alloc.Allocate(tenantCtx, fleet.AllocationRequest{
		TenantID:  b.TenantID,
		ProjectID: b.ProjectID,
		FleetID:   b.FleetID,
		Region:    b.Region,
		GameMode:  b.GameMode,
		Capacity:  len(claim.Tickets),
	})
	if err != nil {
		if relErr := w.queue.ReleaseClaim(ctx, claim, w.cfg.MaxAttempts); relErr != nil {
			return fmt.Errorf("allocate failed (%w) and release claim (%v)", err, relErr)
		}
		return fmt.Errorf("allocate: %w", err)
	}

	committed, err := w.queue.CommitClaim(ctx, claim, alloc.Address, alloc.Protocol)
	if err != nil {
		w.deallocateOrphan(ctx, alloc, "commit error")
		return fmt.Errorf("commit claim: %w", err)
	}
	if committed == 0 {
		w.deallocateOrphan(ctx, alloc, "claim drifted")
		return nil
	}

	// If no client received match_ready, the match can't proceed — release
	// the allocation so the fleet slot is reusable instead of leaking.
	if notified := w.notifyMatched(ctx, claim.Tickets, alloc.Address); notified == 0 {
		w.deallocateOrphan(ctx, alloc, "no clients reachable")
	}
	return nil
}

// deallocateOrphan releases an allocation we made but couldn't bind to any
// ticket. Runs on context.Background() so a cancelled request doesn't strand
// the resource. Errors are logged; nothing else we can do.
func (w *Worker) deallocateOrphan(ctx context.Context, alloc *fleet.Allocation, reason string) {
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := w.alloc.Deallocate(cleanCtx, alloc.ID); err != nil {
		w.log.Warn("matchmaker: orphan deallocate failed",
			"reason", reason, "allocation_id", alloc.ID, "err", err)
		return
	}
	w.log.Info("matchmaker: orphan allocation released", "reason", reason, "allocation_id", alloc.ID)
}

// notifyMatched pushes match_ready to each player and returns the number of
// successful deliveries. A return of 0 means no client will ever connect to
// the allocated server, so the caller should release the allocation.
func (w *Worker) notifyMatched(ctx context.Context, tickets []*Ticket, address string) int {
	if w.hub == nil {
		return len(tickets)
	}
	delivered := 0
	for _, t := range tickets {
		p, _ := json.Marshal(map[string]any{"address": address, "ticket_id": t.ID})
		err := w.hub.Send(ctx, t.TenantID, t.EndUserID, realtime.Message{
			Type:    "match_ready",
			Payload: p,
		})
		switch {
		case err == nil:
			delivered++
		case errors.Is(err, realtime.ErrNotConnected):
			w.log.Info("matchmaker: notify skipped (client not connected)",
				"tenant_id", t.TenantID, "end_user_id", t.EndUserID)
		default:
			w.log.Warn("matchmaker: notify failed",
				"tenant_id", t.TenantID, "end_user_id", t.EndUserID, "err", err)
		}
	}
	return delivered
}

func (w *Worker) dropEvent(b Bucket, source string) {
	if w.cfg.EventDropCounter != nil {
		w.cfg.EventDropCounter.Inc()
	}
	w.log.Warn("matchmaker: bucket event dropped (consumer pool saturated)",
		"source", source, "tenant_id", b.TenantID, "project_id", b.ProjectID,
		"region", b.Region, "game_mode", b.GameMode)
}
