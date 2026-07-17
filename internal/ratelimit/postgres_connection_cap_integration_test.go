//go:build integration

package ratelimit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/migrate"
)

func startConnectionCapPostgres(t *testing.T) *db.Pool {
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

	raw, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	_, err = raw.Exec(ctx, "INSERT INTO tenants (id, name) VALUES (7001, 'cap test'), (7002, 'lease test'), (7003, 'burst test'), (7004, 'shrink test')")
	require.NoError(t, err)

	appConfig, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	appConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appRaw, err := pgxpool.NewWithConfig(ctx, appConfig)
	require.NoError(t, err)
	t.Cleanup(appRaw.Close)
	return db.NewPool(appRaw)
}

func newIntegrationLeasedCap(t *testing.T, pool *db.Pool, region, holder string) *LeasedConnectionCap {
	t.Helper()
	cap := newLeasedConnectionCap(newPostgresGrantStore(pool), nil, leasedCapOptions{
		Region:        region,
		HolderID:      holder,
		Lease:         time.Minute,
		RenewInterval: -1,
	})
	t.Cleanup(func() { _ = cap.Close(context.Background()) })
	return cap
}

func TestPostgresGrantStore_shrinks_a_holders_existing_allocation(t *testing.T) {
	pool := startConnectionCapPostgres(t)
	store := newPostgresGrantStore(pool)
	caps := CapLimits{Sustained: 100, Ceiling: 100}

	peak, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7004, Region: "us-east", HolderID: "shrinking-app",
		Requested: 100, Caps: caps, Lease: time.Minute,
	})
	require.NoError(t, err)
	require.Equal(t, int64(100), peak.Allocated)

	shrunk, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7004, Region: "us-east", HolderID: "shrinking-app",
		Used: 5, Requested: 32, Caps: caps, Lease: time.Minute,
	})
	require.NoError(t, err)
	reused, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7004, Region: "us-east", HolderID: "other-app",
		Requested: 100, Caps: caps, Lease: time.Minute,
	})
	require.NoError(t, err)

	assert.Equal(t, int64(37), shrunk.Allocated)
	assert.Equal(t, int64(63), reused.Allocated)
}

func TestPostgresConnectionCap_shares_a_regional_ceiling_and_isolates_regions(t *testing.T) {
	pool := startConnectionCapPostgres(t)
	eastA := newIntegrationLeasedCap(t, pool, "us-east", "east-a")
	eastB := newIntegrationLeasedCap(t, pool, "us-east", "east-b")
	westA := newIntegrationLeasedCap(t, pool, "us-west", "west-a")
	caps := CapLimits{Sustained: 4, Ceiling: 4}

	for range 4 {
		decision, err := eastA.Acquire(context.Background(), 7001, caps)
		require.NoError(t, err)
		require.True(t, decision.Allowed)
	}
	renewed, err := newPostgresGrantStore(pool).Renew(context.Background(), grantRenewRequest{
		Region:   "us-east",
		HolderID: "east-a",
		Lease:    time.Minute,
		Grants:   []grantRenewal{{TenantID: 7001, Used: 4}},
	})
	require.NoError(t, err)
	assert.Contains(t, renewed, int64(7001), "the app role can run batched cross-tenant renewal")

	eastDecision, err := eastB.Acquire(context.Background(), 7001, caps)
	require.NoError(t, err)
	assert.False(t, eastDecision.Allowed)
	assert.Equal(t, CapRejectCeiling, eastDecision.Reason)

	westDecision, err := westA.Acquire(context.Background(), 7001, caps)
	require.NoError(t, err)
	assert.True(t, westDecision.Allowed, "west has an independent regional envelope")

	for range 4 {
		require.NoError(t, eastA.Release(context.Background(), 7001))
	}
	eastDecision, err = eastB.Acquire(context.Background(), 7001, caps)
	require.NoError(t, err)
	assert.True(t, eastDecision.Allowed, "released east capacity is reusable by the other app")

	_, err = newPostgresGrantStore(pool).Sync(context.Background(), grantRequest{
		TenantID: 7002, Region: "us-east", HolderID: "other-tenant",
		Requested: 1, Caps: CapLimits{Sustained: 2, Ceiling: 2}, Lease: time.Minute,
	})
	require.NoError(t, err)
	var visibleCrossTenant int
	tenantCtx := db.WithTenant(context.Background(), 7001)
	require.NoError(t, pool.Q(tenantCtx, func(tx pgx.Tx) error {
		return tx.QueryRow(tenantCtx, `
SELECT count(*)
FROM realtime_connection_grants
WHERE tenant_id = 7002`).Scan(&visibleCrossTenant)
	}))
	assert.Zero(t, visibleCrossTenant, "worker renewal policy must not weaken request-scoped RLS")
}

func TestPostgresGrantStore_recovers_capacity_after_a_process_lease_expires(t *testing.T) {
	pool := startConnectionCapPostgres(t)
	store := newPostgresGrantStore(pool)
	caps := CapLimits{Sustained: 2, Ceiling: 2}

	first, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7002, Region: "us-east", HolderID: "stopped-app",
		Requested: 2, Caps: caps, Lease: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), first.Allocated)
	blocked, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7002, Region: "us-east", HolderID: "replacement-app",
		Requested: 2, Caps: caps, Lease: time.Minute,
	})
	require.NoError(t, err)
	require.Zero(t, blocked.Allocated)
	time.Sleep(150 * time.Millisecond)

	recovered, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7002, Region: "us-east", HolderID: "replacement-app",
		Requested: 2, Caps: caps, Lease: time.Minute,
	})
	require.NoError(t, err)

	assert.Equal(t, int64(2), recovered.Allocated)
}

func TestPostgresGrantStore_enforces_and_refills_the_regional_burst_budget(t *testing.T) {
	pool := startConnectionCapPostgres(t)
	store := newPostgresGrantStore(pool)
	caps := CapLimits{Sustained: 2, Ceiling: 4}

	_, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7003, Region: "us-east", HolderID: "burst-app",
		Caps: caps, Lease: time.Minute,
	})
	require.NoError(t, err)
	tenantCtx := db.WithTenant(context.Background(), 7003)
	require.NoError(t, pool.Q(tenantCtx, func(tx pgx.Tx) error {
		_, err := tx.Exec(tenantCtx, `
UPDATE realtime_connection_cap_states
SET burst_remaining_ns = 0,
    last_assessed_at = transaction_timestamp() + interval '1 minute'
WHERE tenant_id = 7003 AND region = 'us-east'`)
		return err
	}))

	depleted, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7003, Region: "us-east", HolderID: "burst-app",
		Requested: 4, Caps: caps, Lease: time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), depleted.Allocated)
	require.NoError(t, pool.Q(tenantCtx, func(tx pgx.Tx) error {
		_, err := tx.Exec(tenantCtx, `
UPDATE realtime_connection_cap_states
SET burst_remaining_ns = 0,
    last_assessed_at = transaction_timestamp() - interval '1 hour'
WHERE tenant_id = 7003 AND region = 'us-east'`)
		return err
	}))

	refilled, err := store.Sync(context.Background(), grantRequest{
		TenantID: 7003, Region: "us-east", HolderID: "burst-app",
		Requested: 4, Caps: caps, Lease: time.Minute,
	})
	require.NoError(t, err)

	assert.Equal(t, int64(4), refilled.Allocated)
}
