package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// Fleet is an operator-defined template that allocations are drawn from. It
// captures the backend-specific recipe (Agones fleet name+selector labels or
// opaque plugin config) under a project-scoped name. Allocations reference a
// Fleet by id; the matchmaker API and SDK identify a Fleet by its
// project-scoped Name.
//
// Config is the per-backend payload, flattened to a string map. For agones
// the keys are "namespace" (optional override), "fleet_name", and any
// selector labels passed under "selector.<key>" entries. Plugin templates
// are free-form and pass through verbatim to the plugin RPC.
type Fleet struct {
	ID        int64
	TenantID  int64
	ProjectID int64
	Name      string
	Backend   string
	Config    map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// FleetCreate is the input shape for CreateFleet. Tenant id is read
// from the request context via RLS; passing it as a parameter would let
// callers bypass the policy. The Fleet prefix disambiguates it from
// fleet.Store, which serves allocations rather than templates.
//
//nolint:revive // intentional Fleet-prefix to disambiguate from fleet.Store.
type FleetCreate struct {
	ProjectID int64
	Name      string
	Backend   string
	Config    map[string]string
}

// FleetStore persists fleet templates. Implementations live below
// (Postgres-backed) and in tests (in-memory). Distinct from fleet.Store,
// which serves allocations.
//
//nolint:revive // intentional Fleet-prefix to disambiguate from fleet.Store.
type FleetStore interface {
	GetByID(ctx context.Context, id int64) (*Fleet, error)
	GetByName(ctx context.Context, projectID int64, name string) (*Fleet, error)
	ListForProject(ctx context.Context, projectID int64) ([]*Fleet, error)
	ListDeletedForProject(ctx context.Context, projectID int64) ([]*Fleet, error)
}

// ErrFleetNotFound is returned when a fleet id or name does not resolve to
// an active row.
var ErrFleetNotFound = errors.New("fleet: fleet template not found")

// PostgresFleetStore is the production FleetStore.
type PostgresFleetStore struct {
	pool *db.Pool
}

// NewPostgresFleetStore returns a FleetStore bound to the given pool.
func NewPostgresFleetStore(pool *db.Pool) *PostgresFleetStore {
	return &PostgresFleetStore{pool: pool}
}

// GetByID looks up a fleet by primary key. Returns ErrFleetNotFound when the
// id is unknown to the current tenant.
func (s *PostgresFleetStore) GetByID(ctx context.Context, id int64) (*Fleet, error) {
	var out *Fleet
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetFleetByID(ctx, id)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrFleetNotFound
			}
			return qerr
		}
		f, ferr := fleetRowToModel(row)
		if ferr != nil {
			return ferr
		}
		out = f
		return nil
	})
	return out, err
}

// GetByName resolves a project-scoped name to a fleet. Soft-deleted rows
// are excluded.
func (s *PostgresFleetStore) GetByName(ctx context.Context, projectID int64, name string) (*Fleet, error) {
	var out *Fleet
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetFleetByName(ctx, sqlcgen.GetFleetByNameParams{
			ProjectID: projectID,
			Name:      name,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return ErrFleetNotFound
			}
			return qerr
		}
		f, ferr := fleetRowToModel(row)
		if ferr != nil {
			return ferr
		}
		out = f
		return nil
	})
	return out, err
}

// ListForProject returns active fleet templates for a project, ordered by
// name.
func (s *PostgresFleetStore) ListForProject(ctx context.Context, projectID int64) ([]*Fleet, error) {
	var out []*Fleet
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).ListFleetsForProject(ctx, projectID)
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			f, ferr := fleetRowToModel(row)
			if ferr != nil {
				return ferr
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

// ListDeletedForProject returns the deleted fleet templates of a project,
// newest deletion first.
func (s *PostgresFleetStore) ListDeletedForProject(ctx context.Context, projectID int64) ([]*Fleet, error) {
	var out []*Fleet
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).ListDeletedFleetsForProject(ctx, projectID)
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			f, ferr := fleetRowToModel(row)
			if ferr != nil {
				return ferr
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

func fleetRowToModel(row sqlcgen.Fleet) (*Fleet, error) {
	cfg, err := decodeFleetConfig(row.Config)
	if err != nil {
		return nil, err
	}
	f := &Fleet{
		ID:        row.ID,
		TenantID:  row.TenantID,
		ProjectID: row.ProjectID,
		Name:      row.Name,
		Backend:   row.Backend,
		Config:    cfg,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	if row.DeletedAt.Valid {
		t := row.DeletedAt.Time
		f.DeletedAt = &t
	}
	return f, nil
}

func encodeFleetConfig(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("fleet: encode config: %w", err)
	}
	return b, nil
}

func decodeFleetConfig(b []byte) (map[string]string, error) {
	if len(b) == 0 {
		return map[string]string{}, nil
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("fleet: decode config: %w", err)
	}
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}
