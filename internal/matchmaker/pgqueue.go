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
	"github.com/ggscale/ggscale/internal/fleet"
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
// authenticated context (tenant_id is read via RLS). The MaxActive cap is
// checked in the same transaction as the insert.
func (q *PGQueue) Enqueue(ctx context.Context, req EnqueueRequest) (*Ticket, error) {
	req.normalize()
	attrs := req.Attributes
	if len(attrs) == 0 {
		attrs = []byte("{}")
	}
	stringProps, err := marshalProps(req.StringProperties)
	if err != nil {
		return nil, fmt.Errorf("matchmaker: enqueue: %w", err)
	}
	numericProps, err := marshalProps(req.NumericProperties)
	if err != nil {
		return nil, fmt.Errorf("matchmaker: enqueue: %w", err)
	}
	var fleetID *int64
	if req.FleetID != 0 {
		fleetID = &req.FleetID
	}
	var expiresAt pgtype.Timestamptz
	if req.ExpiresAt != nil {
		expiresAt = pgtype.Timestamptz{Time: *req.ExpiresAt, Valid: true}
	}
	var ticket *Ticket
	err = q.pool.Q(ctx, func(tx pgx.Tx) error {
		if req.MaxActive > 0 {
			n, cerr := sqlcgen.New(tx).CountQueuedTicketsForPlayer(ctx, sqlcgen.CountQueuedTicketsForPlayerParams{
				ProjectID: req.ProjectID,
				PlayerID:  req.PlayerID,
			})
			if cerr != nil {
				return cerr
			}
			if n >= int64(req.MaxActive) {
				return ErrTicketLimit
			}
		}
		row, qerr := sqlcgen.New(tx).InsertMatchmakingTicket(ctx, sqlcgen.InsertMatchmakingTicketParams{
			ProjectID:         req.ProjectID,
			FleetID:           fleetID,
			PlayerID:          req.PlayerID,
			Region:            req.Region,
			GameMode:          req.GameMode,
			Attributes:        attrs,
			Mode:              string(req.Mode),
			MinCount:          int32(req.MinCount),      //nolint:gosec // bounded ≤ math.MaxInt32 by normalizeTicketCounts
			MaxCount:          int32(req.MaxCount),      //nolint:gosec // bounded ≤ math.MaxInt32 by normalizeTicketCounts
			CountMultiple:     int32(req.CountMultiple), //nolint:gosec // bounded ≤ math.MaxInt32 by normalizeTicketCounts
			AllowCrossRegion:  req.AllowCrossRegion,
			Query:             req.Query,
			StringProperties:  stringProps,
			NumericProperties: numericProps,
			ExpiresAt:         expiresAt,
		})
		if qerr != nil {
			return qerr
		}
		ticket = &Ticket{
			ID:                row.ID,
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
			Status:            Status(row.Status),
			CreatedAt:         row.CreatedAt.Time,
			ExpiresAt:         req.ExpiresAt,
		}
		return nil
	})
	if errors.Is(err, ErrTicketLimit) {
		return nil, ErrTicketLimit
	}
	if err != nil {
		return nil, fmt.Errorf("matchmaker: enqueue: %w", err)
	}
	return ticket, nil
}

// marshalProps encodes a property map as JSONB, defaulting to {}.
func marshalProps[V any](m map[string]V) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// unmarshalProps decodes a JSONB property column, tolerating empty input.
func unmarshalProps[V any](raw []byte) (map[string]V, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]V
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
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
		stringProps, perr := unmarshalProps[string](row.StringProperties)
		if perr != nil {
			return perr
		}
		numericProps, perr := unmarshalProps[float64](row.NumericProperties)
		if perr != nil {
			return perr
		}
		t = &Ticket{
			ID:                row.ID,
			TenantID:          row.TenantID,
			ProjectID:         row.ProjectID,
			FleetID:           derefFleetID(row.FleetID),
			PlayerID:          row.PlayerID,
			Mode:              Mode(row.Mode),
			Region:            row.Region,
			GameMode:          row.GameMode,
			Attributes:        row.Attributes,
			MinCount:          int(row.MinCount),
			MaxCount:          int(row.MaxCount),
			CountMultiple:     int(row.CountMultiple),
			AllowCrossRegion:  row.AllowCrossRegion,
			Query:             row.Query,
			StringProperties:  stringProps,
			NumericProperties: numericProps,
			Status:            Status(row.Status),
			MatchID:           row.MatchID,
			MatchAddress:      row.MatchAddress,
			MatchProtocol:     row.MatchProtocol,
			CreatedAt:         row.CreatedAt.Time,
		}
		if row.MatchedAt.Valid {
			v := row.MatchedAt.Time
			t.MatchedAt = &v
		}
		if row.ExpiresAt.Valid {
			v := row.ExpiresAt.Time
			t.ExpiresAt = &v
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
// holding unclaimed queued tickets.
func (q *PGQueue) ListReadyBuckets(ctx context.Context) ([]Bucket, error) {
	var out []Bucket
	err := q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).ListReadyMatchmakerBuckets(ctx)
		if qerr != nil {
			return qerr
		}
		for _, r := range rows {
			out = append(out, Bucket{
				TenantID:  r.TenantID,
				ProjectID: r.ProjectID,
				Mode:      Mode(r.Mode),
				FleetID:   derefFleetID(r.FleetID),
				Region:    r.Region,
				GameMode:  r.GameMode,
			})
		}
		return nil
	})
	return out, err
}

