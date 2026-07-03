package matchmaker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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

// Enqueue persists a queued ticket. The caller must have a player
// authenticated context (tenant_id is read via RLS).
func (q *PGQueue) Enqueue(ctx context.Context, req EnqueueRequest) (*Ticket, error) {
	attrs := req.Attributes
	if len(attrs) == 0 {
		attrs = []byte("{}")
	}
	var ticket *Ticket
	err := q.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).InsertMatchmakingTicket(ctx, sqlcgen.InsertMatchmakingTicketParams{
			ProjectID:  req.ProjectID,
			FleetID:    &req.FleetID,
			PlayerID:   req.PlayerID,
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
			FleetID:    req.FleetID,
			PlayerID:   req.PlayerID,
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
func (q *PGQueue) Get(ctx context.Context, id, playerID int64) (*Ticket, error) {
	var t *Ticket
	err := q.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetMatchmakingTicket(ctx, sqlcgen.GetMatchmakingTicketParams{
			ID:       id,
			PlayerID: playerID,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		t = rowToTicket(row.ID, row.TenantID, row.ProjectID, derefFleetID(row.FleetID), row.PlayerID, row.Region, row.GameMode, row.Attributes, row.Status, row.MatchAddress, row.MatchProtocol)
		t.CreatedAt = row.CreatedAt.Time
		if row.MatchedAt.Valid {
			v := row.MatchedAt.Time
			t.MatchedAt = &v
		}
		return nil
	})
	return t, err
}

func derefFleetID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Cancel sets a queued ticket to cancelled and clears any active claim.
// Returns ErrAlreadyTerminal when the ticket is past 'queued'.
func (q *PGQueue) Cancel(ctx context.Context, id, playerID int64) error {
	return q.pool.Q(ctx, func(tx pgx.Tx) error {
		arg := sqlcgen.CancelMatchmakingTicketParams{
			ID:       id,
			PlayerID: playerID,
		}
		_, qerr := sqlcgen.New(tx).CancelMatchmakingTicket(ctx, arg)
		if errors.Is(qerr, pgx.ErrNoRows) {
			row, gerr := sqlcgen.New(tx).GetMatchmakingTicket(ctx, sqlcgen.GetMatchmakingTicketParams(arg))
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

// ListReadyBuckets is privileged: it scans across all tenants for buckets
// holding minTickets or more unclaimed queued entries.
func (q *PGQueue) ListReadyBuckets(ctx context.Context, minTickets int) ([]Bucket, error) {
	var out []Bucket
	err := q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).ListReadyMatchmakerBuckets(ctx, int32(minTickets)) //nolint:gosec // operator config (BucketSize), validated > 0 by NewWorker
		if qerr != nil {
			return qerr
		}
		for _, r := range rows {
			out = append(out, Bucket{
				TenantID:  r.TenantID,
				ProjectID: r.ProjectID,
				FleetID:   derefFleetID(r.FleetID),
				Region:    r.Region,
				GameMode:  r.GameMode,
			})
		}
		return nil
	})
	return out, err
}

// ClaimBucket stakes a UUID-keyed claim on up to n unclaimed queued tickets.
// Status stays 'queued'; only claim_id / claimed_at / claim_expires_at are
// set. Returns nil on short count.
func (q *PGQueue) ClaimBucket(ctx context.Context, bucket Bucket, n int, ttl time.Duration) (*Claim, error) {
	claimUUID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("matchmaker: generate claim uuid: %w", err)
	}
	pgUUID := pgtype.UUID{Bytes: claimUUID, Valid: true}
	pgTTL := pgtype.Interval{Microseconds: ttl.Microseconds(), Valid: true}

	var rows []sqlcgen.ClaimMatchmakerBucketRow
	err = q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		fleetID := bucket.FleetID
		r, qerr := sqlcgen.New(tx).ClaimMatchmakerBucket(ctx, sqlcgen.ClaimMatchmakerBucketParams{
			TenantID:  bucket.TenantID,
			ProjectID: bucket.ProjectID,
			FleetID:   &fleetID,
			Region:    bucket.Region,
			GameMode:  bucket.GameMode,
			ClaimID:   pgUUID,
			Ttl:       pgTTL,
			Limit:     int32(n), //nolint:gosec // operator config (BucketSize), validated > 0 by NewWorker
		})
		if qerr != nil {
			return qerr
		}
		rows = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(rows) < n {
		// Short count: another worker won the race. Release whatever we
		// did claim so the bucket can be re-picked on the next tick.
		// Pass a large headroom so a short-count release doesn't burn
		// allocation_attempts — only worker-driven failures count toward
		// the failure ceiling.
		if len(rows) > 0 {
			_ = q.releaseByClaimID(ctx, pgUUID, defaultRetryHeadroom)
		}
		return nil, nil
	}
	tickets := make([]*Ticket, 0, len(rows))
	for _, r := range rows {
		t := rowToTicket(r.ID, r.TenantID, r.ProjectID, derefFleetID(r.FleetID), r.PlayerID, r.Region, r.GameMode, r.Attributes, r.Status, r.MatchAddress, r.MatchProtocol)
		t.CreatedAt = r.CreatedAt.Time
		tickets = append(tickets, t)
	}
	return &Claim{ID: claimUUID.String(), Tickets: tickets}, nil
}

// defaultRetryHeadroom keeps a short-count release from immediately failing
// the ticket. Real failure-on-Nth-attempt accounting runs through the
// worker-driven ReleaseClaim with the operator-configured maxAttempts.
const defaultRetryHeadroom = 1 << 30

// CommitClaim flips every still-queued row holding this claim to 'matched'
// with the given address and protocol hint. Returns rows-affected so the
// caller can detect 0-row commits (claim drifted) and deallocate the orphan
// server.
func (q *PGQueue) CommitClaim(ctx context.Context, claim *Claim, matchAddress, matchProtocol string) (int64, error) {
	if claim == nil || claim.ID == "" {
		return 0, nil
	}
	pgUUID, err := parseClaimID(claim.ID)
	if err != nil {
		return 0, err
	}
	var n int64
	err = q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		n, qerr = sqlcgen.New(tx).CommitMatchmakerClaim(ctx, sqlcgen.CommitMatchmakerClaimParams{
			MatchAddress:  matchAddress,
			MatchProtocol: matchProtocol,
			ClaimID:       pgUUID,
		})
		return qerr
	})
	return n, err
}

