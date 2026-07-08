// Package storagelimit resolves and persists per-tenant and per-project
// overrides for the maximum storage-object value size. The platform default is
// supplied by the caller (from config); a per-project override wins over a
// per-tenant override, which wins over the default.
//
// The storage_limits table is platform-global with explicit tenant filtering
// (no RLS), like rate_limit_overrides, so all access goes through BootstrapQ.
package storagelimit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
)

// ErrUnavailable is returned by writes when the store has no database pool.
var ErrUnavailable = errors.New("storagelimit: store unavailable")

// DefaultMaxValueBytes is the platform fallback cap (1 MiB) on a single storage
// object's value when config supplies no default (e.g. unit tests). Config
// (STORAGE_MAX_VALUE_BYTES) sets the platform default; per-tenant and
// per-project rows in storage_limits override it.
const DefaultMaxValueBytes = 1 << 20

// Store reads and writes storage-size overrides.
type Store struct {
	pool *db.Pool
}

// NewStore builds a Postgres-backed store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Resolve returns the effective max value size for (tenantID, projectID). The
// tenant override (or def when absent) is the ceiling; a per-project override
// applies but is clamped to that ceiling, so lowering or clearing the tenant
// limit re-caps stale project overrides instead of letting them outlive it. A
// nil store or missing pool resolves to def so callers can stay unconditional.
func (s *Store) Resolve(ctx context.Context, tenantID, projectID, def int64) (int64, error) {
	if s == nil || s.pool == nil {
		return def, nil
	}
	ceiling := def
	var project *int64
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		const q = `
SELECT project_id, max_value_bytes
FROM storage_limits
WHERE tenant_id = $1
  AND (project_id IS NULL OR ($2::bigint > 0 AND project_id = $2))`
		rows, err := tx.Query(ctx, q, tenantID, projectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pid *int64
			var val int64
			if err := rows.Scan(&pid, &val); err != nil {
				return err
			}
			if pid == nil {
				ceiling = val
				continue
			}
			project = &val
		}
		return rows.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("storagelimit: resolve: %w", err)
	}
	if project != nil && *project < ceiling {
		return *project, nil
	}
	return ceiling, nil
}

// Override is a persisted limit row; ProjectID is nil for the tenant-level row.
type Override struct {
	ProjectID     *int64
	MaxValueBytes int64
}

// ListForTenant returns every override for a tenant (the tenant-level row plus
// any per-project rows) for the control-panel view.
func (s *Store) ListForTenant(ctx context.Context, tenantID int64) ([]Override, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	var out []Override
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT project_id, max_value_bytes FROM storage_limits WHERE tenant_id = $1`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var o Override
			if err := rows.Scan(&o.ProjectID, &o.MaxValueBytes); err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("storagelimit: list: %w", err)
	}
	return out, nil
}

// Set upserts the limit for (tenantID, projectID); a nil projectID targets the
// tenant-level row. maxBytes <= 0 clears the override so it falls back to the
// next-broader scope. The insert path infers the (tenant_id, COALESCE(project_id,
// 0)) unique index so concurrent writers upsert atomically instead of racing a
// delete-then-insert into a duplicate-key error.
func (s *Store) Set(ctx context.Context, updatedBy, tenantID int64, projectID *int64, maxBytes int64) error {
	if s == nil || s.pool == nil {
		return ErrUnavailable
	}
	return s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		if maxBytes <= 0 {
			if _, err := tx.Exec(ctx,
				`DELETE FROM storage_limits
				 WHERE tenant_id = $1 AND COALESCE(project_id, 0) = COALESCE($2::bigint, 0)`,
				tenantID, projectID); err != nil {
				return fmt.Errorf("storagelimit: clear: %w", err)
			}
			return nil
		}
		// updated_by is a control_panel_users FK; 0 (no actor, e.g. tests/seed)
		// must be stored as NULL, not a non-existent id.
		var updatedByArg any
		if updatedBy > 0 {
			updatedByArg = updatedBy
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO storage_limits (tenant_id, project_id, max_value_bytes, updated_by)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, COALESCE(project_id, 0))
			 DO UPDATE SET max_value_bytes = EXCLUDED.max_value_bytes,
			               updated_by = EXCLUDED.updated_by,
			               updated_at = now()`,
			tenantID, projectID, maxBytes, updatedByArg); err != nil {
			return fmt.Errorf("storagelimit: set: %w", err)
		}
		return nil
	})
}
