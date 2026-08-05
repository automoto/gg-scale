//go:build integration

package matchmaker_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
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

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/gamesession"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/migrate"
)

const matchmakerTemplateDB = "ggscale_matchmaker_template"

type matchmakerPostgresFixture struct {
	ctr         *tcpostgres.PostgresContainer
	admin       *pgxpool.Pool
	templateDSN string
	seq         atomic.Uint64
	err         error
}

var (
	matchmakerPGOnce sync.Once
	matchmakerPG     *matchmakerPostgresFixture
)

func TestMain(m *testing.M) {
	code := m.Run()
	if matchmakerPG != nil {
		matchmakerPG.close()
	}
	os.Exit(code)
}

func startMigratedDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	t.Parallel()
	ctx := context.Background()
	pg := sharedMatchmakerPostgres(t)
	dbName, dsn := pg.createDatabase(t)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		pg.dropDatabase(dbName)
	})
	return pool
}

func sharedMatchmakerPostgres(t *testing.T) *matchmakerPostgresFixture {
	t.Helper()
	matchmakerPGOnce.Do(func() {
		matchmakerPG = &matchmakerPostgresFixture{}
		matchmakerPG.err = matchmakerPG.start(context.Background())
	})
	require.NoError(t, matchmakerPG.err)
	return matchmakerPG
}

func (p *matchmakerPostgresFixture) start(ctx context.Context) error {
	ctr, err := tcpostgres.Run(ctx,
		"postgres:17",
		tcpostgres.WithDatabase(matchmakerTemplateDB),
		tcpostgres.WithUsername("ggscale"),
		tcpostgres.WithPassword("ggscale"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return err
	}
	p.ctr = ctr

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	p.templateDSN = dsn

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "..", "db", "migrations"))
	if err != nil {
		return err
	}

	r, err := migrate.New(dsn, migrationsDir)
	if err != nil {
		return err
	}
	if err := r.Up(); err != nil {
		_ = r.Close()
		return err
	}
	if err := r.Close(); err != nil {
		return err
	}

	adminDSN, err := matchmakerDSNForDatabase(dsn, "postgres")
	if err != nil {
		return err
	}
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return err
	}
	p.admin = admin
	return nil
}

func (p *matchmakerPostgresFixture) createDatabase(t *testing.T) (string, string) {
	t.Helper()
	dbName := fmt.Sprintf("ggscale_matchmaker_%d", p.seq.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := p.admin.Exec(ctx,
		"CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()+
			" WITH TEMPLATE "+pgx.Identifier{matchmakerTemplateDB}.Sanitize()+
			" OWNER ggscale")
	require.NoError(t, err)
	dsn, err := matchmakerDSNForDatabase(p.templateDSN, dbName)
	require.NoError(t, err)
	return dbName, dsn
}

func (p *matchmakerPostgresFixture) dropDatabase(dbName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = p.admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
	_, _ = p.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
}

func (p *matchmakerPostgresFixture) close() {
	if p.admin != nil {
		p.admin.Close()
	}
	if p.ctr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = p.ctr.Terminate(ctx)
	}
}

func matchmakerDSNForDatabase(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

// allocatorRecorder is a fake fleet allocator that persists real
// game_server_allocations rows, so worker-created matches exercise the real
// matchmaker_matches_allocation_id_fkey. The previous fake minted IDs without
// rows, which made every fleet-mode CreateMatch fail that FK and quietly
// release the group.
type allocatorRecorder struct {
	pool        *pgxpool.Pool
	address     string
	protocol    string
	called      atomic.Int64
	deallocated atomic.Int64
}

func (a *allocatorRecorder) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	a.called.Add(1)
	var id int64
	err := a.pool.QueryRow(ctx,
		`INSERT INTO game_server_allocations (tenant_id, project_id, fleet_id, backend, region, address, status)
		 VALUES ($1, $2, $3, 'fake', $4, $5, 'ready') RETURNING id`,
		req.TenantID, req.ProjectID, req.FleetID, req.Region, a.address).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &fleet.Allocation{ID: fleet.AllocationID(id), Address: a.address, Protocol: a.protocol, Status: fleet.StatusReady}, nil
}

