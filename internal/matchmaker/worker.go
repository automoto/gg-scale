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
	"github.com/ggscale/ggscale/internal/matchmaker/query"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/webutil"
)

// Allocator is the slice of fleet.Manager the worker uses. The narrow
// interface keeps the worker test-injectable without dragging the whole
// Manager into unit tests. Deallocate is invoked to clean up the orphan
// when CommitClaim loses a race to Cancel or the sweeper.
type Allocator interface {
	Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error)
	Deallocate(ctx context.Context, id fleet.AllocationID) error
}

// Notifier pushes matched envelopes back to matched players. *realtime.Hub
// is the production implementation.
type Notifier interface {
	Send(ctx context.Context, tenantID, playerID int64, msg realtime.Message) error
}

// SessionCreator mints a game session for a matched roster. The narrow
// interface keeps the worker test-injectable without importing the
// gamesession package; gamesession.MatchAdapter is the production
// implementation.
type SessionCreator interface {
	CreateMatchSession(ctx context.Context, projectID int64, gameMode string, players []int64) (sessionID, joinCode string, err error)
}

// Counter is the minimal Prometheus-ish interface the worker uses for the
// dropped-bucket-event metric. Avoids a hard dependency on
// prometheus/client_golang from this package; nil is valid.
type Counter interface {
	Inc()
}

// WorkerConfig controls group formation, scan cadence, and the claim
// lifecycle.
type WorkerConfig struct {
	// RelaxAfter is how long the oldest member of a below-max group waits
	// before the group commits at a smaller (still valid) size. 0 commits
	// undersized groups immediately.
	RelaxAfter time.Duration
	// RegionRelaxAfter is how long a bucket's oldest widen-eligible ticket
	// waits before cross-region grouping unlocks for allow_cross_region
	// tickets. 0 disables cross-region widening.
	RegionRelaxAfter time.Duration
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
	// MatchCounter is incremented once per committed match. Optional; nil
	// disables. Lets main wire a Prometheus counter without this package
	// importing the metrics layer.
	MatchCounter Counter
	// MatchTTL is the retention window for committed matchmaker_matches
	// rows (the poll-recovery record). Defaults to 24h.
	MatchTTL time.Duration
	// Sessions resolves game_session-mode buckets. nil fails those tickets
	// through the attempt counter.
	Sessions SessionCreator
	// QueryRejectCounter is incremented once per candidate pairing
	// rejected by mutual query acceptance. Optional; nil disables.
	QueryRejectCounter Counter
	Logger             *slog.Logger
}

// Worker consumes the queue, allocates servers, and notifies matched players.
type Worker struct {
	queue Queue
	alloc Allocator
	hub   Notifier
	cfg   WorkerConfig
	log   *slog.Logger
}

