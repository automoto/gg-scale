//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/jobs"
)

// The purge sweep hard-deletes players whose delete request passed the grace
// window, cascading over their per-project data, while pending-within-grace
// and untouched players survive. Audit rows written by the purged player stay
// with a NULL actor, the sweep writes its own player.purge service audit rows,
// and tenants.player_count drops only for rows that held a quota slot.

const purgeGrace = 720 * time.Hour

func appRolePool(t *testing.T, raw *pgxpool.Pool) *db.Pool {
	t.Helper()
	appConfig := raw.Config().Copy()
	appConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appPGX, err := pgxpool.NewWithConfig(context.Background(), appConfig)
	require.NoError(t, err)
	t.Cleanup(appPGX.Close)
	return db.NewPool(appPGX)
}

func TestSweepDuePlayerDeletes_purges_only_due_players(t *testing.T) {
	_, raw := startJobsDB(t)
	ctx := context.Background()

	_, err := raw.Exec(ctx, `
INSERT INTO tenants (id, name, player_count) VALUES (8101, 'purge-a', 3), (8102, 'purge-b', 1);
INSERT INTO projects (id, tenant_id, name) VALUES (8101, 8101, 'p-a'), (8102, 8102, 'p-b');
INSERT INTO project_players (id, tenant_id, project_id, external_id, disabled_at, delete_requested_at, deleted_at)
VALUES
    (8111, 8101, 8101, 'due',        now() - interval '31 days', now() - interval '31 days', NULL),
    (8112, 8101, 8101, 'in-grace',   now() - interval '1 day',   now() - interval '1 day',   NULL),
    (8113, 8101, 8101, 'untouched',  NULL,                       NULL,                       NULL),
    (8114, 8101, 8101, 'due-soft',   now() - interval '31 days', now() - interval '31 days', now() - interval '40 days'),
    (8121, 8102, 8102, 'due-b',      now() - interval '31 days', now() - interval '31 days', NULL)`)
	require.NoError(t, err)

	_, err = raw.Exec(ctx, `
INSERT INTO sessions (tenant_id, project_id, player_id, refresh_hash, expires_at)
VALUES (8101, 8101, 8111, '\x01', now() + interval '1 day');
INSERT INTO presence (tenant_id, player_id) VALUES (8101, 8111);
INSERT INTO leaderboards (id, tenant_id, project_id, name) VALUES (8101, 8101, 8101, 'lb');
INSERT INTO leaderboard_entries (tenant_id, leaderboard_id, player_id, period, score)
VALUES (8101, 8101, 8111, 0, 42);
INSERT INTO storage_objects (tenant_id, project_id, owner_user_id, key, value)
VALUES (8101, 8101, 8111, 'save', '{}');
INSERT INTO matchmaking_tickets (tenant_id, project_id, player_id, mode)
VALUES (8101, 8101, 8111, 'match_only');
INSERT INTO game_invite (tenant_id, project_id, from_player_id, to_player_id, session_id, join_code, expires_at)
VALUES (8101, 8101, 8111, 8112, 'sess-1', 'JOIN01', now() + interval '1 day');
INSERT INTO game_session (id, join_code, tenant_id, project_id, host_player_id, expires_at)
VALUES ('sess-1', 'JOIN01', 8101, 8101, 8111, now() + interval '1 day'),
       ('sess-2', 'JOIN02', 8101, 8101, 8112, now() + interval '1 day');
INSERT INTO game_session_peer (tenant_id, session_id, player_id) VALUES (8101, 'sess-1', 8112);
INSERT INTO game_session_signal (tenant_id, session_id, from_player_id, to_player_id, negotiation_id, kind, payload)
VALUES (8101, 'sess-2', 8111, 8112, 'n1', 'offer', 'sdp-from-doomed'),
       (8101, 'sess-2', 8112, 8111, 'n1', 'answer', 'sdp-to-doomed'),
       (8101, 'sess-2', 8112, 8113, 'n2', 'offer', 'sdp-between-survivors');
INSERT INTO audit_log (tenant_id, actor_user_id, action) VALUES (8101, 8111, 'auth.delete_request')`)
	require.NoError(t, err)

	pool := appRolePool(t, raw)
	require.NoError(t, jobs.SweepDuePlayerDeletes(ctx, pool, purgeGrace, time.Now()))

	var survivors []int64
	rows, err := raw.Query(ctx, `SELECT id FROM project_players ORDER BY id`)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		survivors = append(survivors, id)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []int64{8112, 8113}, survivors, "only in-grace and untouched players survive")

	for _, c := range []struct {
		name  string
		query string
	}{
		{"sessions", `SELECT count(*) FROM sessions WHERE player_id = 8111`},
		{"presence", `SELECT count(*) FROM presence WHERE player_id = 8111`},
		{"leaderboard_entries", `SELECT count(*) FROM leaderboard_entries WHERE player_id = 8111`},
		{"storage_objects", `SELECT count(*) FROM storage_objects WHERE owner_user_id = 8111`},
		{"matchmaking_tickets", `SELECT count(*) FROM matchmaking_tickets WHERE player_id = 8111`},
		{"game_invite", `SELECT count(*) FROM game_invite WHERE from_player_id = 8111`},
		{"hosted game_session", `SELECT count(*) FROM game_session WHERE host_player_id = 8111`},
		{"game_session_peer", `SELECT count(*) FROM game_session_peer WHERE session_id = 'sess-1'`},
		{"game_session_signal", `SELECT count(*) FROM game_session_signal WHERE from_player_id = 8111 OR to_player_id = 8111`},
	} {
		var n int64
		require.NoError(t, raw.QueryRow(ctx, c.query).Scan(&n))
		assert.Zero(t, n, "%s rows must cascade away", c.name)
	}

	// Signals between surviving players in the surviving session stay: the
	// purge removes only rows addressed to or sent by the deleted player.
	var survivorSignals int64
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM game_session_signal WHERE session_id = 'sess-2'`).Scan(&survivorSignals))
	assert.Equal(t, int64(1), survivorSignals)

	var actor *int64
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT actor_user_id FROM audit_log WHERE tenant_id = 8101 AND action = 'auth.delete_request'`).
		Scan(&actor))
	assert.Nil(t, actor, "the purged player's audit rows survive with a NULL actor")

	var purgeAuditsA, purgeAuditsB int64
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		 WHERE tenant_id = 8101 AND action = 'player.purge' AND actor_service = 'player_delete_purge'`).
		Scan(&purgeAuditsA))
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM audit_log
		 WHERE tenant_id = 8102 AND action = 'player.purge' AND actor_service = 'player_delete_purge'`).
		Scan(&purgeAuditsB))
	assert.Equal(t, int64(2), purgeAuditsA, "one purge audit per deleted player in tenant a")
	assert.Equal(t, int64(1), purgeAuditsB, "one purge audit in tenant b")

	var countA, countB int64
	require.NoError(t, raw.QueryRow(ctx, `SELECT player_count FROM tenants WHERE id = 8101`).Scan(&countA))
	require.NoError(t, raw.QueryRow(ctx, `SELECT player_count FROM tenants WHERE id = 8102`).Scan(&countB))
	assert.Equal(t, int64(2), countA, "soft-deleted rows never held a slot; only the live purge decrements")
	assert.Equal(t, int64(0), countB)
}