func (a *allocatorRecorder) Deallocate(ctx context.Context, id fleet.AllocationID) error {
	a.deallocated.Add(1)
	_, err := a.pool.Exec(ctx,
		`UPDATE game_server_allocations SET status = 'shutdown', released_at = now() WHERE id = $1`, int64(id))
	return err
}

// TestPGQueueListenWakesWorkerOnInsert is the load-bearing assertion for
// the LISTEN/NOTIFY pivot: Enqueue commits the ticket, then sends the
// post-commit app-side NOTIFY (the per-row AFTER INSERT trigger was removed
// in 0024), the PGQueue listener decodes the payload, the worker wakes, and
// the bucket is processed — well under the one-hour fallback ticker, proving
// the wake came from NOTIFY and not the fallback tick.
func TestPGQueueListenWakesWorkerOnInsert(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)

	ctx := context.Background()
	var tenantID, projectID, fleetID, playerID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-listen-test') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-listen') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	// The matchmaking_tickets.fleet_id FK is RESTRICT — every queued ticket
	// must reference an existing fleet template, even in tests. Seed one
	// before enqueuing.
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	queue := matchmaker.NewPGQueue(appPool)
	alloc := &allocatorRecorder{pool: pool, address: "10.0.0.7:7777"}
	w := matchmaker.NewWorker(queue, alloc, nil, matchmaker.WorkerConfig{
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
	ticket, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		FleetID:   fleetID,
		PlayerID:  playerID,
		Region:    "us-east-1",
		GameMode:  "1v1",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return alloc.called.Load() == 1 },
		2*time.Second, 20*time.Millisecond,
		"worker did not wake within 2s — LISTEN/NOTIFY round-trip failed")
	require.Eventually(t, func() bool {
		got, gerr := queue.Get(tenantCtx, ticket.ID, playerID)
		return gerr == nil && got.Status == matchmaker.StatusMatched
	}, 2*time.Second, 20*time.Millisecond,
		"ticket must reach matched after the NOTIFY wake")
	assert.Zero(t, alloc.deallocated.Load(), "no orphan releases on the happy path")
}

// TestMatchmakerNotifyTriggerRemoved proves migration 0024 applied: the
// in-transaction per-row NOTIFY trigger and its function are gone, so the
// enqueue commit no longer takes the cluster-global notify lock. Wakeups now
// come from the app-side post-commit NOTIFY instead.
func TestMatchmakerNotifyTriggerRemoved(t *testing.T) {
	pool := startMigratedDB(t)
	ctx := context.Background()

	var triggers int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_trigger WHERE tgname = 'matchmaking_tickets_notify'`).Scan(&triggers))
	assert.Equal(t, 0, triggers, "matchmaking_tickets_notify trigger must be dropped by 0024")

	var funcs int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_proc WHERE proname = 'notify_matchmaker_ticket'`).Scan(&funcs))
	assert.Equal(t, 0, funcs, "notify_matchmaker_ticket() must be dropped by 0024")
}

// TestPGQueueEnqueueDebounceDoesNotStrandTickets is the safety net for the
// debounce: two tickets enqueued to the same bucket in quick succession may
// have the second wakeup coalesced away, but neither ticket is ever stranded.
// With a short fallback tick the worker still processes both — this proves a
// debounced (or lost) NOTIFY only defers a ticket to the fallback, never drops
// it.
func TestPGQueueEnqueueDebounceDoesNotStrandTickets(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	var tenantID, projectID, fleetID, playerA, playerB int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-debounce') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-a') RETURNING id`,
		tenantID, projectID).Scan(&playerA))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-b') RETURNING id`,
		tenantID, projectID).Scan(&playerB))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	queue := matchmaker.NewPGQueue(appPool)
	alloc := &allocatorRecorder{pool: pool, address: "10.0.0.9:7777"}
	w := matchmaker.NewWorker(queue, alloc, nil, matchmaker.WorkerConfig{
		// Short fallback so a coalesced-away second wakeup is still caught
		// quickly, keeping the test deterministic.
		Interval: 250 * time.Millisecond,
	})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(runCtx)
	time.Sleep(100 * time.Millisecond)

	tenantCtx := db.WithTenant(ctx, tenantID)
	// Two players, same bucket, back to back. The second enqueue's NOTIFY
	// falls inside the 100ms debounce window and is very likely coalesced.
	tickets := make([]*matchmaker.Ticket, 0, 2)
	for _, playerID := range []int64{playerA, playerB} {
		ticket, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
			TenantID: tenantID, ProjectID: projectID, FleetID: fleetID,
			PlayerID: playerID, Region: "us-east-1", GameMode: "1v1",
		})
		require.NoError(t, err)
		tickets = append(tickets, ticket)
	}

	require.Eventually(t, func() bool {
		for _, ticket := range tickets {
			got, gerr := queue.Get(tenantCtx, ticket.ID, ticket.PlayerID)
			if gerr != nil || got.Status != matchmaker.StatusMatched {
				return false
			}
		}
		return true
	}, 3*time.Second, 20*time.Millisecond,
		"both tickets must be matched even if the second wakeup was debounced")
	assert.Equal(t, int64(2), alloc.called.Load(), "one allocation per singleton match, no retries")
	assert.Zero(t, alloc.deallocated.Load(), "no orphan releases on the happy path")
}

