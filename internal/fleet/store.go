package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

func rowToAllocation(row sqlcgen.GetAllocationRow) (*Allocation, error) {
	status, err := ParseStatus(row.Status)
	if err != nil {
		return nil, err
	}
	labels, err := decodeLabels(row.Metadata)
	if err != nil {
		return nil, err
	}
	return &Allocation{
		ID:         AllocationID(row.ID),
		TenantID:   row.TenantID,
		ProjectID:  row.ProjectID,
		Backend:    row.Backend,
		BackendRef: row.BackendRef,
		Region:     row.Region,
		Address:    row.Address,
		Status:     status,
		Metadata:   labels,
	}, nil
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
