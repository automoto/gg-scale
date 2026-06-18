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
	return &MemQueue{tickets: make(map[int64]*memTicket)}
}

// Enqueue inserts a queued ticket and returns the persisted view.
func (q *MemQueue) Enqueue(_ context.Context, req EnqueueRequest) (*Ticket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextID++
	t := &memTicket{
		Ticket: Ticket{
			ID:         q.nextID,
			TenantID:   req.TenantID,
			ProjectID:  req.ProjectID,
			FleetID:    req.FleetID,
			EndUserID:  req.EndUserID,
			Region:     req.Region,
			GameMode:   req.GameMode,
			Attributes: req.Attributes,
			Status:     StatusQueued,
			CreatedAt:  time.Now().UTC(),
		},
	}
	q.tickets[t.ID] = t
	return cloneTicket(&t.Ticket), nil
}

// Get returns a tenant-scoped view of the ticket. The tenant id is read
// from ctx via db.TenantFromContext, mirroring the Postgres RLS path.
func (q *MemQueue) Get(ctx context.Context, id, endUserID int64) (*Ticket, error) {
	tenantID, err := tenantFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.tickets[id]
	if !ok || t.TenantID != tenantID || t.EndUserID != endUserID {
		return nil, ErrNotFound
	}
	return cloneTicket(&t.Ticket), nil
}

// Cancel marks a queued ticket as cancelled. Returns ErrAlreadyTerminal when
// the ticket has already reached matched/cancelled/failed. Cancelling a
// claimed ticket clears the claim cols too, so the worker's CommitClaim
// finds zero rows and deallocates the orphan.
func (q *MemQueue) Cancel(ctx context.Context, id, endUserID int64) error {
	tenantID, err := tenantFromCtx(ctx)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.tickets[id]
	if !ok || t.TenantID != tenantID || t.EndUserID != endUserID {
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

// ListReadyBuckets returns every (tenant, project, fleet, region, game_mode)
// bucket that currently holds at least minTickets unclaimed queued entries.
func (q *MemQueue) ListReadyBuckets(_ context.Context, minTickets int) ([]Bucket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	counts := make(map[Bucket]int)
	for _, t := range q.tickets {
		if t.Status != StatusQueued || t.claimID != "" {
			continue
		}
		counts[Bucket{TenantID: t.TenantID, ProjectID: t.ProjectID, FleetID: t.FleetID, Region: t.Region, GameMode: t.GameMode}]++
	}
	out := make([]Bucket, 0, len(counts))
	for b, c := range counts {
		if c >= minTickets {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
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

// ClaimBucket stakes a claim on up to n unclaimed queued tickets. Returns
// nil on short count.
func (q *MemQueue) ClaimBucket(_ context.Context, bucket Bucket, n int, ttl time.Duration) (*Claim, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	candidates := make([]*memTicket, 0)
	for _, t := range q.tickets {
		if t.Status != StatusQueued || t.claimID != "" {
			continue
		}
		if t.TenantID != bucket.TenantID || t.ProjectID != bucket.ProjectID {
			continue
		}
		if t.FleetID != bucket.FleetID {
			continue
		}
		if t.Region != bucket.Region || t.GameMode != bucket.GameMode {
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
	if len(candidates) < n {
		return nil, nil
	}
	taken := candidates[:n]
	claimID := uuid.NewString()
	expires := time.Now().UTC().Add(ttl)
	out := make([]*Ticket, 0, n)
	for _, t := range taken {
		t.claimID = claimID
		t.claimExpiresAt = expires
		out = append(out, cloneTicket(&t.Ticket))
	}
	return &Claim{ID: claimID, Tickets: out}, nil
}

// CommitClaim flips every still-queued row with the given claim id to
// 'matched' with the address + protocol hint. Returns rows-affected
// (0 if the claim drifted).
func (q *MemQueue) CommitClaim(_ context.Context, claim *Claim, matchAddress, matchProtocol string) (int64, error) {
	if claim == nil || claim.ID == "" {
		return 0, nil
	}
	now := time.Now().UTC()
	q.mu.Lock()
	defer q.mu.Unlock()
	var affected int64
	for _, t := range q.tickets {
		if t.claimID != claim.ID || t.Status != StatusQueued {
			continue
		}
		t.Status = StatusMatched
		t.MatchAddress = matchAddress
		t.MatchProtocol = matchProtocol
		t.MatchedAt = &now
		t.claimID = ""
		t.claimExpiresAt = time.Time{}
		affected++
	}
	return affected, nil
}

// ReleaseClaim clears the claim, bumps attempts, and flips to 'failed' at
// the cap.
func (q *MemQueue) ReleaseClaim(_ context.Context, claim *Claim, maxAttempts int) error {
	if claim == nil || claim.ID == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.tickets {
		if t.claimID != claim.ID || t.Status != StatusQueued {
			continue
		}
		t.allocationAttempts++
		t.claimID = ""
		t.claimExpiresAt = time.Time{}
		if t.allocationAttempts >= maxAttempts {
			t.Status = StatusFailed
		}
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
		if t.claimID == "" || t.Status != StatusQueued || !t.claimExpiresAt.Before(now) {
			continue
		}
		t.allocationAttempts++
		t.claimID = ""
		t.claimExpiresAt = time.Time{}
		if t.allocationAttempts >= maxAttempts {
			t.Status = StatusFailed
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
	if t.MatchedAt != nil {
		v := *t.MatchedAt
		dup.MatchedAt = &v
	}
	return &dup
}