// TestPGQueueNotifyWakesWorkerForSecondSameBucketPlayer pins the enqueue
// debounce keying: the second player's enqueue must wake the worker via
// NOTIFY even when it lands inside the first player's debounce window. The
// worker runs the production 5s fallback interval, so a coalesced second
// wakeup would leave the formable pair waiting on the tick — the removed
// per-row AFTER INSERT trigger fired for that enqueue too, making that wait
// a regression.
func TestPGQueueNotifyWakesWorkerForSecondSameBucketPlayer(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	var tenantID, projectID, fleetID, playerA, playerB int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-second-wake') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-a') RETURNING id`,
		tenantID, projectID).Scan(&playerA))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-b') RETURNING id`,
		tenantID, projectID).Scan(&playerB))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	queue := matchmaker.NewPGQueue(appPool)
	alloc := &allocatorRecorder{pool: pool, address: "10.0.0.11:7777"}
	// Interval left zero so NewWorker applies the production 5s default:
	// only a NOTIFY-driven wake can match the pair inside the 2s deadline.
	w := matchmaker.NewWorker(queue, alloc, nil, matchmaker.WorkerConfig{})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(runCtx)
	time.Sleep(100 * time.Millisecond)

	tenantCtx := db.WithTenant(ctx, tenantID)
	tickets := make([]*matchmaker.Ticket, 0, 2)
	for i, playerID := range []int64{playerA, playerB} {
		if i == 1 {
			// Inside the first enqueue's 100ms debounce window, but late
			// enough that the first wakeup pass has come and gone.
			time.Sleep(60 * time.Millisecond)
		}
		ticket, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
			TenantID: tenantID, ProjectID: projectID, FleetID: fleetID,
			PlayerID: playerID, Region: "us-east-1", GameMode: "1v1",
			MinCount: 2, MaxCount: 2,
		})
		require.NoError(t, err)
		tickets = append(tickets, ticket)
	}

	require.Eventually(t, func() bool {
		for _, ticket := range tickets {
			got, gerr := queue.Get(tenantCtx, ticket.ID, ticket.PlayerID)
			if gerr != nil || got.Status != matchmaker.StatusMatched {
				return false
			}
		}
		return true
	}, 2*time.Second, 20*time.Millisecond,
		"second same-bucket enqueue must wake the worker via NOTIFY, not the 5s fallback tick")
	assert.Equal(t, int64(1), alloc.called.Load(), "one allocation for the pair")
	assert.Zero(t, alloc.deallocated.Load(), "no orphan releases on the happy path")
}

