//go:build integration

package migrate_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/migrate"
)

func execMigrationFile(t *testing.T, db execer, name string) {
	t.Helper()
	sqlBytes, err := os.ReadFile(filepath.Join(migrationsDir(t), name))
	require.NoError(t, err)
	_, err = db.Exec(string(sqlBytes))
	require.NoError(t, err, "execute migration %s", name)
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func TestBranchFollowup_tier_class_migration_maps_and_round_trips(t *testing.T) {
	// Arrange: start at the current schema, then execute the four branch downs
	// directly so legacy tier values can be populated without changing the
	// golang-migrate bookkeeping used by the runner tests.
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())

	db := openDB(t, dsn)
	for _, name := range []string{
		"0012_tenant_change_requests.down.sql",
		"0011_tenant_storage_usage.down.sql",
		"0010_tenant_enforce_quotas.down.sql",
		"0009_tenant_tier_class.down.sql",
	} {
		execMigrationFile(t, db, name)
	}

	legacy := []string{"free", "payg", "premium"}
	for _, tier := range legacy {
		_, err := db.Exec(`INSERT INTO tenants (name, tier) VALUES ($1, $2)`, "legacy-"+tier, tier)
		require.NoError(t, err)
	}
	var preservedTenant, preservedProject int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO tenants (name, tier) VALUES ('preserved-free', 'free') RETURNING id`).Scan(&preservedTenant))
	require.NoError(t, db.QueryRow(`
		INSERT INTO projects (tenant_id, name) VALUES ($1, 'preserved-project') RETURNING id`, preservedTenant).Scan(&preservedProject))

	// Act: apply the numbered tier migration.
	execMigrationFile(t, db, "0009_tenant_tier_class.up.sql")
	require.NoError(t, db.Close())
	db = openDB(t, dsn)

	// Assert: every legacy value maps as documented and the DB constraint is
	// fail-closed outside 0..3.
	for tier, want := range map[string]int16{"free": 0, "payg": 1, "premium": 2} {
		var got int16
		require.NoError(t, db.QueryRow(`SELECT tier FROM tenants WHERE name = $1`, "legacy-"+tier).Scan(&got))
		assert.Equal(t, want, got)
	}
	for _, bad := range []int{-1, 4} {
		_, err := db.Exec(`INSERT INTO tenants (name, tier) VALUES ($1, $2)`, fmt.Sprintf("bad-%d", bad), bad)
		assert.Error(t, err)
	}
	var preservedCount int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM projects WHERE id = $1 AND tenant_id = $2`,
		preservedProject, preservedTenant).Scan(&preservedCount))
	assert.Equal(t, 1, preservedCount)

	_, err = db.Exec(`INSERT INTO tenants (name, tier) VALUES ('roundtrip-tier3', 3)`)
	require.NoError(t, err)
	execMigrationFile(t, db, "0009_tenant_tier_class.down.sql")
	require.NoError(t, db.Close())
	db = openDB(t, dsn)

	var legacyTier string
	require.NoError(t, db.QueryRow(`SELECT tier FROM tenants WHERE name = 'roundtrip-tier3'`).Scan(&legacyTier))
	assert.Equal(t, "premium", legacyTier)

	for _, name := range []string{
		"0009_tenant_tier_class.up.sql",
		"0010_tenant_enforce_quotas.up.sql",
		"0011_tenant_storage_usage.up.sql",
		"0012_tenant_change_requests.up.sql",
	} {
		execMigrationFile(t, db, name)
	}
	require.NoError(t, db.Close())
	db = openDB(t, dsn)
	var roundTrip int16
	require.NoError(t, db.QueryRow(`SELECT tier FROM tenants WHERE name = 'roundtrip-tier3'`).Scan(&roundTrip))
	assert.Equal(t, int16(2), roundTrip)
	version, dirty, err := r.Version()
	require.NoError(t, err)
	assert.Equal(t, latestMigrationVersion(t), version)
	assert.False(t, dirty)
}

