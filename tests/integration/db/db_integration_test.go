//go:build integration

package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/migrate"
)

func startMigrated(t *testing.T) *pgxpool.Pool {
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

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "..", "db", "migrations"))
	require.NoError(t, err)

	r, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, r.Up())
	require.NoError(t, r.Close())

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

func TestQ_sets_app_tenant_id_GUC_inside_transaction(t *testing.T) {
	pool := startMigrated(t)
	p := db.NewPool(pool)

	ctx := db.WithTenant(context.Background(), 17)

	var got string
	err := p.Q(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT current_setting('app.tenant_id', true)").Scan(&got)
	})

	require.NoError(t, err)
	assert.Equal(t, "17", got)
}

func TestReadPool_allows_reads_and_sets_tenant_guc(t *testing.T) {
	pool := startMigrated(t)
	p := db.NewReadPoolWithTimeout(pool, 30*time.Second)

	ctx := db.WithTenant(context.Background(), 42)

	var got string
	err := p.Q(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT current_setting('app.tenant_id', true)").Scan(&got)
	})

	require.NoError(t, err)
	assert.Equal(t, "42", got)
}

func TestReadPool_rejects_writes(t *testing.T) {
	pool := startMigrated(t)
	p := db.NewReadPoolWithTimeout(pool, 30*time.Second)

	ctx := db.WithTenant(context.Background(), 1)

	err := p.Q(ctx, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, "INSERT INTO tenants (id, name) VALUES (2, 'nope')")
		return execErr
	})

	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "25006", pgErr.Code) // read_only_sql_transaction
}

func TestQ_returns_ErrNoTenant_when_context_missing_tenant(t *testing.T) {
	pool := startMigrated(t)
	p := db.NewPool(pool)

	err := p.Q(context.Background(), func(_ pgx.Tx) error {
		t.Fatal("closure must not run when context has no tenant")
		return nil
	})

	assert.True(t, errors.Is(err, db.ErrNoTenant))
}

func TestQ_rolls_back_on_closure_error(t *testing.T) {
	pool := startMigrated(t)
	p := db.NewPool(pool)

	ctx := db.WithTenant(context.Background(), 1)

	_, err := pool.Exec(ctx, "INSERT INTO tenants (id, name) VALUES (1, 'seed')")
	require.NoError(t, err)

	closureErr := errors.New("boom")
	err = p.Q(ctx, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, "UPDATE tenants SET name = 'mutated' WHERE id = 1")
		return closureErr
	})
	require.ErrorIs(t, err, closureErr)

	var name string
	require.NoError(t, pool.QueryRow(ctx, "SELECT name FROM tenants WHERE id = 1").Scan(&name))
	assert.Equal(t, "seed", name)
}

func TestRevokeSession_reports_zero_rows_when_session_already_revoked(t *testing.T) {
	pool := startMigrated(t)
	ctx := context.Background()

	var tenantID, projectID, playerID, sessionID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('session-revoke-tenant') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'session-revoke-project') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'session-revoke-player') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO sessions (tenant_id, project_id, player_id, refresh_hash, expires_at)
		 VALUES ($1, $2, $3, '\x01', now() + interval '1 hour') RETURNING id`,
		tenantID, projectID, playerID).Scan(&sessionID))

	tenantCtx := db.WithTenant(ctx, tenantID)
	queries := db.NewPool(pool)
	var firstRows, secondRows int64
	err := queries.Q(tenantCtx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		firstRows, err = q.RevokeSession(tenantCtx, sessionID)
		if err != nil {
			return err
		}
		secondRows, err = q.RevokeSession(tenantCtx, sessionID)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), firstRows)
	assert.Zero(t, secondRows)
}
