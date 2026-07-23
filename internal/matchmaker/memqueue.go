package matchmaker

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemQueue is an in-memory Queue used by worker tests and as a stand-in for
// local-only dev. Not safe for production: state vanishes on restart.
type MemQueue struct {
	mu      sync.Mutex
	nextID  int64
	tickets map[int64]*memTicket
	matches map[string]*Match
}

// memTicket is the internal storage row. The exported Ticket is constructed
// by cloning out of this on read.
type memTicket struct {
	Ticket
	claimID            string
	claimExpiresAt     time.Time
	allocationAttempts int
}

// NewMemQueue returns an empty in-memory queue.
func NewMemQueue() *MemQueue {
	return &MemQueue{tickets: make(map[int64]*memTicket), matches: make(map[string]*Match)}
}

// CreateMatch stores a committed match result.
func (q *MemQueue) CreateMatch(_ context.Context, m *Match) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.matches[m.ID] = cloneMatch(m)
	return nil
}

// GetMatch returns the match by id for the tenant on ctx.
func (q *MemQueue) GetMatch(ctx context.Context, id string) (*Match, error) {
	tenantID, err := tenantFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	m, ok := q.matches[id]
	if !ok || m.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneMatch(m), nil
}

// ClaimMatch atomically claims and returns an unexpired tenant match.
func (q *MemQueue) ClaimMatch(ctx context.Context, id string) (*Match, error) {
	tenantID, err := tenantFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	m, ok := q.matches[id]
	if !ok || m.TenantID != tenantID || !m.ExpiresAt.After(time.Now().UTC()) {
		return nil, ErrNotFound
	}
	if m.ClaimedAt.IsZero() {
		m.ClaimedAt = time.Now().UTC()
	}
	return cloneMatch(m), nil
}

func cloneMatch(m *Match) *Match {
	dup := *m
	dup.Roster = append([]RosterEntry(nil), m.Roster...)
	return &dup
}

// Enqueue inserts a queued ticket and returns the persisted view. One active
// (queued) ticket per player per project is enforced under the queue lock,
// mirroring the Postgres partial unique index.
func (q *MemQueue) Enqueue(_ context.Context, req EnqueueRequest) (*Ticket, error) {
	req.normalize()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.tickets {
		if t.Status != StatusQueued || t.TenantID != req.TenantID || t.ProjectID != req.ProjectID || t.PlayerID != req.PlayerID {
			continue
		}
		// An expired-but-unswept ticket must not block re-queuing; TTL-expire
		// it here as the sweeper would, mirroring the Postgres enqueue path.
		// Claimed tickets are left for the claim path to settle.
		if t.claimID == "" && expired(&t.Ticket) {
			t.Status = StatusFailed
			t.FailureReason = failureReasonExpired
			continue
		}
		return nil, &TicketActiveError{ActiveTicketID: t.ID}
	}
	q.nextID++
	t := &memTicket{
		Ticket: Ticket{
			ID:                q.nextID,
			TenantID:          req.TenantID,
			ProjectID:         req.ProjectID,
			FleetID:           req.FleetID,
			PlayerID:          req.PlayerID,
			Mode:              req.Mode,
			Region:            req.Region,
			GameMode:          req.GameMode,
			Attributes:        req.Attributes,
			MinCount:          req.MinCount,
			MaxCount:          req.MaxCount,
			CountMultiple:     req.CountMultiple,
			AllowCrossRegion:  req.AllowCrossRegion,
			Query:             req.Query,
			StringProperties:  req.StringProperties,
			NumericProperties: req.NumericProperties,
			Status:            StatusQueued,
			CreatedAt:         time.Now().UTC(),
			ExpiresAt:         req.ExpiresAt,
		},
	}
	q.tickets[t.ID] = t
	return cloneTicket(&t.Ticket), nil
}

// expired reports whether the ticket's TTL has lapsed.
func expired(t *Ticket) bool {
	return t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now().UTC())
}

