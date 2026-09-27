package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/jackc/pgx/v5"
)

type matchmakerResolution struct {
	id              string
	tenant, project int64
}

// The resolution scan is privileged; allocation reads stay tenant-scoped.
// Backend cleanup runs outside transactions and retains failed work for retry.
func sweepMatchmakerResolutions(ctx context.Context, pool *db.Pool, releaser MatchmakerAllocationReleaser) error {
	var candidates []matchmakerResolution
	err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,tenant_id,project_id FROM matchmaking_resolutions WHERE expires_at<now() AND next_attempt_at<=now() ORDER BY next_attempt_at,id LIMIT 100`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c matchmakerResolution
			if err = rows.Scan(&c.id, &c.tenant, &c.project); err != nil {
				return err
			}
			candidates = append(candidates, c)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range candidates {
		tenantCtx := db.WithTenant(ctx, c.tenant)
		if err = cleanResolution(tenantCtx, pool, releaser, c); err != nil {
			errs = append(errs, err)
		}
		// Delay each retry so old failures do not starve newer resolutions.
		err = pool.Q(tenantCtx, func(tx pgx.Tx) error {
			_, err := tx.Exec(tenantCtx, `UPDATE matchmaking_resolutions SET next_attempt_at=now()+interval '5 minutes' WHERE id=$1`, c.id)
			return err
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func cleanResolution(ctx context.Context, pool *db.Pool, releaser MatchmakerAllocationReleaser, c matchmakerResolution) error {
	var allocations []int64
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM game_server_allocations WHERE project_id=$1 AND metadata->>'ggscale.dev/resolution-id'=$2 AND status IN ('pending','ready','allocated','failed')`, c.project, c.id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				return err
			}
			allocations = append(allocations, id)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if len(allocations) > 0 && releaser == nil {
		return fmt.Errorf("resolution %s needs allocation cleanup", c.id)
	}
	for _, id := range allocations {
		cleanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = releaser.Deallocate(cleanCtx, fleet.AllocationID(id))
		cancel()
		if err != nil {
			return err
		}
	}
	return pool.Q(ctx, func(tx pgx.Tx) error {
		// Keep a tombstone for delayed writes from an interrupted backend call.
		_, err := tx.Exec(ctx, `DELETE FROM matchmaking_resolutions r WHERE id=$1 AND expires_at<now()-interval '24 hours' AND NOT EXISTS(SELECT 1 FROM game_server_allocations a WHERE a.project_id=r.project_id AND a.metadata->>'ggscale.dev/resolution-id'=r.id AND a.status IN ('pending','ready','allocated','failed'))`, c.id)
		return err
	})
}
