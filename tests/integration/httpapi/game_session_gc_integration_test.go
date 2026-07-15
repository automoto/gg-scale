//go:build integration

package httpapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/jobs"
)

// seedGCFixtures inserts one expired + one live session and invite for a tenant
// and returns the tenant id. Uses the bootstrap (superuser) pool, bypassing RLS.
func seedGCFixtures(t *testing.T, c *cluster, token string) int64 {
	t.Helper()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, token)
	ctx := context.Background()

	var hostID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1,$2,$3) RETURNING id`,
		tenantID, projectID, "gc_host_"+token).Scan(&hostID))

	_, err := c.bootstrapPool.Exec(ctx,
		`INSERT INTO game_session (id, join_code, tenant_id, project_id, host_player_id, state, props, max_players, expires_at) VALUES
		   ($4||'_exp',  $5||'X', $1, $2, $3, 'open', '{}', 2, now() - interval '1 hour'),
		   ($4||'_live', $5||'L', $1, $2, $3, 'open', '{}', 2, now() + interval '1 hour')`,
		tenantID, projectID, hostID, "gs_"+token, "JC"+token)
	require.NoError(t, err)

	_, err = c.bootstrapPool.Exec(ctx,
		`INSERT INTO game_invite (tenant_id, project_id, from_player_id, to_player_id, session_id, join_code, expires_at) VALUES
		   ($1, $2, $3, $3, $4||'_live', $5||'L', now() - interval '1 minute'),
		   ($1, $2, $3, $3, $4||'_live', $5||'L', now() + interval '1 minute')`,
		tenantID, projectID, hostID, "gs_"+token, "JC"+token)
	require.NoError(t, err)

	return tenantID
}

func countRows(t *testing.T, c *cluster, table string, tenantID int64) int {
	t.Helper()
	var n int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		"SELECT count(*) FROM "+table+" WHERE tenant_id = $1", tenantID).Scan(&n))
	return n
}

// SweepExpiredGameSessions removes only expired rows, leaving live ones.
func TestGameSessionGC_sweep_deletes_only_expired(t *testing.T) {
	c := startCluster(t)
	tenantID := seedGCFixtures(t, c, "swp")

	require.NoError(t, jobs.SweepExpiredGameSessions(context.Background(), db.NewPool(c.appPool)))

	assert.Equal(t, 1, countRows(t, c, "game_session", tenantID), "only the live session should remain")
	assert.Equal(t, 1, countRows(t, c, "game_invite", tenantID), "only the live invite should remain")
}

// End-to-end through River: validates the client wiring and, critically, that
// ggscale_app has the grants it needs on the river_* tables (migration 0055).
func TestGameSessionGC_runs_via_river(t *testing.T) {
	c := startCluster(t)
	tenantID := seedGCFixtures(t, c, "rvr")

	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewGameSessionGCWorker(db.NewPool(c.appPool)))
	client, err := river.NewClient(riverpgxv5.New(c.appPool), &river.Config{
		TestOnly: true,
		Workers:  workers,
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
	})
	require.NoError(t, err)

	events, cancel := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer cancel()

	ctx := context.Background()
	require.NoError(t, client.Start(ctx))
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = client.Stop(stopCtx)
	}()

	_, err = client.Insert(ctx, jobs.GameSessionGCArgs{}, nil)
	require.NoError(t, err)

	select {
	case ev := <-events:
		require.Equal(t, river.EventKindJobCompleted, ev.Kind,
			"GC job did not complete: state=%s errors=%v", ev.Job.State, ev.Job.Errors)
		require.Equal(t, rivertype.JobStateCompleted, ev.Job.State)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the GC job to run")
	}

	assert.Equal(t, 1, countRows(t, c, "game_session", tenantID), "river job should delete the expired session")
	assert.Equal(t, 1, countRows(t, c, "game_invite", tenantID), "river job should delete the expired invite")
}