// TestPGQueueConcurrentClaimsCannotStrandTickets is the C1 regression. Two
// queues compete for the same bucket; FOR UPDATE SKIP LOCKED guarantees only
// one claim succeeds. The losing claim returns nil instead of stranding
// rows in 'matched' as the previous PopBucket pattern did.
func TestPGQueueConcurrentClaimsCannotStrandTickets(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	var tenantID, projectID, fleetID, playerID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-claim-race') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-claim') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	queue := matchmaker.NewPGQueue(appPool)
	tenantCtx := db.WithTenant(ctx, tenantID)
	ticket, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, FleetID: fleetID,
		PlayerID: playerID, Region: "us-east-1", GameMode: "1v1",
	})
	require.NoError(t, err)

	bucket := matchmaker.Bucket{TenantID: tenantID, ProjectID: projectID, Mode: matchmaker.ModeFleetAllocation, FleetID: fleetID, Region: "us-east-1", GameMode: "1v1"}

	var wg sync.WaitGroup
	var winner, loser atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := queue.ClaimBucket(ctx, bucket, 1, time.Minute)
			require.NoError(t, err)
			if claim != nil {
				winner.Add(1)
				_, _ = queue.CommitTickets(ctx, claim, []int64{ticket.ID}, "", "10.0.0.1:7777", "tcp")
			} else {
				loser.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), winner.Load(), "exactly one worker should claim+commit")
	assert.Equal(t, int64(3), loser.Load(), "the other three must observe an empty claim, not a stranded ticket")

	got, err := queue.Get(tenantCtx, ticket.ID, playerID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusMatched, got.Status)
	assert.Equal(t, "10.0.0.1:7777", got.MatchAddress)
	assert.Equal(t, "tcp", got.MatchProtocol)
}

// TestPGQueueSweepStaleClaimsReturnsExpiredTicketsToQueued proves M14: a
// crashed worker's claim is recovered by the sweeper, the ticket re-enters
// the queue (under attempt cap), and the next worker can pick it up.
func TestPGQueueSweepStaleClaimsReturnsExpiredTicketsToQueued(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	var tenantID, projectID, fleetID, playerID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-sweep') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-sweep') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	queue := matchmaker.NewPGQueue(appPool)
	tenantCtx := db.WithTenant(ctx, tenantID)
	ticket, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, FleetID: fleetID,
		PlayerID: playerID, Region: "us-east-1", GameMode: "1v1",
	})
	require.NoError(t, err)

	bucket := matchmaker.Bucket{TenantID: tenantID, ProjectID: projectID, Mode: matchmaker.ModeFleetAllocation, FleetID: fleetID, Region: "us-east-1", GameMode: "1v1"}
	claim, err := queue.ClaimBucket(ctx, bucket, 1, 50*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, claim)
	time.Sleep(100 * time.Millisecond)

	n, err := queue.SweepStaleClaims(ctx, 5)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	got, err := queue.Get(tenantCtx, ticket.ID, playerID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status, "swept ticket should be available for re-claim")
}

// TestPGQueueGetAndCancelArePlayerScoped is the ticket-ownership regression:
// a same-tenant, different-user caller must not be able to read or cancel
// another player's ticket by ID. The SQL WHERE player_id filter yields
// ErrNotFound (404 at the HTTP layer), never the ticket.
func TestPGQueueGetAndCancelArePlayerScoped(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	var tenantID, projectID, fleetID, ownerID, otherID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-idor') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'ticket-owner') RETURNING id`,
		tenantID, projectID).Scan(&ownerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'ticket-attacker') RETURNING id`,
		tenantID, projectID).Scan(&otherID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	queue := matchmaker.NewPGQueue(appPool)
	tenantCtx := db.WithTenant(ctx, tenantID)
	ticket, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, FleetID: fleetID,
		PlayerID: ownerID, Region: "us-east-1", GameMode: "1v1",
	})
	require.NoError(t, err)

	// Different user, same tenant: get and cancel both denied.
	_, err = queue.Get(tenantCtx, ticket.ID, otherID)
	assert.ErrorIs(t, err, matchmaker.ErrNotFound, "cross-user get must not leak the ticket")

	err = queue.Cancel(tenantCtx, ticket.ID, otherID)
	assert.ErrorIs(t, err, matchmaker.ErrNotFound, "cross-user cancel must not touch the ticket")

	// The owner can still read it and it remains queued (attacker's cancel
	// was a no-op).
	got, err := queue.Get(tenantCtx, ticket.ID, ownerID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status)
}

// The production pool runs SET ROLE ggscale_app (non-owner, no BYPASSRLS)
// and matchmaking_tickets is FORCE RLS, so the worker's GUC-less scans only
// work through the matchmaking_tickets_worker policy. The other integration
// tests connect as the container superuser and silently bypass RLS — this
// one pins the app-role behavior end to end.
func TestPGQueueWorkerPathWorksUnderAppRoleRLS(t *testing.T) {
	pool := startMigratedDB(t)
	ctx := context.Background()

	var tenantID, projectID, playerID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('rls-t') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-rls') RETURNING id`,
		tenantID, projectID).Scan(&playerID))

	appCfg := pool.Config().Copy()
	appCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appPGX, err := pgxpool.NewWithConfig(ctx, appCfg)
	require.NoError(t, err)
	t.Cleanup(appPGX.Close)

	queue := matchmaker.NewPGQueue(db.NewPool(appPGX))
	tenantCtx := db.WithTenant(ctx, tenantID)
	ticket, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		PlayerID:  playerID,
		Mode:      matchmaker.ModeMatchOnly,
		Region:    "eu-1",
		GameMode:  "1v1",
	})
	require.NoError(t, err)

	w := matchmaker.NewWorker(queue, nil, nil, matchmaker.WorkerConfig{})
	require.NoError(t, w.Tick(ctx))

	got, err := queue.Get(tenantCtx, ticket.ID, playerID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusMatched, got.Status,
		"worker scan/claim/commit must see tickets under the app role (worker RLS policy)")
	require.NotEmpty(t, got.MatchID)

	match, err := queue.GetMatch(tenantCtx, got.MatchID)
	require.NoError(t, err)
	assert.Len(t, match.Roster, 1)
	claimed, err := queue.ClaimMatch(tenantCtx, got.MatchID)
	require.NoError(t, err)
	assert.False(t, claimed.ClaimedAt.IsZero(), "poll claim must work under the app-role RLS policy")
}