// Get returns a tenant-scoped view of the ticket. The tenant id is read
// from ctx via db.TenantFromContext, mirroring the Postgres RLS path.
func (q *MemQueue) Get(ctx context.Context, id, playerID int64) (*Ticket, error) {
	tenantID, err := tenantFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.tickets[id]
	if !ok || t.TenantID != tenantID || t.PlayerID != playerID {
		return nil, ErrNotFound
	}
	return cloneTicket(&t.Ticket), nil
}

// Cancel marks a queued ticket as cancelled. Returns ErrAlreadyTerminal when
// the ticket has already reached matched/cancelled/failed. Cancelling a
// claimed ticket clears the claim cols too, so the worker's CommitClaim
// finds zero rows and deallocates the orphan.
func (q *MemQueue) Cancel(ctx context.Context, id, playerID int64) error {
	tenantID, err := tenantFromCtx(ctx)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.tickets[id]
	if !ok || t.TenantID != tenantID || t.PlayerID != playerID {
		return ErrNotFound
	}
	if t.Status != StatusQueued {
		return ErrAlreadyTerminal
	}
	t.Status = StatusCancelled
	t.claimID = ""
	t.claimExpiresAt = time.Time{}
	return nil
}

// ListReadyBuckets returns every bucket that currently holds unclaimed
// queued tickets. Region is a bucket dimension only for fleet_allocation;
// non-fleet buckets mix regions and the worker applies the soft-region
// rules in Go.
func (q *MemQueue) ListReadyBuckets(_ context.Context) ([]Bucket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	counts := make(map[Bucket]int)
	for _, t := range q.tickets {
		if t.Status != StatusQueued || t.claimID != "" || expired(&t.Ticket) {
			continue
		}
		counts[bucketKey(&t.Ticket)]++
	}
	out := make([]Bucket, 0, len(counts))
	for b := range counts {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		if out[i].Mode != out[j].Mode {
			return out[i].Mode < out[j].Mode
		}
		if out[i].FleetID != out[j].FleetID {
			return out[i].FleetID < out[j].FleetID
		}
		if out[i].Region != out[j].Region {
			return out[i].Region < out[j].Region
		}
		return out[i].GameMode < out[j].GameMode
	})
	return out, nil
}

// bucketKey maps a ticket to its bucket, blanking region for non-fleet
// modes (soft-region grouping happens in the worker).
func bucketKey(t *Ticket) Bucket {
	region := t.Region
	if t.Mode != ModeFleetAllocation {
		region = ""
	}
	return Bucket{TenantID: t.TenantID, ProjectID: t.ProjectID, Mode: t.Mode, FleetID: t.FleetID, Region: region, GameMode: t.GameMode}
}

