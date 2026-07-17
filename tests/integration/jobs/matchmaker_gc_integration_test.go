//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/jobs"
)

type matchReleaseCall struct {
	tenantID     int64
	allocationID fleet.AllocationID
}

type recordingMatchReleaser struct {
	calls []matchReleaseCall
}

func (r *recordingMatchReleaser) Deallocate(ctx context.Context, allocationID fleet.AllocationID) error {
	tenantID, err := db.TenantFromContext(ctx)
	if err != nil {
		return err
	}
	r.calls = append(r.calls, matchReleaseCall{tenantID: tenantID, allocationID: allocationID})
	return nil
}

func TestSweepMatchmakerRecords_deallocatesOnlyExpiredUnclaimedMatches(t *testing.T) {
	_, raw := startJobsDB(t)
	ctx := context.Background()

	var tenantID, projectID, fleetID int64
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('match-gc') RETURNING id`).Scan(&tenantID))
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'game') RETURNING id`, tenantID).Scan(&projectID))
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend)
		 VALUES ($1, $2, 'default', 'fake') RETURNING id`, tenantID, projectID).Scan(&fleetID))

	insertAllocation := func() int64 {
		t.Helper()
		var allocationID int64
		require.NoError(t, raw.QueryRow(ctx,
			`INSERT INTO game_server_allocations
			 (tenant_id, project_id, fleet_id, backend, status)
			 VALUES ($1, $2, $3, 'fake', 'allocated') RETURNING id`,
			tenantID, projectID, fleetID).Scan(&allocationID))
		return allocationID
	}
	insertMatch := func(id string, allocationID int64, expiresAt time.Time, claimedAt *time.Time) {
		t.Helper()
		_, err := raw.Exec(ctx,
			`INSERT INTO matchmaker_matches
			 (id, tenant_id, project_id, mode, fleet_id, roster, expires_at, allocation_id, claimed_at)
			 VALUES ($1, $2, $3, 'fleet_allocation', $4, '[]', $5, $6, $7)`,
			id, tenantID, projectID, fleetID, expiresAt, allocationID, claimedAt)
		require.NoError(t, err)
	}

	expiredUnclaimedID := insertAllocation()
	expiredClaimedID := insertAllocation()
	activeUnclaimedID := insertAllocation()
	now := time.Now().UTC()
	claimedAt := now.Add(-2 * time.Hour)
	insertMatch("expired-unclaimed", expiredUnclaimedID, now.Add(-time.Hour), nil)
	insertMatch("expired-claimed", expiredClaimedID, now.Add(-time.Hour), &claimedAt)
	insertMatch("active-unclaimed", activeUnclaimedID, now.Add(time.Hour), nil)
	releaser := &recordingMatchReleaser{}
	appConfig := raw.Config().Copy()
	appConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appPGX, err := pgxpool.NewWithConfig(ctx, appConfig)
	require.NoError(t, err)
	t.Cleanup(appPGX.Close)
	pool := db.NewPool(appPGX)

	err = jobs.SweepMatchmakerRecords(ctx, pool, releaser)

	require.NoError(t, err)
	assert.Equal(t, []matchReleaseCall{{tenantID: tenantID, allocationID: fleet.AllocationID(expiredUnclaimedID)}}, releaser.calls)
	var expiredUnclaimedRows, activeUnclaimedRows int64
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM matchmaker_matches WHERE id = 'expired-unclaimed'`).Scan(&expiredUnclaimedRows))
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM matchmaker_matches WHERE id = 'active-unclaimed'`).Scan(&activeUnclaimedRows))
	assert.Zero(t, expiredUnclaimedRows)
	assert.Equal(t, int64(1), activeUnclaimedRows)
}
