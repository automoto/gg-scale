//go:build integration

package jobs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/migrate"
)

func startJobsDB(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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
	r, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, r.Up())
	require.NoError(t, r.Close())

	raw, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	return db.NewPool(raw), raw
}

func TestSweepExpiredTrustedDevices_removes_only_expired_rows(t *testing.T) {
	pool, raw := startJobsDB(t)
	ctx := context.Background()

	var userID int64
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO dashboard_users (email, password_hash, email_verified_at)
		 VALUES ('gc@example.com', '\x00', now()) RETURNING id`).Scan(&userID))
	var accountID string
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO player_accounts (email, password_hash, email_verified_at)
		 VALUES ('gc-player@example.com', '\x00', now()) RETURNING id`).Scan(&accountID))

	_, err := raw.Exec(ctx,
		`INSERT INTO dashboard_trusted_devices (dashboard_user_id, token_hash, expires_at)
		 VALUES ($1, '\x01', now() - interval '1 day'),
		        ($1, '\x02', now() + interval '1 day')`, userID)
	require.NoError(t, err)
	_, err = raw.Exec(ctx,
		`INSERT INTO player_account_trusted_devices (player_account_id, token_hash, expires_at)
		 VALUES ($1, '\x03', now() - interval '1 day'),
		        ($1, '\x04', now() + interval '1 day')`, accountID)
	require.NoError(t, err)

	require.NoError(t, SweepExpiredTrustedDevices(ctx, pool))

	var dashboardLeft, playersLeft int64
	require.NoError(t, raw.QueryRow(ctx, `SELECT count(*) FROM dashboard_trusted_devices`).Scan(&dashboardLeft))
	require.NoError(t, raw.QueryRow(ctx, `SELECT count(*) FROM player_account_trusted_devices`).Scan(&playersLeft))
	assert.Equal(t, int64(1), dashboardLeft, "only the live dashboard device survives")
	assert.Equal(t, int64(1), playersLeft, "only the live player device survives")
}