// game_session mode end to end against Postgres: two tickets match, the
// worker creates a real game_session sized to the roster with both players
// pre-seeded as members, and the match record carries session_id+join_code.
func TestPGQueueGameSessionModeCreatesJoinableSession(t *testing.T) {
	pool := startMigratedDB(t)
	ctx := context.Background()

	var tenantID, projectID, p1, p2 int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('gs-t') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'gs-p1') RETURNING id`, tenantID, projectID).Scan(&p1))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'gs-p2') RETURNING id`, tenantID, projectID).Scan(&p2))

	appPool := db.NewPool(pool)
	queue := matchmaker.NewPGQueue(appPool)
	sessions := gamesession.NewService(appPool)
	w := matchmaker.NewWorker(queue, nil, nil, matchmaker.WorkerConfig{
		Sessions: gamesession.NewMatchAdapter(sessions),
	})

	tenantCtx := db.WithTenant(ctx, tenantID)
	for _, pid := range []int64{p1, p2} {
		_, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
			TenantID: tenantID, ProjectID: projectID, PlayerID: pid,
			Mode: matchmaker.ModeGameSession, GameMode: "coop",
			MinCount: 2, MaxCount: 2,
		})
		require.NoError(t, err)
	}

	require.NoError(t, w.Tick(ctx))

	var matchID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT match_id FROM matchmaking_tickets WHERE tenant_id = $1 AND player_id = $2`,
		tenantID, p1).Scan(&matchID))
	require.NotEmpty(t, matchID)

	match, err := queue.GetMatch(tenantCtx, matchID)
	require.NoError(t, err)
	require.NotEmpty(t, match.SessionID)
	require.NotEmpty(t, match.JoinCode)
	require.Len(t, match.Roster, 2)

	var maxPlayers, peerCount int
	var private bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT max_players, private FROM game_session WHERE id = $1`,
		match.SessionID).Scan(&maxPlayers, &private))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM game_session_peer WHERE session_id = $1`,
		match.SessionID).Scan(&peerCount))
	assert.Equal(t, 2, maxPlayers, "session sized to roster")
	assert.True(t, private, "matchmade sessions admit only the roster")
	assert.Equal(t, 2, peerCount, "both players pre-seeded as members")
}

// countLiveUnclaimedAllocs returns the fleet matches that still count against a
// player's per-player cap: allocated, unclaimed and unexpired.
func countLiveUnclaimedAllocs(t *testing.T, pool *pgxpool.Pool, tenantID, playerID int64) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM matchmaker_matches
		 WHERE tenant_id = $1 AND mode = 'fleet_allocation'
		   AND allocation_id IS NOT NULL AND claimed_at IS NULL AND expires_at > now()
		   AND roster @> jsonb_build_array(jsonb_build_object('player_id', $2::bigint))`,
		tenantID, playerID).Scan(&n))
	return n
}

