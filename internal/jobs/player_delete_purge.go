package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// PlayerDeletePurgeKind is the River job kind for the player-deletion purge
// sweep.
const PlayerDeletePurgeKind = "player_delete_purge"

// playerDeletePurgeBatch bounds one delete transaction; a tenant with more
// due rows drains across several batches within the same sweep.
const playerDeletePurgeBatch = 100

// PlayerDeletePurgeArgs is the (argument-less) periodic purge job. River
// schedules it on the elected leader, so it runs once across the fleet.
type PlayerDeletePurgeArgs struct{}

// Kind implements river.JobArgs.
func (PlayerDeletePurgeArgs) Kind() string { return PlayerDeletePurgeKind }

// PlayerDeletePurgeWorker hard-deletes players whose delete request has
// passed the grace window.
type PlayerDeletePurgeWorker struct {
	river.WorkerDefaults[PlayerDeletePurgeArgs]
	pool  *db.Pool
	grace time.Duration
}

// NewPlayerDeletePurgeWorker returns a worker bound to the app pool and the
// configured grace period.
func NewPlayerDeletePurgeWorker(pool *db.Pool, grace time.Duration) *PlayerDeletePurgeWorker {
	return &PlayerDeletePurgeWorker{pool: pool, grace: grace}
}

// Work implements river.Worker.
func (w *PlayerDeletePurgeWorker) Work(ctx context.Context, _ *river.Job[PlayerDeletePurgeArgs]) error {
	return SweepDuePlayerDeletes(ctx, w.pool, w.grace, time.Now())
}

// SweepDuePlayerDeletes hard-deletes every player whose delete_requested_at
// lies at least one grace period in the past. FK cascades take the player's
// per-project data with the row; audit_log rows survive with a NULL actor.
// Each batch also writes one player.purge service audit row per player and
// releases the quota slots the deleted rows held (soft-deleted rows never
// held one).
//
// Tenants are listed via BootstrapQ (top-level scope, no RLS context); the
// deletes run inside each tenant's own RLS-scoped transaction because
// project_players FORCEs row level security and the app role has no
// BYPASSRLS. The FOR UPDATE in ListPlayersDueForPurge serializes against a
// racing cancel: the cancel lands first or reports "no pending deletion".
func SweepDuePlayerDeletes(ctx context.Context, pool *db.Pool, grace time.Duration, now time.Time) error {
	var tenantIDs []int64
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT id FROM tenants ORDER BY id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			tenantIDs = append(tenantIDs, id)
		}
		return rows.Err()
	}); err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}

	cutoff := pgtype.Timestamptz{Time: now.Add(-grace), Valid: true}
	var purged, failed int64
	for _, tenantID := range tenantIDs {
		tctx := db.WithTenant(ctx, tenantID)
		n, err := purgeDuePlayersForTenant(tctx, pool, cutoff)
		if err != nil {
			slog.WarnContext(ctx, "player delete purge: tenant failed", "err", err, "tenant_id", tenantID)
			failed++
			continue
		}
		purged += n
	}
	if purged > 0 {
		slog.InfoContext(ctx, "player delete purge", "players_purged", purged)
	}
	if failed > 0 {
		return fmt.Errorf("player delete purge: %d tenant(s) failed", failed)
	}
	return nil
}

func purgeDuePlayersForTenant(ctx context.Context, pool *db.Pool, cutoff pgtype.Timestamptz) (int64, error) {
	var purged int64
	for {
		var batch int64
		err := pool.Q(ctx, func(tx pgx.Tx) error {
			var qerr error
			batch, qerr = purgeOneBatch(ctx, tx, cutoff)
			return qerr
		})
		if err != nil {
			return purged, err
		}
		purged += batch
		if batch < playerDeletePurgeBatch {
			return purged, nil
		}
	}
}

func purgeOneBatch(ctx context.Context, tx pgx.Tx, cutoff pgtype.Timestamptz) (int64, error) {
	q := sqlcgen.New(tx)
	due, err := q.ListPlayersDueForPurge(ctx, sqlcgen.ListPlayersDueForPurgeParams{
		Cutoff: cutoff,
		Batch:  playerDeletePurgeBatch,
	})
	if err != nil || len(due) == 0 {
		return 0, err
	}

	ids := make([]int64, 0, len(due))
	var slotsHeld int64
	for _, p := range due {
		ids = append(ids, p.ID)
		if !p.DeletedAt.Valid {
			slotsHeld++
		}
		payload := map[string]any{
			"project_id":          p.ProjectID,
			"external_id":         p.ExternalID,
			"delete_requested_at": p.DeleteRequestedAt.Time,
		}
		if err := auditlog.WriteService(ctx, tx, "player_delete_purge", "player.purge",
			fmt.Sprintf("%d", p.ID), payload); err != nil {
			return 0, err
		}
	}
	// Signals carry no player FKs and survive the row cascade when the
	// session's host is someone else — remove them explicitly.
	if err := q.DeletePlayerGameSessionSignals(ctx, ids); err != nil {
		return 0, err
	}
	if _, err := q.HardDeletePlayers(ctx, ids); err != nil {
		return 0, err
	}
	if slotsHeld > 0 {
		if err := q.ReleaseTenantPlayerSlots(ctx, slotsHeld); err != nil {
			return 0, err
		}
	}
	return int64(len(due)), nil
}
