package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Store is the persistence dependency the Manager owns. Implementations live
// in store.go (Postgres-backed) and in tests (in-memory). The shape is
// minimal on purpose: the manager doesn't expose query helpers, so the only
// surface to maintain is the lifecycle four-pack plus Get plus a couple of
// dashboard read paths.
type Store interface {
	InsertPending(ctx context.Context, req AllocationRequest, backend string) (AllocationID, error)
	MarkReady(ctx context.Context, id AllocationID, backendRef, address string) error
	MarkFailed(ctx context.Context, id AllocationID) error
	Release(ctx context.Context, id AllocationID) error
	Get(ctx context.Context, id AllocationID) (*Allocation, error)

	// List returns the most recent allocations for a project. include=true
	// includes terminal rows (shutdown/failed). offset is 0-based.
	List(ctx context.Context, projectID int64, includeTerminal bool, limit, offset int) ([]*Allocation, int64, error)

	// AppendEvent persists one entry on the per-allocation ring buffer.
	AppendEvent(ctx context.Context, id AllocationID, status Status, address, errMessage string) error

	// ListEvents returns the most recent ring-buffer entries for an allocation.
	ListEvents(ctx context.Context, id AllocationID, limit int) ([]Event, error)

	// BackendsForTenant lists distinct backends seen in this tenant's
	// allocations, with the row count for each.
	BackendsForTenant(ctx context.Context) ([]BackendStats, error)
}

// Event is one ring-buffer entry on fleet_allocation_events.
type Event struct {
	ID           int64
	AllocationID AllocationID
	Status       Status
	Address      string
	ErrMessage   string
	CreatedAt    time.Time
}

// BackendStats is one row of Store.BackendsForTenant.
type BackendStats struct {
	Name            string
	AllocationCount int64
}

// ManagerOptions controls retry and backoff behaviour. Tests pass Clock to
// suppress sleep; production leaves it nil and gets exponential backoff.
type ManagerOptions struct {
	// Retries is the number of additional Allocate attempts after the first
	// fails. Zero means a single attempt with no retries.
	Retries int
	// Clock returns the backoff duration before retry attempt n (1-indexed).
	// nil falls back to 100ms * 2^(n-1).
	Clock func(attempt int) time.Duration
}

// Manager is the matchmaker-facing entry point to the fleet subsystem. One
// Manager binds to one backend; the host swaps backends by constructing a
// different Manager during startup.
type Manager struct {
	store   Store
	backend Backend
	opts    ManagerOptions
}

// NewManager wires a Manager around the provided store and backend. The
// backend's Name() is used as the persisted backend column on each
// allocation so operators can correlate rows with the running plugin.
func NewManager(store Store, backend Backend, opts ManagerOptions) *Manager {
	if opts.Clock == nil {
		opts.Clock = defaultBackoff
	}
	return &Manager{store: store, backend: backend, opts: opts}
}

// Allocate persists a pending row, asks the backend to bring up a server,
// and persists the result. On terminal failure the row is marked failed and
// a non-nil error is returned so the matchmaker can re-queue the ticket.
func (m *Manager) Allocate(ctx context.Context, req AllocationRequest) (*Allocation, error) {
	id, err := m.store.InsertPending(ctx, req, m.backend.Name())
	if err != nil {
		return nil, fmt.Errorf("fleet: insert pending: %w", err)
	}
	m.appendEvent(ctx, id, StatusPending, "", "")

	var lastErr error
	for attempt := 0; attempt <= m.opts.Retries; attempt++ {
		if attempt > 0 {
			if d := m.opts.Clock(attempt); d > 0 {
				select {
				case <-ctx.Done():
					_ = m.store.MarkFailed(ctx, id)
					m.appendEvent(ctx, id, StatusFailed, "", ctx.Err().Error())
					return nil, ctx.Err()
				case <-time.After(d):
				}
			}
		}
		alloc, err := m.backend.Allocate(ctx, req)
		if err == nil {
			if err := m.store.MarkReady(ctx, id, alloc.BackendRef, alloc.Address); err != nil {
				return nil, fmt.Errorf("fleet: mark ready: %w", err)
			}
			alloc.ID = id
			alloc.TenantID = req.TenantID
			alloc.ProjectID = req.ProjectID
			alloc.Backend = m.backend.Name()
			alloc.Region = req.Region
			alloc.Status = StatusReady
			m.appendEvent(ctx, id, StatusReady, alloc.Address, "")
			return alloc, nil
		}
		lastErr = err
	}

	if err := m.store.MarkFailed(ctx, id); err != nil {
		return nil, errors.Join(lastErr, fmt.Errorf("fleet: mark failed: %w", err))
	}
	m.appendEvent(ctx, id, StatusFailed, "", lastErr.Error())
	return nil, fmt.Errorf("fleet: allocate after %d attempts: %w", m.opts.Retries+1, lastErr)
}

// appendEvent is fire-and-forget — event-log failures must not block the
// allocator's hot path. The dashboard renders "no events yet" if the table is
// empty for any reason.
func (m *Manager) appendEvent(ctx context.Context, id AllocationID, status Status, address, errMsg string) {
	_ = m.store.AppendEvent(ctx, id, status, address, errMsg)
}

// Deallocate releases the backend resource and marks the row shutdown.
// Errors from the backend bubble up; the store update happens only on
// backend success so we don't lose the ability to retry shutdown.
func (m *Manager) Deallocate(ctx context.Context, id AllocationID) error {
	a, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if a.Status.IsTerminal() {
		return nil
	}
	if err := m.backend.Deallocate(ctx, id, a.BackendRef); err != nil {
		return fmt.Errorf("fleet: backend deallocate: %w", err)
	}
	if err := m.store.Release(ctx, id); err != nil {
		return err
	}
	m.appendEvent(ctx, id, StatusShutdown, a.Address, "")
	return nil
}

// Watch pipes the backend's StatusUpdate stream to the caller. It does not
// re-emit historical state — the caller should pair Watch with a Get for a
// consistent initial snapshot.
func (m *Manager) Watch(ctx context.Context, id AllocationID) (<-chan StatusUpdate, error) {
	a, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.backend.Watch(ctx, id, a.BackendRef)
}

// Get returns the persisted view of an allocation.
func (m *Manager) Get(ctx context.Context, id AllocationID) (*Allocation, error) {
	return m.store.Get(ctx, id)
}

// List returns the most recent allocations for a project. Pagination is
// 0-based; total is the row count for the same filter (so callers can render
// "N of M").
func (m *Manager) List(ctx context.Context, projectID int64, includeTerminal bool, limit, offset int) ([]*Allocation, int64, error) {
	return m.store.List(ctx, projectID, includeTerminal, limit, offset)
}

// ListEvents returns the most recent ring-buffer entries for an allocation.
func (m *Manager) ListEvents(ctx context.Context, id AllocationID, limit int) ([]Event, error) {
	return m.store.ListEvents(ctx, id, limit)
}

// BackendsForTenant lists backends used by the current tenant's allocations
// alongside the running count.
func (m *Manager) BackendsForTenant(ctx context.Context) ([]BackendStats, error) {
	return m.store.BackendsForTenant(ctx)
}

// Backend returns the in-process backend the manager is bound to. The dashboard
// uses this for live health probes (HealthCheck) and to surface the configured
// backend name on the backends page.
func (m *Manager) Backend() Backend {
	return m.backend
}

func defaultBackoff(attempt int) time.Duration {
	d := 100 * time.Millisecond
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	return d
}