// A match row is written before its tickets commit. When the commit finds no
// rows the backend server is released, but the row itself keeps counting
// against the player's cap for the whole match TTL — a day by default — so a
// player who cancels at the wrong instant locks themselves out while holding
// nothing. The row must go as soon as the server is released.
func TestPGQueueDriftedCommitDoesNotStrandCapSlot(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	var tenantID, projectID, fleetID, playerID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-cap-drift') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-cap-drift') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	queue := matchmaker.NewPGQueue(appPool)
	tenantCtx := db.WithTenant(ctx, tenantID)
	alloc := &allocatorRecorder{pool: pool, address: "10.0.0.9:7777", protocol: "udp"}
	w := matchmaker.NewWorker(queue, &slowAllocator{inner: alloc, delay: 250 * time.Millisecond}, nil, matchmaker.WorkerConfig{})

	ticket, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, FleetID: fleetID,
		PlayerID: playerID, Region: "us-east-1", GameMode: "1v1",
	})
	require.NoError(t, err)

	// Cancel while the worker is inside Allocate. Claiming leaves the ticket
	// 'queued', so the cancel succeeds, and the later commit affects no rows.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = queue.Cancel(tenantCtx, ticket.ID, playerID)
	}()

	// The worker allocates, writes the match row, then finds nothing to commit.
	_ = w.Tick(ctx)

	require.Eventually(t, func() bool { return alloc.deallocated.Load() >= 1 }, 5*time.Second, 20*time.Millisecond,
		"the orphan server should be released")
	assert.Eventually(t, func() bool { return countLiveUnclaimedAllocs(t, pool, tenantID, playerID) == 0 },
		5*time.Second, 20*time.Millisecond,
		"a drifted commit must not leave a row consuming a cap slot")
}

// The enqueue-time cap check counts committed matches only, and the worker
// inserts its match in a later transaction. Two tickets enqueued around an
// in-flight allocation must still not put the player over the cap.
func TestPGQueueFleetCapHoldsAcrossConcurrentWorkers(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	ctx := context.Background()

	var tenantID, projectID, fleetID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('mm-cap-race') RETURNING id`).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'test-fleet', 'fake', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))

	var playerID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, 'player-cap-race') RETURNING id`,
		tenantID, projectID).Scan(&playerID))

	const cap = 2
	queue := matchmaker.NewPGQueue(appPool).WithMaxUnclaimedFleetAllocations(cap)
	tenantCtx := db.WithTenant(ctx, tenantID)
	alloc := &allocatorRecorder{pool: pool, address: "10.0.0.10:7777", protocol: "udp"}

	// Each pass: enqueue one ticket and let a worker allocate and commit it.
	// Past the cap the ticket must be refused, and the live count must never
	// exceed it no matter how many passes run.
	for range cap + 3 {
		_, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
			TenantID: tenantID, ProjectID: projectID, FleetID: fleetID,
			PlayerID: playerID, Region: "us-east-1", GameMode: "1v1",
		})
		if errors.Is(err, matchmaker.ErrTooManyUnclaimedAllocations) {
			continue
		}
		require.NoError(t, err)

		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := matchmaker.NewWorker(queue, alloc, nil, matchmaker.WorkerConfig{})
				_ = w.Tick(ctx)
			}()
		}
		wg.Wait()

		assert.LessOrEqual(t, countLiveUnclaimedAllocs(t, pool, tenantID, playerID), cap,
			"live unclaimed allocations must never exceed the configured cap")
	}

	assert.Equal(t, cap, countLiveUnclaimedAllocs(t, pool, tenantID, playerID),
		"the player should settle exactly at the cap")
}

// slowAllocator delays Allocate so a test can land a cancel inside the window
// between the bucket claim and the ticket commit.
type slowAllocator struct {
	inner *allocatorRecorder
	delay time.Duration
}

func (s *slowAllocator) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	time.Sleep(s.delay)
	return s.inner.Allocate(ctx, req)
}

func (s *slowAllocator) Deallocate(ctx context.Context, id fleet.AllocationID) error {
	return s.inner.Deallocate(ctx, id)
}