// NewWorker constructs a Worker. The queue is required; alloc may be nil
// when no fleet backend is configured (fleet buckets then release + fail
// through the attempt counter instead of allocating); notifier may be nil
// to suppress WS pushes (useful in tests).
func NewWorker(q Queue, alloc Allocator, hub Notifier, cfg WorkerConfig) *Worker {
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
	if cfg.MatchTTL <= 0 {
		cfg.MatchTTL = 24 * time.Hour
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
const listenerReconnectMaxBackoff = 30 * time.Second

// maxClaimBatch caps how many tickets one bucket pass claims for group
// formation. Anything beyond the cap stays queued for the next pass.
const maxClaimBatch = 128

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
	buckets, err := w.queue.ListReadyBuckets(ctx)
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
	buckets, err := w.queue.ListReadyBuckets(ctx)
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
	backoff := time.Second
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
			w.log.Warn("matchmaker: listener disconnected", "err", err, "retry_in", backoff)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > listenerReconnectMaxBackoff {
			backoff = listenerReconnectMaxBackoff
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
	claim, err := w.queue.ClaimBucket(ctx, b, maxClaimBatch, w.cfg.ClaimTTL)
	if err != nil {
		return fmt.Errorf("claim bucket: %w", err)
	}
	if claim == nil {
		return nil
	}
	// Tickets that fit no group this pass go straight back to waiting —
	// group formation is retried on every tick/notify. Every grouped ticket
	// is settled by commit or release below, so only pay for the un-claim
	// round-trip when some claimed ticket was left over.
	allGrouped := false
	defer func() {
		if allGrouped {
			return
		}
		if rerr := w.queue.ReturnUnmatched(ctx, claim); rerr != nil {
			w.log.Warn("matchmaker: return unmatched failed", "err", rerr)
		}
	}()

	tenantCtx := db.WithTenant(ctx, b.TenantID)
	tenantCtx = db.WithProject(tenantCtx, b.ProjectID)
	groups := formGroups(claim.Tickets, time.Now().UTC(), groupConfig{
		relaxAfter:       w.cfg.RelaxAfter,
		regionRelaxAfter: w.cfg.RegionRelaxAfter,
		queries:          w.parseQueries(claim.Tickets),
		onQueryReject:    w.countQueryReject,
	})
	var firstErr error
	grouped := 0
	for _, group := range groups {
		grouped += len(group)
		var gerr error
		switch b.Mode {
		case ModeMatchOnly:
			gerr = w.commitMatchOnly(ctx, tenantCtx, b, claim, group)
		case ModeGameSession:
			gerr = w.commitGameSession(ctx, tenantCtx, b, claim, group)
		case ModeFleetAllocation, "":
			gerr = w.commitFleetAllocation(ctx, tenantCtx, b, claim, group)
		default:
			// Fail unknown-mode tickets through the attempt counter
			// rather than starving them silently.
			gerr = w.releaseOnError(ctx, claim, group, fmt.Errorf("no matching path for mode %q", b.Mode))
		}
		if gerr != nil && firstErr == nil {
			firstErr = gerr
		}
	}
	allGrouped = grouped == len(claim.Tickets)
	return firstErr
}

// parseQueries compiles each ticket's criteria once per bucket pass. The
// HTTP handler validates queries at creation, so a parse failure here is a
// legacy/corrupt row — degrade it to match-all rather than starving the
// ticket.
func (w *Worker) parseQueries(tickets []*Ticket) map[int64]query.Expr {
	out := make(map[int64]query.Expr, len(tickets))
	for _, t := range tickets {
		e, err := query.Parse(t.Query)
		if err != nil {
			w.log.Warn("matchmaker: stored query unparsable, treating as match-all",
				"ticket_id", t.ID, "err", err)
			e = query.MatchAll
		}
		out[t.ID] = e
	}
	return out
}

func (w *Worker) countQueryReject() {
	if w.cfg.QueryRejectCounter != nil {
		w.cfg.QueryRejectCounter.Inc()
	}
}

// ticketIDs projects a group onto the id list the queue subset calls take.
func ticketIDs(group []*Ticket) []int64 {
	ids := make([]int64, 0, len(group))
	for _, t := range group {
		ids = append(ids, t.ID)
	}
	return ids
}

// releaseOnError is the shared failure path for a claimed-but-uncommitted
// group: release its tickets (bumping allocation_attempts) and return the
// step error, folding in a release failure when one happens.
func (w *Worker) releaseOnError(ctx context.Context, claim *Claim, group []*Ticket, err error) error {
	if relErr := w.queue.ReleaseTickets(ctx, claim, ticketIDs(group), w.cfg.MaxAttempts); relErr != nil {
		return fmt.Errorf("%w (and release tickets: %v)", err, relErr)
	}
	return err
}

// finalizeMatch persists the match, commits the claimed group, meters it, and
// fans out the matched event. Shared by the backend-free modes (match_only,
// game_session) whose commit tail is identical; the fleet path stays separate
// because it must reclaim its allocation on every failure branch. A commit
// error releases the group through the attempt counter (like every other
// failure branch) rather than un-claiming it penalty-free, so a persistently
// failing group eventually flips to 'failed' instead of looping forever.
func (w *Worker) finalizeMatch(ctx, tenantCtx context.Context, claim *Claim, group []*Ticket, match *Match) error {
	if err := w.queue.CreateMatch(tenantCtx, match); err != nil {
		return w.releaseOnError(ctx, claim, group, fmt.Errorf("create match: %w", err))
	}
	committed, err := w.queue.CommitTickets(ctx, claim, ticketIDs(group), match.ID, match.Address, match.Protocol)
	switch {
	case err != nil:
		return w.releaseOnError(ctx, claim, group, fmt.Errorf("commit tickets: %w", err))
	case committed == 0:
		// Claim drifted (cancel/sweep race). The orphan match row is
		// harmless and GC'd by retention.
		return nil
	}
	if w.cfg.MatchCounter != nil {
		w.cfg.MatchCounter.Inc()
	}
	w.notifyMatched(ctx, group, match)
	return nil
}

// commitMatchOnly resolves a group without any backend: mint a match id,
// persist the roster for poll recovery, flip the tickets, and fan the
// matched event. Delivery is best-effort — unlike fleet allocations there
// is no resource to reclaim, so the match stands even if nobody is
// connected.
func (w *Worker) commitMatchOnly(ctx, tenantCtx context.Context, b Bucket, claim *Claim, group []*Ticket) error {
	match, err := w.newMatch(b, group)
	if err != nil {
		return w.releaseOnError(ctx, claim, group, err)
	}
	return w.finalizeMatch(ctx, tenantCtx, claim, group, match)
}

// commitGameSession resolves a group by creating a game session: members
// are pre-seeded so a private session admits exactly the matched players,
// who then join/heartbeat with their own endpoints. A session orphaned by a
// commit race simply expires via its TTL.
func (w *Worker) commitGameSession(ctx, tenantCtx context.Context, b Bucket, claim *Claim, group []*Ticket) error {
	if w.cfg.Sessions == nil {
		return w.releaseOnError(ctx, claim, group, errors.New("no session creator configured"))
	}
	match, err := w.newMatch(b, group)
	if err != nil {
		return w.releaseOnError(ctx, claim, group, err)
	}
	players := make([]int64, 0, len(group))
	for _, t := range group {
		players = append(players, t.PlayerID)
	}
	sessionID, joinCode, err := w.cfg.Sessions.CreateMatchSession(tenantCtx, b.ProjectID, b.GameMode, players)
	if err != nil {
		return w.releaseOnError(ctx, claim, group, fmt.Errorf("create session: %w", err))
	}
	match.SessionID = sessionID
	match.JoinCode = joinCode
	return w.finalizeMatch(ctx, tenantCtx, claim, group, match)
}

// commitFleetAllocation is the dedicated-server path: allocate first, then
// commit; a failed allocate releases the group, and an unclaimable
// allocation is returned to the fleet.
func (w *Worker) commitFleetAllocation(ctx, tenantCtx context.Context, b Bucket, claim *Claim, group []*Ticket) error {
	if w.alloc == nil {
		return w.releaseOnError(ctx, claim, group, errors.New("no fleet allocator configured"))
	}
	alloc, err := w.alloc.Allocate(tenantCtx, fleet.AllocationRequest{
		TenantID:  b.TenantID,
		ProjectID: b.ProjectID,
		FleetID:   b.FleetID,
		Region:    b.Region,
		GameMode:  b.GameMode,
		Capacity:  len(group),
	})
	if err != nil {
		return w.releaseOnError(ctx, claim, group, fmt.Errorf("allocate: %w", err))
	}

	match, err := w.newMatch(b, group)
	if err != nil {
		w.deallocateOrphan(tenantCtx, alloc, "match setup failed")
		return w.releaseOnError(ctx, claim, group, err)
	}
	match.Address = alloc.Address
	match.Protocol = alloc.Protocol
	if err := w.queue.CreateMatch(tenantCtx, match); err != nil {
		w.deallocateOrphan(tenantCtx, alloc, "match setup failed")
		return w.releaseOnError(ctx, claim, group, fmt.Errorf("create match: %w", err))
	}

	committed, err := w.queue.CommitTickets(ctx, claim, ticketIDs(group), match.ID, alloc.Address, alloc.Protocol)
	switch {
	case err != nil:
		w.deallocateOrphan(tenantCtx, alloc, "commit error")
		// Release through the attempt counter instead of letting the group
		// be un-claimed penalty-free; otherwise a recurring commit error
		// re-allocates and re-fails forever, churning fleet servers.
		return w.releaseOnError(ctx, claim, group, fmt.Errorf("commit tickets: %w", err))
	case committed == 0:
		w.deallocateOrphan(tenantCtx, alloc, "claim drifted")
		return nil
	}
	if w.cfg.MatchCounter != nil {
		w.cfg.MatchCounter.Inc()
	}

	// If no client received the matched event, the match can't proceed —
	// release the allocation so the fleet slot is reusable instead of
	// leaking. Deallocate goes through the fleet store, which requires
	// tenant context. The players can still recover the result by polling.
	if notified := w.notifyMatched(ctx, group, match); notified == 0 {
		w.deallocateOrphan(tenantCtx, alloc, "no clients reachable")
	}
	return nil
}

// newMatch mints a match record for the bucket's tickets.
func (w *Worker) newMatch(b Bucket, tickets []*Ticket) (*Match, error) {
	id, err := webutil.RandomHex("mm_", 8)
	if err != nil {
		return nil, fmt.Errorf("generate match id: %w", err)
	}
	roster := make([]RosterEntry, 0, len(tickets))
	for _, t := range tickets {
		roster = append(roster, RosterEntry{
			PlayerID:          t.PlayerID,
			Region:            t.Region,
			StringProperties:  t.StringProperties,
			NumericProperties: t.NumericProperties,
		})
	}
	return &Match{
		ID:        id,
		TenantID:  b.TenantID,
		ProjectID: b.ProjectID,
		Mode:      b.Mode,
		FleetID:   b.FleetID,
		Roster:    roster,
		ExpiresAt: time.Now().UTC().Add(w.cfg.MatchTTL),
	}, nil
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

// matchedEventType is the realtime envelope type for committed matches.
const matchedEventType = "matchmaker_matched"

// matchedPayload is the matchmaker_matched envelope body. Address/protocol
// are set for fleet_allocation, session/join code for game_session.
type matchedPayload struct {
	TicketID     int64         `json:"ticket_id"`
	MatchID      string        `json:"match_id"`
	Mode         Mode          `json:"mode"`
	Address      string        `json:"address,omitempty"`
	ProtocolHint string        `json:"protocol_hint,omitempty"`
	SessionID    string        `json:"session_id,omitempty"`
	JoinCode     string        `json:"join_code,omitempty"`
	Users        []RosterEntry `json:"users"`
}

// notifyMatched pushes matchmaker_matched to each rostered player and
// returns the number of successful deliveries. A return of 0 tells the
// fleet path no client will ever connect to the allocated server, so that
// caller releases the allocation.
func (w *Worker) notifyMatched(ctx context.Context, tickets []*Ticket, match *Match) int {
	if w.hub == nil {
		return len(tickets)
	}
	delivered := 0
	for _, t := range tickets {
		p, _ := json.Marshal(matchedPayload{
			TicketID:     t.ID,
			MatchID:      match.ID,
			Mode:         match.Mode,
			Address:      match.Address,
			ProtocolHint: match.Protocol,
			SessionID:    match.SessionID,
			JoinCode:     match.JoinCode,
			Users:        match.Roster,
		})
		err := w.hub.Send(ctx, t.TenantID, t.PlayerID, realtime.Message{
			Type:    matchedEventType,
			Payload: p,
		})
		switch {
		case err == nil:
			delivered++
		case errors.Is(err, realtime.ErrNotConnected):
			w.log.Info("matchmaker: notify skipped (client not connected)",
				"tenant_id", t.TenantID, "player_id", t.PlayerID)
		default:
			w.log.Warn("matchmaker: notify failed",
				"tenant_id", t.TenantID, "player_id", t.PlayerID, "err", err)
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