// ClaimBucket stakes a UUID-keyed claim on up to max unclaimed queued
// tickets, oldest first. Status stays 'queued'; only claim_id / claimed_at /
// claim_expires_at are set. Returns nil when nothing was claimable.
func (q *PGQueue) ClaimBucket(ctx context.Context, bucket Bucket, max int, ttl time.Duration) (*Claim, error) {
	claimUUID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("matchmaker: generate claim uuid: %w", err)
	}
	pgUUID := pgtype.UUID{Bytes: claimUUID, Valid: true}
	pgTTL := pgtype.Interval{Microseconds: ttl.Microseconds(), Valid: true}

	var fleetID *int64
	if bucket.FleetID != 0 {
		f := bucket.FleetID
		fleetID = &f
	}
	var rows []sqlcgen.ClaimMatchmakerBucketRow
	err = q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		r, qerr := sqlcgen.New(tx).ClaimMatchmakerBucket(ctx, sqlcgen.ClaimMatchmakerBucketParams{
			TenantID:  bucket.TenantID,
			ProjectID: bucket.ProjectID,
			Mode:      string(bucket.Mode),
			FleetID:   fleetID,
			Region:    bucket.Region,
			GameMode:  bucket.GameMode,
			ClaimID:   pgUUID,
			Ttl:       pgTTL,
			Limit:     int32(max), //nolint:gosec // worker batch cap, small constant
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
	if len(rows) == 0 {
		// Another worker won the race; the next tick or notify retries.
		return nil, nil
	}
	tickets := make([]*Ticket, 0, len(rows))
	for _, r := range rows {
		stringProps, perr := unmarshalProps[string](r.StringProperties)
		if perr != nil {
			return nil, perr
		}
		numericProps, perr := unmarshalProps[float64](r.NumericProperties)
		if perr != nil {
			return nil, perr
		}
		tickets = append(tickets, &Ticket{
			ID:                r.ID,
			TenantID:          r.TenantID,
			ProjectID:         r.ProjectID,
			FleetID:           derefFleetID(r.FleetID),
			PlayerID:          r.PlayerID,
			Mode:              Mode(r.Mode),
			Region:            r.Region,
			GameMode:          r.GameMode,
			Attributes:        r.Attributes,
			MinCount:          int(r.MinCount),
			MaxCount:          int(r.MaxCount),
			CountMultiple:     int(r.CountMultiple),
			AllowCrossRegion:  r.AllowCrossRegion,
			Query:             r.Query,
			StringProperties:  stringProps,
			NumericProperties: numericProps,
			Status:            Status(r.Status),
			MatchAddress:      r.MatchAddress,
			MatchProtocol:     r.MatchProtocol,
			CreatedAt:         r.CreatedAt.Time,
		})
	}
	return &Claim{ID: claimUUID.String(), Tickets: tickets}, nil
}

// CommitTickets flips the given still-queued claim tickets to 'matched'
// with the given match id, address and protocol hint. Returns rows-affected
// so the caller can detect 0-row commits (claim drifted) and deallocate the
// orphan server.
func (q *PGQueue) CommitTickets(ctx context.Context, claim *Claim, ticketIDs []int64, matchID, matchAddress, matchProtocol string) (int64, error) {
	if claim == nil || claim.ID == "" || len(ticketIDs) == 0 {
		return 0, nil
	}
	pgUUID, err := parseClaimID(claim.ID)
	if err != nil {
		return 0, err
	}
	var n int64
	err = q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		n, qerr = sqlcgen.New(tx).CommitMatchmakerTickets(ctx, sqlcgen.CommitMatchmakerTicketsParams{
			MatchID:       matchID,
			MatchAddress:  matchAddress,
			MatchProtocol: matchProtocol,
			ClaimID:       pgUUID,
			TicketIds:     ticketIDs,
		})
		return qerr
	})
	return n, err
}

// ReleaseTickets clears the claim on the given tickets, bumps
// allocation_attempts, and flips to 'failed' on the Nth attempt.
func (q *PGQueue) ReleaseTickets(ctx context.Context, claim *Claim, ticketIDs []int64, maxAttempts int) error {
	if claim == nil || claim.ID == "" || len(ticketIDs) == 0 {
		return nil
	}
	pgUUID, err := parseClaimID(claim.ID)
	if err != nil {
		return err
	}
	return q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).ReleaseMatchmakerTickets(ctx, sqlcgen.ReleaseMatchmakerTicketsParams{
			MaxAttempts: int32(maxAttempts), //nolint:gosec // operator config (MaxAttempts), validated > 0 by NewWorker
			ClaimID:     pgUUID,
			TicketIds:   ticketIDs,
		})
		return qerr
	})
}

