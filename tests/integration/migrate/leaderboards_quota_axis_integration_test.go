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

// migratedWithLeaderboardsTenant returns a database at the newest version
// and a tenant in it.
func migratedWithLeaderboardsTenant(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())
	db := openDB(t, dsn)
	var tenantID int64
	require.NoError(t, db.QueryRow(`INSERT INTO tenants (name) VALUES ('boards') RETURNING id`).Scan(&tenantID))
	return db, tenantID
}

func TestLeaderboardsQuotaAxis_should_accept_leaderboards_override(t *testing.T) {
	// Arrange
	db, tenantID := migratedWithLeaderboardsTenant(t)

	// Act
	_, err := db.Exec(`INSERT INTO tenant_quota_overrides (tenant_id, axis, "limit") VALUES ($1, 'leaderboards', 5000)`, tenantID)

	// Assert
	assert.NoError(t, err)
}

func TestLeaderboardsQuotaAxis_down_should_delete_leaderboards_overrides_and_keep_others(t *testing.T) {
	// Arrange
	db, tenantID := migratedWithLeaderboardsTenant(t)
	_, err := db.Exec(`INSERT INTO tenant_quota_overrides (tenant_id, axis, "limit")
		VALUES ($1, 'leaderboards', 5000), ($1, 'relay_allocations', 300)`, tenantID)
	require.NoError(t, err)

	// Act
	execMigrationFile(t, db, "0059_leaderboards_quota_axis.down.sql")

	// Assert
	var axes []string
	rows, err := db.Query(`SELECT axis FROM tenant_quota_overrides WHERE tenant_id = $1 ORDER BY axis`, tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		axes = append(axes, a)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"relay_allocations"}, axes)
}

func TestLeaderboardsQuotaAxis_down_should_refuse_leaderboards_override(t *testing.T) {
	// Arrange
	db, tenantID := migratedWithLeaderboardsTenant(t)
	execMigrationFile(t, db, "0059_leaderboards_quota_axis.down.sql")

	// Act
	_, err := db.Exec(`INSERT INTO tenant_quota_overrides (tenant_id, axis, "limit") VALUES ($1, 'leaderboards', 5000)`, tenantID)

	// Assert
	assert.Error(t, err)
}