// ReleaseClaim clears the claim, bumps allocation_attempts, and flips to
// 'failed' on the Nth attempt.
func (q *PGQueue) ReleaseClaim(ctx context.Context, claim *Claim, maxAttempts int) error {
	if claim == nil || claim.ID == "" {
		return nil
	}
	pgUUID, err := parseClaimID(claim.ID)
	if err != nil {
		return err
	}
	return q.releaseByClaimID(ctx, pgUUID, maxAttempts)
}

func (q *PGQueue) releaseByClaimID(ctx context.Context, pgUUID pgtype.UUID, maxAttempts int) error {
	return q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).ReleaseMatchmakerClaim(ctx, sqlcgen.ReleaseMatchmakerClaimParams{
			MaxAttempts: int32(maxAttempts), //nolint:gosec // operator config (MaxAttempts), validated > 0 by NewWorker
			ClaimID:     pgUUID,
		})
		return qerr
	})
}

// SweepStaleClaims releases every claim whose lease has expired.
func (q *PGQueue) SweepStaleClaims(ctx context.Context, maxAttempts int) (int64, error) {
	var n int64
	err := q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		n, qerr = sqlcgen.New(tx).SweepStaleMatchmakerClaims(ctx, int32(maxAttempts)) //nolint:gosec // operator config (MaxAttempts), validated > 0 by NewWorker
		return qerr
	})
	return n, err
}

// Listen subscribes to the matchmaker_ticket NOTIFY channel and dispatches
// each bucket payload to fn. Returns nil on ctx.Done() and a wrapped error
// if the underlying connection drops — the worker reconnects with backoff
// and the fallback ticker covers any gap.
func (q *PGQueue) Listen(ctx context.Context, fn func(Bucket)) error {
	return q.pool.ListenChannel(ctx, notifyChannel, func(payload string) {
		var p struct {
			TenantID  int64  `json:"tenant_id"`
			ProjectID int64  `json:"project_id"`
			FleetID   int64  `json:"fleet_id"`
			Region    string `json:"region"`
			GameMode  string `json:"game_mode"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			slog.WarnContext(ctx, "matchmaker: malformed notify payload", "err", err, "payload", payload)
			return
		}
		fn(Bucket{TenantID: p.TenantID, ProjectID: p.ProjectID, FleetID: p.FleetID, Region: p.Region, GameMode: p.GameMode})
	})
}

func rowToTicket(id, tenantID, projectID, fleetID, playerID int64, region, gameMode string, attrs []byte, status, matchAddress, matchProtocol string) *Ticket {
	return &Ticket{
		ID:            id,
		TenantID:      tenantID,
		ProjectID:     projectID,
		FleetID:       fleetID,
		PlayerID:      playerID,
		Region:        region,
		GameMode:      gameMode,
		Attributes:    attrs,
		Status:        Status(status),
		MatchAddress:  matchAddress,
		MatchProtocol: matchProtocol,
	}
}

func parseClaimID(s string) (pgtype.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("matchmaker: parse claim id %q: %w", s, err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}
