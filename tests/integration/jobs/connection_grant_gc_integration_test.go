//go:build integration

package jobs_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/jobs"
)

func TestSweepExpiredConnectionGrants_removes_expired_rows_across_tenants(t *testing.T) {
	_, raw := startJobsDB(t)
	ctx := context.Background()

	_, err := raw.Exec(ctx, `
INSERT INTO tenants (id, name) VALUES (7101, 'expired grant'), (7102, 'live grant');
INSERT INTO realtime_connection_cap_states (
    tenant_id, region, sustained, ceiling, burst_remaining_ns, last_assessed_at
) VALUES
    (7101, 'us-east', 100, 200, 300000000000, transaction_timestamp()),
    (7102, 'us-west', 100, 200, 300000000000, transaction_timestamp());
INSERT INTO realtime_connection_grants (
    tenant_id, region, holder_id, allocated, used, expires_at
) VALUES
    (7101, 'us-east', 'dead-holder', 32, 0, transaction_timestamp() - interval '1 minute'),
    (7102, 'us-west', 'live-holder', 32, 1, transaction_timestamp() + interval '1 minute')`)
	require.NoError(t, err)

	appConfig := raw.Config().Copy()
	appConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appPGX, err := pgxpool.NewWithConfig(ctx, appConfig)
	require.NoError(t, err)
	t.Cleanup(appPGX.Close)

	require.NoError(t, jobs.SweepExpiredConnectionGrants(ctx, db.NewPool(appPGX)))

	var expiredRows, liveRows int64
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM realtime_connection_grants WHERE holder_id = 'dead-holder'`).Scan(&expiredRows))
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM realtime_connection_grants WHERE holder_id = 'live-holder'`).Scan(&liveRows))
	assert.Zero(t, expiredRows)
	assert.Equal(t, int64(1), liveRows)
}
