package matchmaker

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemQueue is an in-memory Queue used by worker tests and as a stand-in for
// local-only dev. Not safe for production: state vanishes on restart.
type MemQueue struct {
	mu      sync.Mutex
	nextID  int64
	tickets map[int64]*Ticket
}

// NewMemQueue returns an empty in-memory queue.
func NewMemQueue() *MemQueue {
	return &MemQueue{tickets: make(map[int64]*Ticket)}
}

// Enqueue inserts a queued ticket and returns the persisted view.
func (q *MemQueue) Enqueue(_ context.Context, req EnqueueRequest) (*Ticket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextID++
	t := &Ticket{
		ID:         q.nextID,
		TenantID:   req.TenantID,
		ProjectID:  req.ProjectID,
		EndUserID:  req.EndUserID,
		Region:     req.Region,
		GameMode:   req.GameMode,
		Attributes: req.Attributes,
		Status:     StatusQueued,
		CreatedAt:  time.Now().UTC(),
	}
	q.tickets[t.ID] = t
	return cloneTicket(t), nil
}

// Get returns a tenant-scoped view of the ticket. The tenant id is read
// from ctx via db.TenantFromContext, mirroring the Postgres RLS path.
func (q *MemQueue) Get(ctx context.Context, id int64) (*Ticket, error) {
	tenantID, err := tenantFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.tickets[id]
	if !ok || t.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneTicket(t), nil
}

// Cancel marks a queued ticket as cancelled. Returns ErrAlreadyTerminal when
// the ticket has already reached matched/cancelled/failed.
func (q *MemQueue) Cancel(ctx context.Context, id int64) error {
	tenantID, err := tenantFromCtx(ctx)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.tickets[id]
	if !ok || t.TenantID != tenantID {
		return ErrNotFound
	}
	if t.Status != StatusQueued {
		return ErrAlreadyTerminal
	}
	t.Status = StatusCancelled
	return nil
}

// ListReadyBuckets returns every (tenant, project, region, game_mode)
// bucket that currently holds at least minTickets queued entries.
func (q *MemQueue) ListReadyBuckets(_ context.Context, minTickets int) ([]Bucket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	counts := make(map[Bucket]int)
	for _, t := range q.tickets {
		if t.Status != StatusQueued {
			continue
		}
		counts[Bucket{TenantID: t.TenantID, ProjectID: t.ProjectID, Region: t.Region, GameMode: t.GameMode}]++
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
		if out[i].Region != out[j].Region {
			return out[i].Region < out[j].Region
		}
		return out[i].GameMode < out[j].GameMode
	})
	return out, nil
}

// PopBucket atomically claims up to n queued tickets in the bucket,
// marking them 'matched' (placeholder; the worker fills in the address
// afterwards via MarkMatched).
func (q *MemQueue) PopBucket(_ context.Context, bucket Bucket, n int) ([]*Ticket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	candidates := make([]*Ticket, 0)
	for _, t := range q.tickets {
		if t.Status != StatusQueued {
			continue
		}
		if t.TenantID != bucket.TenantID || t.ProjectID != bucket.ProjectID {
			continue
		}
		if t.Region != bucket.Region || t.GameMode != bucket.GameMode {
			continue
		}
		candidates = append(candidates, t)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	if len(candidates) < n {
		return nil, nil
	}
	taken := candidates[:n]
	out := make([]*Ticket, 0, n)
	for _, t := range taken {
		t.Status = StatusMatched
		out = append(out, cloneTicket(t))
	}
	return out, nil
}

// MarkMatched sets address + matched_at on each id (and re-asserts matched
// status in case of replay).
func (q *MemQueue) MarkMatched(_ context.Context, ids []int64, address string) error {
	now := time.Now().UTC()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range ids {
		t, ok := q.tickets[id]
		if !ok {
			continue
		}
		t.Status = StatusMatched
		t.MatchAddress = address
		t.MatchedAt = &now
	}
	return nil
}

// MarkFailed restores the previously-popped tickets to a terminal failed
// state.
func (q *MemQueue) MarkFailed(_ context.Context, ids []int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range ids {
		if t, ok := q.tickets[id]; ok {
			t.Status = StatusFailed
		}
	}
	return nil
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