// ReturnUnmatched un-claims whatever the claim still holds without touching
// allocation_attempts.
func (q *PGQueue) ReturnUnmatched(ctx context.Context, claim *Claim) error {
	if claim == nil || claim.ID == "" {
		return nil
	}
	pgUUID, err := parseClaimID(claim.ID)
	if err != nil {
		return err
	}
	return q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).ReturnMatchmakerClaim(ctx, pgUUID)
		return qerr
	})
}

// SweepStaleClaims releases every claim whose lease has expired and fails
// unclaimed tickets past their TTL.
func (q *PGQueue) SweepStaleClaims(ctx context.Context, maxAttempts int) (int64, error) {
	var n int64
	err := q.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		n, qerr = sqlcgen.New(tx).SweepStaleMatchmakerClaims(ctx, int32(maxAttempts)) //nolint:gosec // operator config (MaxAttempts), validated > 0 by NewWorker
		if qerr != nil {
			return qerr
		}
		expired, qerr := sqlcgen.New(tx).ExpireMatchmakerTickets(ctx)
		n += expired
		return qerr
	})
	return n, err
}

// CreateMatch persists a committed match result under the tenant on ctx.
func (q *PGQueue) CreateMatch(ctx context.Context, m *Match) error {
	roster, err := json.Marshal(m.Roster)
	if err != nil {
		return fmt.Errorf("matchmaker: marshal roster: %w", err)
	}
	var fleetID *int64
	if m.FleetID != 0 {
		f := m.FleetID
		fleetID = &f
	}
	var allocationID *int64
	if m.AllocationID != 0 {
		id := int64(m.AllocationID)
		allocationID = &id
	}
	claimedAt := pgtype.Timestamptz{}
	if !m.ClaimedAt.IsZero() {
		claimedAt = pgtype.Timestamptz{Time: m.ClaimedAt, Valid: true}
	}
	return q.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).InsertMatchmakerMatch(ctx, sqlcgen.InsertMatchmakerMatchParams{
			ID:           m.ID,
			ProjectID:    m.ProjectID,
			Mode:         string(m.Mode),
			FleetID:      fleetID,
			Address:      m.Address,
			Protocol:     m.Protocol,
			SessionID:    m.SessionID,
			JoinCode:     m.JoinCode,
			AllocationID: allocationID,
			ClaimedAt:    claimedAt,
			Roster:       roster,
			ExpiresAt:    pgtype.Timestamptz{Time: m.ExpiresAt, Valid: true},
		})
	})
}

// GetMatch returns the match by id for the tenant on ctx.
func (q *PGQueue) GetMatch(ctx context.Context, id string) (*Match, error) {
	var m *Match
	err := q.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetMatchmakerMatch(ctx, id)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if qerr != nil {
			return qerr
		}
		m, qerr = decodeMatchmakerMatch(row)
		return qerr
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ClaimMatch atomically claims and returns an unexpired match for polling or
// successful realtime delivery.
func (q *PGQueue) ClaimMatch(ctx context.Context, id string) (*Match, error) {
	var m *Match
	err := q.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).ClaimMatchmakerMatch(ctx, id)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if qerr != nil {
			return qerr
		}
		m, qerr = decodeMatchmakerMatch(row)
		return qerr
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func decodeMatchmakerMatch(row sqlcgen.MatchmakerMatch) (*Match, error) {
	var roster []RosterEntry
	if err := json.Unmarshal(row.Roster, &roster); err != nil {
		return nil, fmt.Errorf("matchmaker: unmarshal roster: %w", err)
	}
	return &Match{
		ID:           row.ID,
		TenantID:     row.TenantID,
		ProjectID:    row.ProjectID,
		Mode:         Mode(row.Mode),
		FleetID:      derefFleetID(row.FleetID),
		Address:      row.Address,
		Protocol:     row.Protocol,
		SessionID:    row.SessionID,
		JoinCode:     row.JoinCode,
		AllocationID: fleet.AllocationID(derefFleetID(row.AllocationID)),
		ClaimedAt:    row.ClaimedAt.Time,
		Roster:       roster,
		CreatedAt:    row.CreatedAt.Time,
		ExpiresAt:    row.ExpiresAt.Time,
	}, nil
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
			Mode      string `json:"mode"`
			FleetID   *int64 `json:"fleet_id"`
			Region    string `json:"region"`
			GameMode  string `json:"game_mode"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			slog.WarnContext(ctx, "matchmaker: malformed notify payload", "err", err, "payload", payload)
			return
		}
		// Region is a bucket dimension only for fleet_allocation — blank
		// it for other modes so notify buckets match ListReadyBuckets.
		region := p.Region
		if Mode(p.Mode) != ModeFleetAllocation {
			region = ""
		}
		fn(Bucket{TenantID: p.TenantID, ProjectID: p.ProjectID, Mode: Mode(p.Mode), FleetID: derefFleetID(p.FleetID), Region: region, GameMode: p.GameMode})
	})
}

func parseClaimID(s string) (pgtype.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("matchmaker: parse claim id %q: %w", s, err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}
