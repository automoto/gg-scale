//go:build integration

package migrate_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/migrate"
)

// The 0042 collapse turns the append-only submission log into one row per
// (leaderboard, player, period): the best score per the board's sort order
// survives, superseded rows become the attempts count, and recorded_at keeps
// the first submission time.

func TestLeaderboardFeatures_collapse_keeps_best_row_per_sort_order(t *testing.T) {
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())

	db := openDB(t, dsn)
	execMigrationFile(t, db, "0042_leaderboard_features.down.sql")

	var tenantID, projectID, playerA, playerB int64
	require.NoError(t, db.QueryRow(
		`INSERT INTO tenants (name) VALUES ('collapse-t') RETURNING id`).Scan(&tenantID))
	require.NoError(t, db.QueryRow(
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'collapse-p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, db.QueryRow(
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'anon_a') RETURNING id`,
		tenantID, projectID).Scan(&playerA))
	require.NoError(t, db.QueryRow(
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'anon_b') RETURNING id`,
		tenantID, projectID).Scan(&playerB))

	var descBoard, ascBoard int64
	require.NoError(t, db.QueryRow(
		`INSERT INTO leaderboards (tenant_id, project_id, name, sort_order) VALUES ($1, $2, 'high', 'desc') RETURNING id`,
		tenantID, projectID).Scan(&descBoard))
	require.NoError(t, db.QueryRow(
		`INSERT INTO leaderboards (tenant_id, project_id, name, sort_order) VALUES ($1, $2, 'fast', 'asc') RETURNING id`,
		tenantID, projectID).Scan(&ascBoard))

	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	seed := func(board, player, score int64, at time.Time) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO leaderboard_entries (tenant_id, leaderboard_id, player_id, score, recorded_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			tenantID, board, player, score, at)
		require.NoError(t, err)
	}
	// Player A on the desc board: best is 300, first submission at base.
	seed(descBoard, playerA, 100, base)
	seed(descBoard, playerA, 300, base.Add(time.Minute))
	seed(descBoard, playerA, 200, base.Add(2*time.Minute))
	// Player B on the desc board: a single row must survive untouched.
	seed(descBoard, playerB, 50, base)
	// Player A on the asc board (a time trial): best is the LOWEST score.
	seed(ascBoard, playerA, 95, base)
	seed(ascBoard, playerA, 87, base.Add(time.Minute))

	execMigrationFile(t, db, "0042_leaderboard_features.up.sql")

	type row struct {
		score, attempts int64
		recordedAt      time.Time
		updatedAt       time.Time
	}
	read := func(board, player int64) row {
		t.Helper()
		var got row
		require.NoError(t, db.QueryRow(
			`SELECT score, attempts, recorded_at, updated_at FROM leaderboard_entries
			 WHERE leaderboard_id = $1 AND player_id = $2`,
			board, player).Scan(&got.score, &got.attempts, &got.recordedAt, &got.updatedAt))
		return got
	}

	descA := read(descBoard, playerA)
	assert.Equal(t, int64(300), descA.score)
	assert.Equal(t, int64(3), descA.attempts)
	assert.True(t, descA.recordedAt.Equal(base), "recorded_at keeps the first submission")
	assert.True(t, descA.updatedAt.Equal(base.Add(2*time.Minute)), "updated_at keeps the last submission")

	descB := read(descBoard, playerB)
	assert.Equal(t, int64(50), descB.score)
	assert.Equal(t, int64(1), descB.attempts)

	ascA := read(ascBoard, playerA)
	assert.Equal(t, int64(87), ascA.score, "asc board keeps the lowest score")
	assert.Equal(t, int64(2), ascA.attempts)

	var total int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM leaderboard_entries`).Scan(&total))
	assert.Equal(t, 3, total, "superseded rows are gone")

	// The upsert arbiter: a second row for the same player and period must be
	// rejected at the schema level.
	_, err = db.Exec(
		`INSERT INTO leaderboard_entries (tenant_id, leaderboard_id, player_id, score) VALUES ($1, $2, $3, 1)`,
		tenantID, descBoard, playerA)
	assert.Error(t, err, "duplicate (leaderboard, player, period) row must violate the unique index")
}

func TestLeaderboardFeatures_periods_table_has_rls_policy(t *testing.T) {
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())

	assert.True(t, tableExists(t, dsn, "leaderboard_periods"))
	assert.GreaterOrEqual(t, policyCount(t, dsn, "leaderboard_periods"), 1)
}
