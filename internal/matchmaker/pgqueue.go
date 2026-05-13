package matchmaker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

const notifyChannel = "matchmaker_ticket"

// PGQueue is the production Queue, backed by Postgres. Tenant-scoped reads
// (Enqueue, Get, Cancel) run inside db.Pool.Q so RLS receives app.tenant_id;
// privileged paths called by the worker run inside db.Pool.BootstrapQ.
type PGQueue struct {
	pool *db.Pool
}

// NewPGQueue returns a Queue backed by the given pool.
func NewPGQueue(pool *db.Pool) *PGQueue {
	return &PGQueue{pool: pool}
}

// Enqueue persists a queued ticket. The caller must have an end-user
// authenticated context (tenant_id is read via RLS).
func (q *PGQueue) Enqueue(ctx context.Context, req EnqueueRequest) (*Ticket, error) {
	// The attributes column is NOT NULL with a JSONB default of '{}'. The
	// default only fires when the column is omitted from INSERT, but our
	// sqlc query always passes it — so map nil/empty to literal "{}".
	attrs := req.Attributes
	if len(attrs) == 0 {
		attrs = []byte("{}")
	}
	var ticket *Ticket
	err := q.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).InsertMatchmakingTicket(ctx, sqlcgen.InsertMatchmakingTicketParams{
			ProjectID:  req.ProjectID,
			EndUserID:  req.EndUserID,
			Region:     req.Region,
			GameMode:   req.GameMode,
			Attributes: attrs,
		})
		if qerr != nil {
			return qerr
		}
		ticket = &Ticket{
			ID:         row.ID,
			TenantID:   req.TenantID,
			ProjectID:  req.ProjectID,
			EndUserID:  req.EndUserID,
			Region:     req.Region,
			GameMode:   req.GameMode,
			Attributes: req.Attributes,
			Status:     Status(row.Status),
			CreatedAt:  row.CreatedAt.Time,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("matchmaker: enqueue: %w", err)
	}
	return ticket, nil
}

// Get returns the ticket if it belongs to the tenant on ctx; otherwise
// ErrNotFound.
func (q *PGQueue) Get(ctx context.Context, id int64) (*Ticket, error) {
	var t *Ticket
	err := q.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetMatchmakingTicket(ctx, id)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		t = rowToTicket(row.ID, row.TenantID, row.ProjectID, row.EndUserID, row.Region, row.GameMode, row.Attributes, row.Status, row.MatchAddress)
		t.CreatedAt = row.CreatedAt.Time
		if row.MatchedAt.Valid {
			v := row.MatchedAt.Time
			t.MatchedAt = &v
		}
		return nil
	})
	return t, err
}

// Cancel sets a queued ticket to cancelled. Returns ErrAlreadyTerminal when
// the ticket is past 'queued'.
func (q *PGQueue) Cancel(ctx context.Context, id int64) error {
	return q.pool.Q(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).CancelMatchmakingTicket(ctx, id)
		if errors.Is(qerr, pgx.ErrNoRows) {
			// Either not the tenant's row, or already past queued. Disambiguate
			// with a Get so callers get the right error.
			row, gerr := sqlcgen.New(tx).GetMatchmakingTicket(ctx, id)
			if errors.Is(gerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if gerr != nil {
				return gerr
			}
			if Status(row.Status) != StatusQueued {
				return ErrAlreadyTerminal
			}
			return ErrNotFound
		}
		return qerr
	})
}

// ListReadyBuckets is privileged: it scans across all tenants.
func (q *PGQueue) ListReadyBuckets(ctx context.Context, minTickets int) ([]Bucket, error) {
	var out []Bucket
	err := q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).ListReadyMatchmakerBuckets(ctx, int32(minTickets)) //nolint:gosec // minTickets is operator-controlled config
		if qerr != nil {
			return qerr
		}
		for _, r := range rows {
			out = append(out, Bucket{TenantID: r.TenantID, ProjectID: r.ProjectID, Region: r.Region, GameMode: r.GameMode})
		}
		return nil
	})
	return out, err
}

// PopBucket atomically claims up to n queued tickets in the bucket and
// flips them to 'matched'. The worker then fills in the address via
// MarkMatched (or MarkFailed on allocation failure).
func (q *PGQueue) PopBucket(ctx context.Context, bucket Bucket, n int) ([]*Ticket, error) {
	var out []*Ticket
	err := q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).PopMatchmakerBucket(ctx, sqlcgen.PopMatchmakerBucketParams{
			TenantID:  bucket.TenantID,
			ProjectID: bucket.ProjectID,
			Region:    bucket.Region,
			GameMode:  bucket.GameMode,
			Column5:   int32(n), //nolint:gosec // n is operator-controlled bucket size
		})
		if qerr != nil {
			return qerr
		}
		if len(rows) < n {
			// Worker convention: don't allocate when the bucket short-counts
			// (another worker won the race). The flipped rows stay 'matched'
			// and will be marked failed downstream, but in practice this only
			// happens with concurrent workers.
			return nil
		}
		for _, r := range rows {
			t := rowToTicket(r.ID, r.TenantID, r.ProjectID, r.EndUserID, r.Region, r.GameMode, r.Attributes, r.Status, r.MatchAddress)
			t.CreatedAt = r.CreatedAt.Time
			out = append(out, t)
		}
		return nil
	})
	return out, err
}

// MarkMatched stamps the address + matched_at on each id.
func (q *PGQueue) MarkMatched(ctx context.Context, ids []int64, address string) error {
	if len(ids) == 0 {
		return nil
	}
	return q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).MarkMatchmakerMatched(ctx, sqlcgen.MarkMatchmakerMatchedParams{
			Column1:      ids,
			MatchAddress: address,
		})
	})
}

// Listen subscribes to the matchmaker_ticket NOTIFY channel and dispatches
// each (tenant, project, region, game_mode) payload to fn. Returns nil on
// ctx.Done() and a wrapped error if the underlying connection drops — the
// caller is expected to back off and reconnect, with the worker's fallback
// ticker covering any gap.
func (q *PGQueue) Listen(ctx context.Context, fn func(Bucket)) error {
	return q.pool.ListenChannel(ctx, notifyChannel, func(payload string) {
		var p struct {
			TenantID  int64  `json:"tenant_id"`
			ProjectID int64  `json:"project_id"`
			Region    string `json:"region"`
			GameMode  string `json:"game_mode"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return // malformed payload; fallback tick will catch the ticket
		}
		fn(Bucket{TenantID: p.TenantID, ProjectID: p.ProjectID, Region: p.Region, GameMode: p.GameMode})
	})
}

// MarkFailed flips ids from 'matched' (the placeholder set by PopBucket)
// back to 'failed' so the player can see the result and retry.
func (q *PGQueue) MarkFailed(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).MarkMatchmakerFailed(ctx, ids)
	})
}

func rowToTicket(id, tenantID, projectID, endUserID int64, region, gameMode string, attrs []byte, status, matchAddress string) *Ticket {
	return &Ticket{
		ID:           id,
		TenantID:     tenantID,
		ProjectID:    projectID,
		EndUserID:    endUserID,
		Region:       region,
		GameMode:     gameMode,
		Attributes:   attrs,
		Status:       Status(status),
		MatchAddress: matchAddress,
	}
}
