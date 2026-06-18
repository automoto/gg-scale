package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// PostgresStore is the production Store. It reads tenant_id from context via
// db.Pool.Q and never accepts a TenantID parameter directly — RLS is the
// final boundary, and bypassing it via explicit tenant args is a footgun we
// keep out of the API.
type PostgresStore struct {
	pool *db.Pool
}

// NewPostgresStore returns a Store bound to the given pool.
func NewPostgresStore(pool *db.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// InsertPending creates a row in state 'pending'. The caller is expected to
// follow up with MarkReady or MarkFailed.
func (s *PostgresStore) InsertPending(ctx context.Context, req AllocationRequest, backend string) (AllocationID, error) {
	meta, err := encodeLabels(req.Labels)
	if err != nil {
		return 0, err
	}
	var id AllocationID
	err = s.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).CreatePendingAllocation(ctx, sqlcgen.CreatePendingAllocationParams{
			ProjectID: req.ProjectID,
			FleetID:   &req.FleetID,
			Backend:   backend,
			Region:    req.Region,
			Metadata:  meta,
		})
		if qerr != nil {
			return qerr
		}
		id = AllocationID(row.ID)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("fleet: insert pending: %w", err)
	}
	return id, nil
}

// MarkReady flips the row to 'ready' and stamps backend_ref + address.
func (s *PostgresStore) MarkReady(ctx context.Context, id AllocationID, backendRef, address string) error {
	return s.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).MarkAllocationReady(ctx, sqlcgen.MarkAllocationReadyParams{
			ID:         int64(id),
			BackendRef: backendRef,
			Address:    address,
		})
	})
}

// MarkFailed is invoked once the manager has exhausted its retries.
func (s *PostgresStore) MarkFailed(ctx context.Context, id AllocationID) error {
	return s.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).MarkAllocationFailed(ctx, int64(id))
	})
}

// Release marks the row 'shutdown' and stamps released_at.
func (s *PostgresStore) Release(ctx context.Context, id AllocationID) error {
	return s.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).ReleaseAllocation(ctx, int64(id))
	})
}

// Get returns the persisted view of an allocation.
func (s *PostgresStore) Get(ctx context.Context, id AllocationID) (*Allocation, error) {
	var alloc *Allocation
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetAllocation(ctx, int64(id))
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return qerr
		}
		converted, cerr := rowToAllocation(row)
		if cerr != nil {
			return cerr
		}
		alloc = converted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return alloc, nil
}

// List returns the most recent allocations for a project. The total is the
// highest row position known from this fetch; callers that request limit+1 can
// trim the extra row and infer whether another page exists without a companion
// COUNT(*).
func (s *PostgresStore) List(ctx context.Context, projectID int64, includeTerminal bool, limit, offset int) ([]*Allocation, int64, error) {
	var (
		out   []*Allocation
		total int64
	)
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		rows, err := q.ListAllocationsForProject(ctx, sqlcgen.ListAllocationsForProjectParams{
			ProjectID:       projectID,
			IncludeTerminal: includeTerminal,
			Lim:             toInt32(limit),
			Off:             toInt32(offset),
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			converted, cerr := rowToAllocation(sqlcgen.GetAllocationRow(row))
			if cerr != nil {
				return cerr
			}
			out = append(out, converted)
		}
		total = int64(offset + len(out))
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// AppendEvent inserts one row on the per-allocation ring buffer. The trim
// trigger keeps history bounded so callers don't need to prune.
func (s *PostgresStore) AppendEvent(ctx context.Context, id AllocationID, status Status, address, errMessage string) error {
	return s.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).InsertAllocationEvent(ctx, sqlcgen.InsertAllocationEventParams{
			AllocationID: int64(id),
			Status:       sqlcgen.AllocationStatus(status.String()),
			Address:      address,
			ErrMessage:   errMessage,
		})
	})
}

// ListEvents returns the most recent ring-buffer entries (newest first).
func (s *PostgresStore) ListEvents(ctx context.Context, id AllocationID, limit int) ([]Event, error) {
	var out []Event
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListAllocationEvents(ctx, sqlcgen.ListAllocationEventsParams{
			AllocationID: int64(id),
			Lim:          toInt32(limit),
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			status, perr := ParseStatus(row.Status)
			if perr != nil {
				return perr
			}
			out = append(out, Event{
				ID:           row.ID,
				AllocationID: AllocationID(row.AllocationID),
				Status:       status,
				Address:      row.Address,
				ErrMessage:   row.ErrMessage,
				CreatedAt:    row.CreatedAt.Time,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BackendsForTenant lists distinct backends seen by this tenant and how
// many allocation rows each one owns.
func (s *PostgresStore) BackendsForTenant(ctx context.Context) ([]BackendStats, error) {
	var out []BackendStats
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListAllocationBackendsForTenant(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, BackendStats{Name: row.Backend, AllocationCount: row.AllocationCount})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func rowToAllocation(row sqlcgen.GetAllocationRow) (*Allocation, error) {
	status, err := ParseStatus(row.Status)
	if err != nil {
		return nil, err
	}
	labels, err := decodeLabels(row.Metadata)
	if err != nil {
		return nil, err
	}
	var fleetID int64
	if row.FleetID != nil {
		fleetID = *row.FleetID
	}
	return &Allocation{
		ID:         AllocationID(row.ID),
		TenantID:   row.TenantID,
		ProjectID:  row.ProjectID,
		FleetID:    fleetID,
		Backend:    row.Backend,
		BackendRef: row.BackendRef,
		Region:     row.Region,
		Address:    row.Address,
		Status:     status,
		Metadata:   labels,
	}, nil
}

// toInt32 clamps a non-negative int down to int32, capping at math.MaxInt32.
// All callers come from pagination params already validated upstream so the
// clamp is defensive — kept here to satisfy gosec G115 cleanly.
func toInt32(n int) int32 {
	if n < 0 {
		return 0
	}
	if int64(n) > int64(math.MaxInt32) {
		return math.MaxInt32
	}
	return int32(n)
}

func encodeLabels(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("fleet: encode labels: %w", err)
	}
	return b, nil
}

func decodeLabels(b []byte) (map[string]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("fleet: decode labels: %w", err)
	}
	return out, nil
}
