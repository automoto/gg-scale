//go:build integration

package migrate_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/migrate"
)

func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx,
		"postgres:17",
		tcpostgres.WithDatabase("ggscale_test"),
		tcpostgres.WithUsername("ggscale"),
		tcpostgres.WithPassword("ggscale"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(shutdownCtx)
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return dsn
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "db", "migrations"))
	require.NoError(t, err)
	return abs
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func extensionCount(t *testing.T, dsn string) int {
	t.Helper()
	db := openDB(t, dsn)

	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pg_extension WHERE extname IN ('pgcrypto','citext')`).Scan(&n)
	require.NoError(t, err)
	return n
}

func tableExists(t *testing.T, dsn, name string) bool {
	t.Helper()
	db := openDB(t, dsn)

	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		)`, name).Scan(&exists)
	require.NoError(t, err)
	return exists
}

func policyCount(t *testing.T, dsn, table string) int {
	t.Helper()
	db := openDB(t, dsn)

	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pg_policies WHERE schemaname = 'public' AND tablename = $1`, table).Scan(&n)
	require.NoError(t, err)
	return n
}

func TestUp_applies_all_pending_migrations(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Up())

	assert.Equal(t, 2, extensionCount(t, dsn))
}

func TestUp_is_idempotent(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Up())
	assert.NoError(t, r.Up())
}

func TestDown_reverses_last_migration(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Up())
	require.NoError(t, r.Down())

	assert.Equal(t, 0, extensionCount(t, dsn))
}

func TestVersion_reports_current_schema_version(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Up())

	v, dirty, err := r.Version()
	require.NoError(t, err)
	assert.Equal(t, uint(17), v)
	assert.False(t, dirty)
}

func TestUp_creates_all_phase1_tables(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Up())

	for _, table := range []string{
		"tenants", "projects", "api_keys",
		"end_users", "sessions",
		"storage_objects",
		"leaderboards", "leaderboard_entries",
		"friend_edges",
		"audit_log",
		"usage_samples",
		"dashboard_users", "dashboard_memberships", "dashboard_sessions",
	} {
		assert.True(t, tableExists(t, dsn, table), "table %s should exist", table)
	}
}

func TestUp_enables_rls_with_isolation_policy(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Up())

	for _, table := range []string{
		"tenants", "projects", "api_keys",
		"end_users", "sessions",
		"storage_objects",
		"leaderboards", "leaderboard_entries",
		"friend_edges",
		"audit_log",
		"usage_samples",
	} {
		assert.GreaterOrEqual(t, policyCount(t, dsn, table), 1, "%s should have an RLS policy", table)
	}
}

func TestDown_walks_back_through_every_migration(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Up())

	for v := uint(17); v > 0; v-- {
		require.NoError(t, r.Down(), "down from version %d", v)
	}

	v, dirty, err := r.Version()
	require.NoError(t, err)
	assert.Equal(t, uint(0), v)
	assert.False(t, dirty)
	assert.False(t, tableExists(t, dsn, "tenants"))
}

func TestUp_creates_monthly_partitions_for_usage_samples(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Up())

	db := openDB(t, dsn)
	var n int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhparent
		WHERE c.relname = 'usage_samples'`).Scan(&n)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 12, "should auto-create at least 12 monthly partitions")
}
