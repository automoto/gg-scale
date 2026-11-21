//go:build integration

// e2e:bucket b

package migrate_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/migrate"
)

// migratedWithoutRename returns a database at the newest version, then with
// 0058 undone, so a test can write rows in the old shape.
func migratedWithoutRename(t *testing.T) *sql.DB {
	t.Helper()
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())
	db := openDB(t, dsn)
	execMigrationFile(t, db, "0058_management_tokens_rename.down.sql")
	return db
}

func TestManagementTokensRename_should_keep_token_id_and_change_source_mcp_to_token(t *testing.T) {
	// Arrange
	db := migratedWithoutRename(t)
	var tenantID, projectID, userID, tokenID int64
	require.NoError(t, db.QueryRow(`INSERT INTO tenants (name) VALUES ('rename') RETURNING id`).Scan(&tenantID))
	require.NoError(t, db.QueryRow(`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`, tenantID).Scan(&projectID))
	require.NoError(t, db.QueryRow(
		`INSERT INTO control_panel_users (email, email_verified_at) VALUES ('rename@example.com', now()) RETURNING id`).Scan(&userID))
	require.NoError(t, db.QueryRow(`
		INSERT INTO mcp_tokens (tenant_id, project_id, created_by_user_id, label, token_hash, token_hint, expires_at)
		VALUES ($1, $2, $3, 'agent', '\x01', 'ggm_abcd', now() + interval '1 day') RETURNING id`,
		tenantID, projectID, userID).Scan(&tokenID))
	_, err := db.Exec(`
		INSERT INTO settings_revisions (tenant_id, project_id, resource_kind, resource_id, revision, snapshot, mcp_token_id, source)
		VALUES ($1, $2, 'remote_config', $2, 1, '{}', $3, 'mcp')`, tenantID, projectID, tokenID)
	require.NoError(t, err)

	// Act
	execMigrationFile(t, db, "0058_management_tokens_rename.up.sql")

	// Assert
	var source string
	var gotTokenID int64
	require.NoError(t, db.QueryRow(
		`SELECT source, management_token_id FROM settings_revisions WHERE project_id = $1`, projectID).Scan(&source, &gotTokenID))
	assert.Equal(t, []any{"token", tokenID}, []any{source, gotTokenID})
}

func TestManagementTokensRename_should_leave_no_object_named_mcp(t *testing.T) {
	// Arrange
	db := migratedWithoutRename(t)

	// Act
	execMigrationFile(t, db, "0058_management_tokens_rename.up.sql")

	// Assert
	var names []string
	rows, err := db.Query(`
		SELECT relname FROM pg_class WHERE relname LIKE '%mcp%'
		UNION ALL SELECT conname FROM pg_constraint WHERE conname LIKE '%mcp%'
		UNION ALL SELECT polname FROM pg_policy WHERE polname LIKE '%mcp%'
		UNION ALL SELECT column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND column_name LIKE '%mcp%'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, names)
}

func TestManagementTokensRename_should_keep_app_grants_on_renamed_table(t *testing.T) {
	// Arrange
	db := migratedWithoutRename(t)

	// Act
	execMigrationFile(t, db, "0058_management_tokens_rename.up.sql")

	// Assert
	var privs []string
	rows, err := db.Query(`
		SELECT privilege_type FROM information_schema.role_table_grants
		WHERE grantee = 'ggscale_app' AND table_name = 'management_tokens' ORDER BY privilege_type`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		privs = append(privs, p)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"INSERT", "SELECT", "UPDATE"}, privs)
}