func TestBranchFollowup_storage_usage_migration_backfills_live_canonical_bytes(t *testing.T) {
	// Arrange: remove the branch storage table while retaining the baseline
	// storage schema, then seed live/deleted JSON in two tenants.
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())

	db := openDB(t, dsn)
	execMigrationFile(t, db, "0012_tenant_change_requests.down.sql")
	execMigrationFile(t, db, "0011_tenant_storage_usage.down.sql")

	var tenantA, tenantB, projectA, projectB, playerA, playerB int64
	require.NoError(t, db.QueryRow(`INSERT INTO tenants (name, tier) VALUES ('backfill-a', 0) RETURNING id`).Scan(&tenantA))
	require.NoError(t, db.QueryRow(`INSERT INTO tenants (name, tier) VALUES ('backfill-b', 0) RETURNING id`).Scan(&tenantB))
	require.NoError(t, db.QueryRow(`INSERT INTO projects (tenant_id, name) VALUES ($1, 'a') RETURNING id`, tenantA).Scan(&projectA))
	require.NoError(t, db.QueryRow(`INSERT INTO projects (tenant_id, name) VALUES ($1, 'b') RETURNING id`, tenantB).Scan(&projectB))
	require.NoError(t, db.QueryRow(`
		INSERT INTO project_players (tenant_id, project_id, external_id)
		VALUES ($1, $2, 'backfill-player-a') RETURNING id`, tenantA, projectA).Scan(&playerA))
	require.NoError(t, db.QueryRow(`
		INSERT INTO project_players (tenant_id, project_id, external_id)
		VALUES ($1, $2, 'backfill-player-b') RETURNING id`, tenantB, projectB).Scan(&playerB))

	fixtures := []struct {
		tenantID, projectID, playerID int64
		key, value                    string
		deleted                       bool
	}{
		{tenantA, projectA, playerA, "nested", `{"z":[1,2],"a":"value"}`, false},
		{tenantA, projectA, playerA, "unicode", `{"text":"café"}`, false},
		{tenantA, projectA, playerA, "deleted", `{"ignored":true}`, true},
		{tenantB, projectB, playerB, "empty", `{}`, false},
	}
	for _, f := range fixtures {
		_, err := db.Exec(`
			INSERT INTO storage_objects (tenant_id, project_id, owner_user_id, key, value, deleted_at)
			VALUES ($1, $2, $3, $4, $5::jsonb,
			        CASE WHEN $6 THEN now() ELSE NULL END)`,
			f.tenantID, f.projectID, f.playerID, f.key, f.value, f.deleted)
		require.NoError(t, err)
	}

	// Act.
	execMigrationFile(t, db, "0011_tenant_storage_usage.up.sql")

	// Assert: backfill equals canonical live jsonb::text bytes per tenant.
	for _, tenantID := range []int64{tenantA, tenantB} {
		var metered, actual int64
		require.NoError(t, db.QueryRow(`
			SELECT u.total_bytes,
			       COALESCE((SELECT SUM(octet_length(value::text))
			                 FROM storage_objects
			                 WHERE tenant_id = $1 AND deleted_at IS NULL), 0)
			FROM tenant_storage_usage u WHERE tenant_id = $1`, tenantID).Scan(&metered, &actual))
		assert.Equal(t, actual, metered, "tenant %d", tenantID)
	}
	var emptyTenant int64
	require.NoError(t, db.QueryRow(`INSERT INTO tenants (name, tier) VALUES ('backfill-empty', 0) RETURNING id`).Scan(&emptyTenant))
	var zero int64
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE((SELECT total_bytes FROM tenant_storage_usage WHERE tenant_id = $1), 0)`,
		emptyTenant).Scan(&zero))
	assert.Zero(t, zero)
}

func TestBranchFollowup_maintain_grant_covers_existing_and_future_tables(t *testing.T) {
	// Arrange.
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())

	db := openDB(t, dsn)
	_, err = db.Exec(`CREATE TABLE maintain_after_migration (id bigint PRIMARY KEY)`)
	require.NoError(t, err)

	// Act/Assert: 0006 grants MAINTAIN to existing tables and establishes the
	// same default privilege for tables created later by the migration owner.
	for _, table := range []string{"river_job", "maintain_after_migration"} {
		var granted bool
		require.NoError(t, db.QueryRow(
			`SELECT has_table_privilege('ggscale_app', $1, 'MAINTAIN')`, table).Scan(&granted))
		assert.True(t, granted, "ggscale_app should hold MAINTAIN on %s", table)
	}

	_, err = db.Exec(`SET ROLE ggscale_app; ANALYZE river_job; RESET ROLE`)
	assert.NoError(t, err)
	_, err = db.Exec(`SET ROLE ggscale_app; ALTER TABLE river_job ADD COLUMN forbidden bigint; RESET ROLE`)
	assert.Error(t, err, "MAINTAIN must not grant ALTER TABLE")
}
