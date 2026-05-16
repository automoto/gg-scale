//go:build integration

package matchmaker_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/migrate"
)

func startMigratedDB(t *testing.T) *pgxpool.Pool {
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

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

type allocatorRecorder struct {
	address string
	called  atomic.Int64
}

func (a *allocatorRecorder) Allocate(_ context.Context, _ fleet.AllocationRequest) (*fleet.Allocation, error) {
	a.called.Add(1)
	return &fleet.Allocation{Address: a.address, Status: fleet.StatusReady}, nil
}

// TestPGQueueListenWakesWorkerOnInsert is the load-bearing assertion for
// the LISTEN/NOTIFY pivot: a ticket inserted into matchmaking_tickets fires
// the trigger, the PGQueue listener decodes the payload, the worker wakes,
// and the bucket is processed — well under the fallback ticker would have
// fired.
func TestPGQueueListenWakesWorkerOnInsert(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)

	ctx := context.Background()
	var tenantID, projectID, fleetID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-listen-test') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	// The matchmaking_tickets.fleet_id FK is RESTRICT — every queued ticket
	// must reference an existing fleet template, even in tests. Seed one
	// before enqueuing.
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	queue := matchmaker.NewPGQueue(appPool)
	alloc := &allocatorRecorder{address: "10.0.0.7:7777"}
	w := matchmaker.NewWorker(queue, alloc, nil, matchmaker.WorkerConfig{
		BucketSize: 1,
		// Long enough that any sub-second wakeup proves it came from
		// LISTEN/NOTIFY, not the fallback tick.
		Interval: time.Hour,
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(runCtx)

	// Give the listener a beat to subscribe before we publish.
	time.Sleep(100 * time.Millisecond)

	tenantCtx := db.WithTenant(ctx, tenantID)
	_, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		FleetID:   fleetID,
		EndUserID: 1,
		Region:    "us-east-1",
		GameMode:  "1v1",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return alloc.called.Load() == 1 },
		2*time.Second, 20*time.Millisecond,
		"worker did not wake within 2s — LISTEN/NOTIFY round-trip failed")
}
