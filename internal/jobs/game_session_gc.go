// Package jobs holds background jobs run via River (github.com/riverqueue/river).
package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// GameSessionGCKind is the River job kind for the expired-session/invite sweep.
const GameSessionGCKind = "game_session_gc"

// GameSessionGCArgs is the (argument-less) periodic GC job. River schedules it
// on the elected leader, so it runs once across the fleet rather than once per
// instance.
type GameSessionGCArgs struct{}

// Kind implements river.JobArgs.
func (GameSessionGCArgs) Kind() string { return GameSessionGCKind }

// GameSessionGCWorker deletes expired game_session and game_invite rows.
type GameSessionGCWorker struct {
	river.WorkerDefaults[GameSessionGCArgs]
	pool *db.Pool
}

// NewGameSessionGCWorker returns a worker bound to the app pool.
func NewGameSessionGCWorker(pool *db.Pool) *GameSessionGCWorker {
	return &GameSessionGCWorker{pool: pool}
}

// Work implements river.Worker by sweeping expired sessions and invites.
func (w *GameSessionGCWorker) Work(ctx context.Context, _ *river.Job[GameSessionGCArgs]) error {
	return SweepExpiredGameSessions(ctx, w.pool)
}

// SweepExpiredGameSessions removes expired game_session and game_invite rows for
// every tenant. It lists tenants via BootstrapQ (the tenants table is the
// top-level scope, no RLS context needed) and deletes within each tenant's RLS
// scope via pool.Q — no superuser or SECURITY DEFINER function required.
//
// The deletes are idempotent, so a returned error simply causes River to retry
// the whole sweep with backoff; a later periodic run would also catch anything
// missed.
func SweepExpiredGameSessions(ctx context.Context, pool *db.Pool) error {
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

	var sessions, invites, failed int64
	for _, tenantID := range tenantIDs {
		tctx := db.WithTenant(ctx, tenantID)
		var s, i int64
		if err := pool.Q(tctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			var qerr error
			if s, qerr = q.DeleteExpiredGameSessionsForTenant(tctx); qerr != nil {
				return qerr
			}
			i, qerr = q.DeleteExpiredGameInvitesForTenant(tctx)
			return qerr
		}); err != nil {
			slog.WarnContext(ctx, "game session GC: delete for tenant", "err", err, "tenant_id", tenantID)
			failed++
			continue
		}
		sessions += s
		invites += i
	}
	if sessions > 0 || invites > 0 {
		slog.InfoContext(ctx, "game session GC", "sessions_deleted", sessions, "invites_deleted", invites)
	}
	if failed > 0 {
		return fmt.Errorf("game session GC: %d tenant(s) failed", failed)
	}
	return nil
}
