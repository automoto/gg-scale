//go:build integration

// e2e:bucket b

package migrate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/migrate"
)

// 0053 marks old base rows with source 'base'. A base row is revision 1 with
// no actor that shares created_at with revision 2. A create by a user who was
// deleted later also has no actor, but must keep its source.
func TestSettingsRevisionsBase_backfill_marks_only_base_rows(t *testing.T) {
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())
	db := openDB(t, dsn)
	execMigrationFile(t, db, "0053_settings_revisions_base_source.down.sql")

	var tenantID, projectID int64
	require.NoError(t, db.QueryRow(`INSERT INTO tenants (name) VALUES ('rev') RETURNING id`).Scan(&tenantID))
	require.NoError(t, db.QueryRow(`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`, tenantID).Scan(&projectID))
	_, err = db.Exec(`
		INSERT INTO settings_revisions (tenant_id, project_id, resource_kind, resource_id, revision, snapshot, source, created_at) VALUES
		  ($1, $2, 'remote_config', $2, 1, '{}', 'mcp',       '2026-10-01 10:00:00+00'),
		  ($1, $2, 'remote_config', $2, 2, '{}', 'mcp',       '2026-10-01 10:00:00+00'),
		  ($1, $2, 'leaderboard',   99, 1, '{}', 'dashboard', '2026-10-01 10:00:00+00'),
		  ($1, $2, 'leaderboard',   99, 2, '{}', 'dashboard', '2026-10-01 11:00:00+00')`,
		tenantID, projectID)
	require.NoError(t, err)

	execMigrationFile(t, db, "0053_settings_revisions_base_source.up.sql")

	rows, err := db.Query(`SELECT resource_kind, revision, source FROM settings_revisions ORDER BY resource_kind DESC, revision`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var kind, source string
		var rev int
		require.NoError(t, rows.Scan(&kind, &rev, &source))
		got = append(got, kind+":"+source)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"remote_config:base", "remote_config:mcp",
		"leaderboard:dashboard", "leaderboard:dashboard",
	}, got)
}
