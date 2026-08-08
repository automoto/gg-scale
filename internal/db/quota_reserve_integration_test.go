//go:build integration

package db_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/migrate"
	"github.com/automoto/gg-scale/internal/quota"
)

func startQuotaPostgres(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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
	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "db", "migrations"))
	require.NoError(t, err)
	runner, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, runner.Up())
	require.NoError(t, runner.Close())

	owner, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(owner.Close)

	appConfig, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	appConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appRaw, err := pgxpool.NewWithConfig(ctx, appConfig)
	require.NoError(t, err)
	t.Cleanup(appRaw.Close)
	return db.NewPool(appRaw), owner
}

func seedQuotaTenant(t *testing.T, owner *pgxpool.Pool, id int64, tier int16, enforce bool, playerCount int64) {
	t.Helper()
	_, err := owner.Exec(context.Background(),
		`INSERT INTO tenants (id, name, tier, enforce_quotas, player_count)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, fmt.Sprintf("quota tenant %d", id), tier, enforce, playerCount)
	require.NoError(t, err)
}

func reserveSlot(t *testing.T, pool *db.Pool, tenantID, limit int64) int64 {
	t.Helper()
	var rows int64
	ctx := db.WithTenant(context.Background(), tenantID)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlcgen.New(tx).ReserveTenantPlayerSlot(ctx, limit)
		return qerr
	})
	require.NoError(t, err)
	return rows
}

func tenantPlayerCount(t *testing.T, owner *pgxpool.Pool, tenantID int64) int64 {
	t.Helper()
	var n int64
	err := owner.QueryRow(context.Background(),
		"SELECT player_count FROM tenants WHERE id = $1", tenantID).Scan(&n)
	require.NoError(t, err)
	return n
}

func TestReserveTenantPlayerSlot(t *testing.T) {
	pool, owner := startQuotaPostgres(t)

	t.Run("should_reserve_and_increment_under_limit", func(t *testing.T) {
		seedQuotaTenant(t, owner, 8001, 1, true, 0)
		rows := reserveSlot(t, pool, 8001, 500_000)
		assert.Equal(t, int64(1), rows)
		assert.Equal(t, int64(1), tenantPlayerCount(t, owner, 8001))
	})

	t.Run("should_reject_at_limit_without_incrementing", func(t *testing.T) {
		seedQuotaTenant(t, owner, 8002, 0, true, 100_000)
		rows := reserveSlot(t, pool, 8002, 100_000)
		assert.Equal(t, int64(0), rows)
		assert.Equal(t, int64(100_000), tenantPlayerCount(t, owner, 8002))
	})

	t.Run("should_increment_unlimited_class", func(t *testing.T) {
		seedQuotaTenant(t, owner, 8003, 3, true, 5_000_000)
		rows := reserveSlot(t, pool, 8003, quota.Unlimited)
		assert.Equal(t, int64(1), rows)
		assert.Equal(t, int64(5_000_001), tenantPlayerCount(t, owner, 8003))
	})

	t.Run("should_admit_and_increment_when_unenforced", func(t *testing.T) {
		seedQuotaTenant(t, owner, 8004, 0, false, 7)
		rows := reserveSlot(t, pool, 8004, 3)
		assert.Equal(t, int64(1), rows)
		assert.Equal(t, int64(8), tenantPlayerCount(t, owner, 8004))
	})

	t.Run("should_not_over_admit_under_concurrency", func(t *testing.T) {
		seedQuotaTenant(t, owner, 8005, 0, true, 0)
		const limit, attempts = 10, 25
		var admitted atomic.Int64
		var wg sync.WaitGroup
		for range attempts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx := db.WithTenant(context.Background(), 8005)
				_ = pool.Q(ctx, func(tx pgx.Tx) error {
					rows, err := sqlcgen.New(tx).ReserveTenantPlayerSlot(ctx, limit)
					if err != nil {
						return err
					}
					admitted.Add(rows)
					return nil
				})
			}()
		}
		wg.Wait()
		assert.Equal(t, int64(limit), admitted.Load())
		assert.Equal(t, int64(limit), tenantPlayerCount(t, owner, 8005))
	})
}

func TestGetTenantQuotaContext_returns_player_count_snapshot(t *testing.T) {
	pool, owner := startQuotaPostgres(t)
	seedQuotaTenant(t, owner, 8101, 2, true, 42)

	var qc sqlcgen.GetTenantQuotaContextRow
	ctx := db.WithTenant(context.Background(), 8101)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		var qerr error
		qc, qerr = sqlcgen.New(tx).GetTenantQuotaContext(ctx)
		return qerr
	})
	require.NoError(t, err)
	assert.Equal(t, int16(2), qc.Tier)
	assert.True(t, qc.EnforceQuotas)
	assert.Equal(t, int64(42), qc.PlayerCount)
}
