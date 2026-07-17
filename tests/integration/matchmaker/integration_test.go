//go:build integration

package matchmaker_test

import (
	"context"
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

type allocatorRecorder struct {
	address     string
	protocol    string
	called      atomic.Int64
	deallocated atomic.Int64
	nextID      atomic.Int64
}

func (a *allocatorRecorder) Allocate(_ context.Context, _ fleet.AllocationRequest) (*fleet.Allocation, error) {
	a.called.Add(1)
	id := fleet.AllocationID(a.nextID.Add(1))
	return &fleet.Allocation{ID: id, Address: a.address, Protocol: a.protocol, Status: fleet.StatusReady}, nil
}

func (a *allocatorRecorder) Deallocate(_ context.Context, _ fleet.AllocationID) error {
	a.deallocated.Add(1)
	return nil
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
	alloc := &allocatorRecorder{address: "10.0.0.7:7777"}
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
	_, err := queue.Enqueue(tenantCtx, matchmaker.EnqueueRequest{
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