func TestSweepDuePlayerDeletes_drains_batches_larger_than_batch_size(t *testing.T) {
	_, raw := startJobsDB(t)
	ctx := context.Background()

	_, err := raw.Exec(ctx, `
INSERT INTO tenants (id, name, player_count) VALUES (8201, 'purge-batch', 150);
INSERT INTO projects (id, tenant_id, name) VALUES (8201, 8201, 'p-batch');
INSERT INTO project_players (id, tenant_id, project_id, external_id, disabled_at, delete_requested_at)
SELECT 8300 + n, 8201, 8201, 'bulk_' || n, now() - interval '31 days', now() - interval '31 days'
FROM generate_series(1, 150) n`)
	require.NoError(t, err)

	pool := appRolePool(t, raw)
	require.NoError(t, jobs.SweepDuePlayerDeletes(ctx, pool, purgeGrace, time.Now()))

	var left, count int64
	require.NoError(t, raw.QueryRow(ctx, `SELECT count(*) FROM project_players WHERE tenant_id = 8201`).Scan(&left))
	require.NoError(t, raw.QueryRow(ctx, `SELECT player_count FROM tenants WHERE id = 8201`).Scan(&count))
	assert.Zero(t, left, "every due player drains across batches")
	assert.Zero(t, count)
}
