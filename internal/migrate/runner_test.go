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

func extensionCount(t *testing.T, dsn string) int {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM pg_extension WHERE extname IN ('pgcrypto','citext')`).Scan(&n)
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
	assert.Equal(t, uint(1), v)
	assert.False(t, dirty)
}
