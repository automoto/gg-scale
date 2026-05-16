package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// Fleet is an operator-defined template that allocations are drawn from. It
// captures the backend-specific recipe (Docker image+port+probe, Agones
// fleet name+selector labels, or opaque plugin config) under a project-
// scoped name. Allocations reference a Fleet by id; the matchmaker API and
// SDK identify a Fleet by its project-scoped Name.
//
// Config is the per-backend payload, flattened to a string map. For docker
// the keys are "image", "port", "probe_type", "probe_path", "pull_image".
// For agones: "namespace" (optional override), "fleet_name", and any
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

// FleetCreate is the input shape for FleetStore.Create. Tenant id is read
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

// FleetUpdate is the input shape for FleetStore.Update.
//
//nolint:revive // intentional Fleet-prefix to disambiguate from fleet.Store.
type FleetUpdate struct {
	ID      int64
	Name    string
	Backend string
	Config  map[string]string
}

// FleetStore persists fleet templates. Implementations live below
// (Postgres-backed) and in tests (in-memory). Distinct from fleet.Store,
// which serves allocations.
//
//nolint:revive // intentional Fleet-prefix to disambiguate from fleet.Store.
type FleetStore interface {
	Create(ctx context.Context, in FleetCreate) (*Fleet, error)
	GetByID(ctx context.Context, id int64) (*Fleet, error)
	GetByName(ctx context.Context, projectID int64, name string) (*Fleet, error)
	ListForProject(ctx context.Context, projectID int64) ([]*Fleet, error)
	Update(ctx context.Context, in FleetUpdate) error
	SoftDelete(ctx context.Context, id int64) error
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

// Create inserts a fleet template. The tenant id is read from the context.
func (s *PostgresFleetStore) Create(ctx context.Context, in FleetCreate) (*Fleet, error) {
	cfg, err := encodeFleetConfig(in.Config)
	if err != nil {
		return nil, err
	}
	var out *Fleet
	err = s.pool.Q(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).CreateFleet(ctx, sqlcgen.CreateFleetParams{
			ProjectID: in.ProjectID,
			Name:      in.Name,
			Backend:   in.Backend,
			Config:    cfg,
		})
		if qerr != nil {
			return qerr
		}
		f, ferr := fleetRowToModel(row)
		if ferr != nil {
			return ferr
		}
		out = f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: create fleet: %w", err)
	}
	return out, nil
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

// Update overwrites the editable fields. Soft-deleted rows are not visible
// to Update (the underlying query filters deleted_at IS NULL).
func (s *PostgresFleetStore) Update(ctx context.Context, in FleetUpdate) error {
	cfg, err := encodeFleetConfig(in.Config)
	if err != nil {
		return err
	}
	return s.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).UpdateFleet(ctx, sqlcgen.UpdateFleetParams{
			ID:      in.ID,
			Name:    in.Name,
			Backend: in.Backend,
			Config:  cfg,
		})
	})
}

// SoftDelete marks the row deleted_at = now(). The unique-on-name index is
// partial on deleted_at IS NULL so the operator can recreate a fleet under
// the same name.
func (s *PostgresFleetStore) SoftDelete(ctx context.Context, id int64) error {
	return s.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).SoftDeleteFleet(ctx, id)
	})
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
