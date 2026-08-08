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

// The reset job archives each due board's period at its scheduled boundary,
// bumps current_period, and leaves the old entries readable in place. Boards
// that are not due, unscheduled boards, and other tenants' boards advance (or
// not) independently.

func TestResetDueLeaderboardPeriods_archives_and_advances(t *testing.T) {
	_, raw := startJobsDB(t)
	ctx := context.Background()

	// now: Wednesday 2026-07-15 12:00 UTC. The overdue weekly boundary was
	// Monday 2026-07-13; the next is Monday 2026-07-20.
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	prevBoundary := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	nextBoundary := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	periodStart := prevBoundary.AddDate(0, 0, -7)

	// Parameterized statements go through the extended protocol, which only
	// takes one command per Exec — seed in separate calls.
	_, err := raw.Exec(ctx, `
INSERT INTO tenants (id, name) VALUES (7201, 'period-a'), (7202, 'period-b');
INSERT INTO projects (id, tenant_id, name) VALUES (7201, 7201, 'p-a'), (7202, 7202, 'p-b');
INSERT INTO project_players (id, tenant_id, project_id, external_id)
VALUES (7201, 7201, 7201, 'anon_a')`)
	require.NoError(t, err)
	_, err = raw.Exec(ctx, `
INSERT INTO leaderboards (id, tenant_id, project_id, name, reset_schedule, current_period, period_started_at, next_reset_at)
VALUES
    (7201, 7201, 7201, 'due',      'weekly', 1, $1, $2),
    (7202, 7201, 7201, 'not-due',  'weekly', 0, $2, $3),
    (7203, 7201, 7201, 'unsched',  'none',   0, NULL, NULL),
    (7204, 7202, 7202, 'due-b',    'daily',  4, $1, $2)`,
		periodStart, prevBoundary, nextBoundary)
	require.NoError(t, err)
	_, err = raw.Exec(ctx, `
INSERT INTO leaderboard_entries (tenant_id, leaderboard_id, player_id, period, score)
VALUES (7201, 7201, 7201, 1, 500)`)
	require.NoError(t, err)

	appConfig := raw.Config().Copy()
	appConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appPGX, err := pgxpool.NewWithConfig(ctx, appConfig)
	require.NoError(t, err)
	t.Cleanup(appPGX.Close)
	pool := db.NewPool(appPGX)

	require.NoError(t, jobs.ResetDueLeaderboardPeriods(ctx, pool, now))

	var currentPeriod int
	var startedAt, nextAt time.Time
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT current_period, period_started_at, next_reset_at FROM leaderboards WHERE id = 7201`).
		Scan(&currentPeriod, &startedAt, &nextAt))
	assert.Equal(t, 2, currentPeriod)
	assert.True(t, startedAt.Equal(prevBoundary), "new period starts at the passed boundary, got %v", startedAt)
	assert.True(t, nextAt.Equal(nextBoundary), "next reset is the following boundary, got %v", nextAt)

	var archStart, archEnd time.Time
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT started_at, ended_at FROM leaderboard_periods WHERE leaderboard_id = 7201 AND period = 1`).
		Scan(&archStart, &archEnd))
	assert.True(t, archStart.Equal(periodStart))
	assert.True(t, archEnd.Equal(prevBoundary))

	var entryPeriod int
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT period FROM leaderboard_entries WHERE leaderboard_id = 7201`).Scan(&entryPeriod))
	assert.Equal(t, 1, entryPeriod, "old entries stay under their period number")

	var notDuePeriod int
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT current_period FROM leaderboards WHERE id = 7202`).Scan(&notDuePeriod))
	assert.Zero(t, notDuePeriod, "a future boundary must not reset")
	var unschedPeriod int
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT current_period FROM leaderboards WHERE id = 7203`).Scan(&unschedPeriod))
	assert.Zero(t, unschedPeriod)

	var otherTenantPeriod int
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT current_period FROM leaderboards WHERE id = 7204`).Scan(&otherTenantPeriod))
	assert.Equal(t, 5, otherTenantPeriod, "due boards advance in every tenant")

	// A second run is a no-op: nothing is due any more.
	require.NoError(t, jobs.ResetDueLeaderboardPeriods(ctx, pool, now))
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT current_period FROM leaderboards WHERE id = 7201`).Scan(&currentPeriod))
	assert.Equal(t, 2, currentPeriod)
	var archives int
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM leaderboard_periods WHERE leaderboard_id = 7201`).Scan(&archives))
	assert.Equal(t, 1, archives)
}