// ClaimBucket stakes a claim on up to max unclaimed queued tickets, oldest
// first. Returns nil when nothing was claimable.
func (q *MemQueue) ClaimBucket(_ context.Context, bucket Bucket, max int, ttl time.Duration) (*Claim, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	candidates := make([]*memTicket, 0)
	for _, t := range q.tickets {
		if t.Status != StatusQueued || t.claimID != "" || expired(&t.Ticket) {
			continue
		}
		if bucketKey(&t.Ticket) != bucket {
			continue
		}
		candidates = append(candidates, t)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	if len(candidates) == 0 {
		return nil, nil
	}
	taken := candidates[:min(max, len(candidates))]
	claimID := uuid.NewString()
	expires := time.Now().UTC().Add(ttl)
	out := make([]*Ticket, 0, len(taken))
	for _, t := range taken {
		t.claimID = claimID
		t.claimExpiresAt = expires
		out = append(out, cloneTicket(&t.Ticket))
	}
	return &Claim{ID: claimID, Tickets: out}, nil
}

// CommitTickets flips the given still-queued claim tickets to 'matched' with
// the match id, address + protocol hint, all-or-none: a full commit returns
// (len, nil), a fully drifted claim returns (0, nil), and a partial commit
// flips nothing and returns the would-be count with ErrShortCommit.
func (q *MemQueue) CommitTickets(_ context.Context, claim *Claim, ticketIDs []int64, matchID, matchAddress, matchProtocol string) (int64, error) {
	if claim == nil || claim.ID == "" || len(ticketIDs) == 0 {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var committable int64
	for _, id := range ticketIDs {
		if t, ok := q.tickets[id]; ok && t.claimID == claim.ID && t.Status == StatusQueued {
			committable++
		}
	}
	if committable == 0 {
		return 0, nil
	}
	if committable != int64(len(ticketIDs)) {
		return committable, ErrShortCommit
	}
	now := time.Now().UTC()
	for _, id := range ticketIDs {
		t := q.tickets[id]
		t.Status = StatusMatched
		t.MatchID = matchID
		t.MatchAddress = matchAddress
		t.MatchProtocol = matchProtocol
		t.MatchedAt = &now
		t.claimID = ""
		t.claimExpiresAt = time.Time{}
	}
	return committable, nil
}

// ReleaseTickets clears the claim on the given tickets, bumps attempts, and
// flips to 'failed' at the cap.
func (q *MemQueue) ReleaseTickets(_ context.Context, claim *Claim, ticketIDs []int64, maxAttempts int) error {
	if claim == nil || claim.ID == "" || len(ticketIDs) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range ticketIDs {
		t, ok := q.tickets[id]
		if !ok || t.claimID != claim.ID || t.Status != StatusQueued {
			continue
		}
		t.allocationAttempts++
		t.claimID = ""
		t.claimExpiresAt = time.Time{}
		if t.allocationAttempts >= maxAttempts {
			t.Status = StatusFailed
			t.FailureReason = failureReasonAttemptsExhausted
		}
	}
	return nil
}

// ReturnUnmatched un-claims whatever the claim still holds without penalty.
func (q *MemQueue) ReturnUnmatched(_ context.Context, claim *Claim) error {
	if claim == nil || claim.ID == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.tickets {
		if t.claimID != claim.ID || t.Status != StatusQueued {
			continue
		}
		t.claimID = ""
		t.claimExpiresAt = time.Time{}
	}
	return nil
}

// ReturnTickets un-claims the given still-queued claim tickets without
// penalty (short-commit survivors).
func (q *MemQueue) ReturnTickets(_ context.Context, claim *Claim, ticketIDs []int64) error {
	if claim == nil || claim.ID == "" || len(ticketIDs) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range ticketIDs {
		t, ok := q.tickets[id]
		if !ok || t.claimID != claim.ID || t.Status != StatusQueued {
			continue
		}
		t.claimID = ""
		t.claimExpiresAt = time.Time{}
	}
	return nil
}

// SweepStaleClaims releases every expired claim. MemQueue implements Sweeper
// for symmetry with PGQueue and for cleanup tests.
func (q *MemQueue) SweepStaleClaims(_ context.Context, maxAttempts int) (int64, error) {
	now := time.Now().UTC()
	q.mu.Lock()
	defer q.mu.Unlock()
	var released int64
	for _, t := range q.tickets {
		if t.Status != StatusQueued {
			continue
		}
		if t.claimID == "" && expired(&t.Ticket) {
			t.Status = StatusFailed
			t.FailureReason = failureReasonExpired
			released++
			continue
		}
		if t.claimID == "" || !t.claimExpiresAt.Before(now) {
			continue
		}
		t.allocationAttempts++
		t.claimID = ""
		t.claimExpiresAt = time.Time{}
		if t.allocationAttempts >= maxAttempts {
			t.Status = StatusFailed
			t.FailureReason = failureReasonAttemptsExhausted
		}
		released++
	}
	return released, nil
}

func cloneTicket(t *Ticket) *Ticket {
	dup := *t
	if t.Attributes != nil {
		dup.Attributes = append([]byte(nil), t.Attributes...)
	}
	if t.StringProperties != nil {
		dup.StringProperties = make(map[string]string, len(t.StringProperties))
		for k, v := range t.StringProperties {
			dup.StringProperties[k] = v
		}
	}
	if t.NumericProperties != nil {
		dup.NumericProperties = make(map[string]float64, len(t.NumericProperties))
		for k, v := range t.NumericProperties {
			dup.NumericProperties[k] = v
		}
	}
	if t.MatchedAt != nil {
		v := *t.MatchedAt
		dup.MatchedAt = &v
	}
	if t.ExpiresAt != nil {
		v := *t.ExpiresAt
		dup.ExpiresAt = &v
	}
	return &dup
}
