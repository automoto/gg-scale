//go:build integration

// e2e:bucket b

package matchmaker_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/gamesession"
	"github.com/automoto/gg-scale/internal/jobs"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/party"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
)

func TestPartyCodeReadyQueueRematch(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "party", "leader")
	_, _, friend := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "friend")
	ctx := db.WithTenant(context.Background(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 2, MaxCount: 2, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	code, err := store.CreateCode(ctx, projectID, p.ID, leader, p.Version, 1)
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.JoinCode(ctx, projectID, friend, code.PartyVersion, code.Code, "127.0.0.1")
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Ready(ctx, projectID, p.ID, leader, p.Version, true, party.Member{})
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Ready(ctx, projectID, p.ID, friend, p.Version, true, party.Member{})
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Queue(ctx, projectID, p.ID, leader, p.Version, "first", "", time.Minute)
	if !assert.NoError(t, err) {
		return
	}
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	buckets, err := q.ListReadyBuckets(ctx)
	if !assert.NoError(t, err) || !assert.Len(t, buckets, 1) {
		return
	}
	claim, err := q.ClaimBucket(ctx, buckets[0], 1, time.Minute)
	if !assert.NoError(t, err) || !assert.NotNil(t, claim) {
		return
	}
	assert.Len(t, claim.Tickets, 2, "batch boundary must not split the party")
	_, err = q.CommitTickets(ctx, claim, []int64{claim.Tickets[0].ID}, "partial", "", "")
	assert.ErrorIs(t, err, matchmaker.ErrShortCommit)
	assert.NoError(t, q.ReturnUnmatched(ctx, claim))
	worker := matchmaker.NewWorker(q, nil, nil, matchmaker.WorkerConfig{})
	if !assert.NoError(t, worker.Tick(ctx)) {
		return
	}
	p, err = store.Get(ctx, projectID, p.ID, leader)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, "matched", p.State)
	assert.NotEmpty(t, p.LastMatchID)
	assert.Zero(t, p.Members[0].ReadyVersion)
	match, err := q.GetMatch(ctx, p.LastMatchID)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, p.ID, match.Roster[0].PartyID)
	previous := p.LastMatchID
	for _, player := range []int64{leader, friend} {
		p, err = store.Ready(ctx, projectID, p.ID, player, p.Version, true, party.Member{})
		if !assert.NoError(t, err) {
			return
		}
	}
	version := p.Version
	p, err = store.Queue(ctx, projectID, p.ID, leader, version, "rematch", previous, time.Minute)
	if !assert.NoError(t, err) {
		return
	}
	replay, err := store.Queue(ctx, projectID, p.ID, leader, version, "rematch", previous, time.Minute)
	assert.NoError(t, err)
	assert.Equal(t, p.CurrentQueueEntryID, replay.CurrentQueueEntryID)
	if !assert.NoError(t, worker.Tick(ctx)) {
		return
	}
	p, err = store.Get(ctx, projectID, p.ID, leader)
	if !assert.NoError(t, err) {
		return
	}
	assert.NotEqual(t, previous, p.LastMatchID)
	assert.Equal(t, match.Roster[0].PartyID, p.ID)

}

func TestPartyDisconnectCancelsEntryAndPromotesLeader(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "disconnect", "leader")
	_, _, friend := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "friend")
	ctx := db.WithTenant(context.Background(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 2, MaxCount: 2, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	code, err := store.CreateCode(ctx, projectID, p.ID, leader, p.Version, 1)
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.JoinCode(ctx, projectID, friend, code.PartyVersion, code.Code, "127.0.0.1")
	if !assert.NoError(t, err) {
		return
	}
	for _, player := range []int64{leader, friend} {
		p, err = store.Ready(ctx, projectID, p.ID, player, p.Version, true, party.Member{})
		if !assert.NoError(t, err) {
			return
		}
	}
	p, err = store.Queue(ctx, projectID, p.ID, leader, p.Version, "queue", "", time.Minute)
	if !assert.NoError(t, err) {
		return
	}
	_, err = pool.Exec(ctx, `UPDATE party_members SET disconnect_deadline=now()-interval '1 second' WHERE player_id=$1`, leader)
	if !assert.NoError(t, err) {
		return
	}
	assert.NoError(t, store.Sweep(ctx))
	p, err = store.Get(ctx, projectID, p.ID, friend)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, friend, p.LeaderID)
	assert.Equal(t, "idle", p.State)
	assert.Len(t, p.Members, 1)
	assert.Zero(t, p.Members[0].ReadyVersion)
}

func TestPartyCodesBlockAfterTenFailures(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, player := seedTenantProjectPlayer(t, pool, "code-limit", "player")
	ctx := db.WithTenant(context.Background(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	for range 10 {
		_, err := store.JoinCode(ctx, projectID, player, 1, "0000000000000000", "127.0.0.1")
		assert.ErrorIs(t, err, party.ErrInvite)
	}
	_, err := store.JoinCode(ctx, projectID, player, 1, "0000000000000000", "127.0.0.2")
	assert.ErrorIs(t, err, party.ErrCooldown)
}

func TestPartyMutationsRequireVersionAndLeader(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "controls", "leader")
	_, _, friend := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "friend")
	ctx := db.WithTenant(context.Background(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 2, MaxCount: 2, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	code, err := store.CreateCode(ctx, projectID, p.ID, leader, p.Version, 1)
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.JoinCode(ctx, projectID, friend, code.PartyVersion, code.Code, "127.0.0.1")
	if !assert.NoError(t, err) {
		return
	}
	_, err = store.Update(ctx, projectID, p.ID, friend, p.Version, p.Settings)
	assert.ErrorIs(t, err, party.ErrNotLeader)
	_, err = store.Ready(ctx, projectID, p.ID, leader, p.Version-1, true, party.Member{})
	assert.ErrorIs(t, err, party.ErrStale)
	p, err = store.Ready(ctx, projectID, p.ID, leader, p.Version, true, party.Member{})
	if !assert.NoError(t, err) {
		return
	}
	before := p.RosterVersion
	p, err = store.Heartbeat(ctx, projectID, p.ID, leader, p.Version)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, before, p.Members[0].ReadyVersion)
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	_, err = q.Enqueue(ctx, matchmaker.EnqueueRequest{TenantID: tenantID, ProjectID: projectID, PlayerID: leader, Mode: matchmaker.ModeMatchOnly})
	assert.ErrorIs(t, err, matchmaker.ErrPartyMember)
	_, err = store.Get(ctx, projectID+1, p.ID, leader)
	assert.ErrorIs(t, err, party.ErrNotFound)
}

func TestPartyInviteRequiresAcceptedFriend(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "friend-invite", "leader")
	_, _, friend := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "friend")
	ctx := db.WithTenant(context.Background(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 2, MaxCount: 2, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	_, err = store.InviteFriend(ctx, projectID, p.ID, leader, p.Version, friend)
	assert.ErrorIs(t, err, party.ErrInvite)
	for _, player := range []int64{leader, friend} {
		_, err = pool.Exec(ctx, `WITH account AS (INSERT INTO player_accounts(email,password_hash) VALUES('party-'||$1::bigint::text||'@example.test','test'::bytea) RETURNING id) UPDATE project_players SET player_account_id=(SELECT id FROM account) WHERE id=$1`, player)
		if !assert.NoError(t, err) {
			return
		}
	}
	_, err = pool.Exec(ctx, `INSERT INTO friend_edges(from_account_id,to_account_id,status) SELECT a.player_account_id,b.player_account_id,'accepted' FROM project_players a,project_players b WHERE a.id=$1 AND b.id=$2`, leader, friend)
	if !assert.NoError(t, err) {
		return
	}
	invite, err := store.InviteFriend(ctx, projectID, p.ID, leader, p.Version, friend)
	if !assert.NoError(t, err) {
		return
	}
	invites, err := store.Invites(ctx, projectID, friend)
	if !assert.NoError(t, err) {
		return
	}
	assert.Len(t, invites, 1)
	p, err = store.ResolveInvite(ctx, projectID, invite.ID, friend, invite.PartyVersion, true)
	if !assert.NoError(t, err) {
		return
	}
	assert.Len(t, p.Members, 2)
}

func TestPartyResolutionIsDurableBeforeBackendWork(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, _ := seedTenantProjectPlayer(t, pool, "resolution", "player")
	ctx := db.WithTenant(context.Background(), tenantID)
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	m := &matchmaker.Match{ID: "mm_resolution", TenantID: tenantID, ProjectID: projectID, Mode: matchmaker.ModeFleetAllocation, ExpiresAt: time.Now().Add(time.Minute)}
	if !assert.NoError(t, q.BeginResolution(ctx, m)) {
		return
	}
	var found bool
	err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM matchmaking_resolutions WHERE id=$1 AND tenant_id=$2 AND project_id=$3)`, m.ID, tenantID, projectID).Scan(&found)
	assert.NoError(t, err)
	assert.True(t, found)
}

func TestPartyCapacityRaceAdmitsOnlyOneLastMember(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "capacity-race", "leader")
	_, _, a := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "a")
	_, _, b := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "b")
	ctx := db.WithTenant(context.Background(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 2, MaxCount: 2, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	_, err = pool.Exec(ctx, `UPDATE parties SET max_members=2 WHERE id=$1`, p.ID)
	if !assert.NoError(t, err) {
		return
	}
	code, err := store.CreateCode(ctx, projectID, p.ID, leader, p.Version, 2)
	if !assert.NoError(t, err) {
		return
	}
	results := make(chan error, 2)
	for _, player := range []int64{a, b} {
		go func() {
			_, err := store.JoinCode(ctx, projectID, player, code.PartyVersion, code.Code, "127.0.0.1")
			results <- err
		}()
	}
	first, second := <-results, <-results
	if first == nil {
		assert.True(t, errors.Is(second, party.ErrFull) || errors.Is(second, party.ErrStale))
	} else {
		assert.True(t, errors.Is(first, party.ErrFull) || errors.Is(first, party.ErrStale))
		assert.NoError(t, second)
	}
}

func TestPartyCancelPreventsClaimCommit(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "cancel-race", "leader")
	ctx := db.WithTenant(context.Background(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 1, MaxCount: 1, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Ready(ctx, projectID, p.ID, leader, p.Version, true, party.Member{})
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Queue(ctx, projectID, p.ID, leader, p.Version, "cancel", "", time.Minute)
	if !assert.NoError(t, err) {
		return
	}
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	buckets, err := q.ListReadyBuckets(ctx)
	if !assert.NoError(t, err) || !assert.Len(t, buckets, 1) {
		return
	}
	claim, err := q.ClaimBucket(ctx, buckets[0], 1, time.Minute)
	if !assert.NoError(t, err) || !assert.NotNil(t, claim) {
		return
	}
	_, err = store.Cancel(ctx, projectID, p.ID, leader, p.Version)
	if !assert.NoError(t, err) {
		return
	}
	m := &matchmaker.Match{ID: "mm_cancelled", TenantID: tenantID, ProjectID: projectID, Mode: matchmaker.ModeMatchOnly, ExpiresAt: time.Now().Add(time.Minute)}
	n, err := q.CommitMatch(ctx, claim, []int64{claim.Tickets[0].ID}, m)
	assert.NoError(t, err)
	assert.Zero(t, n)
	_, err = q.GetMatch(ctx, m.ID)
	assert.ErrorIs(t, err, matchmaker.ErrNotFound, "a failed commit must roll back the match row")
}

func TestPartyResolvesGameSessionAndFleetModes(t *testing.T) {
	for _, mode := range []string{"game_session", "fleet_allocation"} {
		t.Run(mode, func(t *testing.T) {
			pool := startMigratedDB(t)
			tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "party-mode", "leader")
			_, _, friend := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "friend")
			ctx := db.WithTenant(context.Background(), tenantID)
			appPool := db.NewPool(pool)
			store := party.NewStore(appPool)
			cfg := party.Settings{Mode: mode, MinCount: 2, MaxCount: 2, CountMultiple: 1}
			if mode == "fleet_allocation" {
				err := pool.QueryRow(ctx, `INSERT INTO fleets(tenant_id,project_id,name,backend,config) VALUES($1,$2,'party-fleet','fake','{}') RETURNING id`, tenantID, projectID).Scan(&cfg.FleetID)
				if !assert.NoError(t, err) {
					return
				}
			}
			p, err := store.Create(ctx, projectID, leader, cfg)
			if !assert.NoError(t, err) {
				return
			}
			code, err := store.CreateCode(ctx, projectID, p.ID, leader, p.Version, 1)
			if !assert.NoError(t, err) {
				return
			}
			p, err = store.JoinCode(ctx, projectID, friend, code.PartyVersion, code.Code, "127.0.0.1")
			if !assert.NoError(t, err) {
				return
			}
			for _, player := range []int64{leader, friend} {
				p, err = store.Ready(ctx, projectID, p.ID, player, p.Version, true, party.Member{})
				if !assert.NoError(t, err) {
					return
				}
			}
			p, err = store.Queue(ctx, projectID, p.ID, leader, p.Version, "mode", "", time.Minute)
			if !assert.NoError(t, err) {
				return
			}
			q := matchmaker.NewPGQueue(appPool)
			worker := matchmaker.NewWorker(q, &allocatorRecorder{pool: pool, address: "127.0.0.1:7777"}, nil, matchmaker.WorkerConfig{Sessions: gamesession.NewMatchAdapter(gamesession.NewService(appPool))})
			if !assert.NoError(t, worker.Tick(ctx)) {
				return
			}
			p, err = store.Get(ctx, projectID, p.ID, leader)
			if !assert.NoError(t, err) {
				return
			}
			assert.Equal(t, "matched", p.State)
			m, err := q.GetMatch(ctx, p.LastMatchID)
			if !assert.NoError(t, err) {
				return
			}
			assert.Len(t, m.Roster, 2)
			assert.Equal(t, p.ID, m.Roster[0].PartyID)
			if mode == "game_session" {
				assert.NotEmpty(t, m.SessionID)
			} else {
				assert.NotZero(t, m.AllocationID)
			}
		})
	}
}

func TestPartyOrphanAllocationIsRecoveredAfterCrash(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, _ := seedTenantProjectPlayer(t, pool, "orphan", "player")
	ctx := db.WithTenant(context.Background(), tenantID)
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	m := &matchmaker.Match{ID: "mm_crashed", TenantID: tenantID, ProjectID: projectID, ExpiresAt: time.Now().Add(-time.Minute)}
	if !assert.NoError(t, q.BeginResolution(ctx, m)) {
		return
	}
	var fleetID int64
	err := pool.QueryRow(ctx, `INSERT INTO fleets(tenant_id,project_id,name,backend,config) VALUES($1,$2,'orphan-fleet','fake','{}') RETURNING id`, tenantID, projectID).Scan(&fleetID)
	if !assert.NoError(t, err) {
		return
	}
	var allocationID int64
	err = pool.QueryRow(ctx, `INSERT INTO game_server_allocations(tenant_id,project_id,fleet_id,backend,status,metadata) VALUES($1,$2,$4,'fake','ready',jsonb_build_object('ggscale.dev/resolution-id',$3::text)) RETURNING id`, tenantID, projectID, m.ID, fleetID).Scan(&allocationID)
	if !assert.NoError(t, err) {
		return
	}
	allocator := &allocatorRecorder{pool: pool}
	appCfg := pool.Config().Copy()
	appCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appPool, err := pgxpool.NewWithConfig(ctx, appCfg)
	if !assert.NoError(t, err) {
		return
	}
	defer appPool.Close()
	assert.NoError(t, jobs.SweepMatchmakerRecords(ctx, db.NewPool(appPool), allocator))
	var state string
	assert.NoError(t, pool.QueryRow(ctx, `SELECT status FROM game_server_allocations WHERE id=$1`, allocationID).Scan(&state))
	assert.Equal(t, "shutdown", state)
}

func TestPartyCodeIPBudgetAppliesAcrossProjects(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, _ := seedTenantProjectPlayer(t, pool, "global-ip", "leader")
	ctx := db.WithTenant(context.Background(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	for i := range 10 {
		_, _, player := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, fmt.Sprintf("attempt-%d", i))
		for range 10 {
			_, err := store.JoinCode(ctx, projectID, player, 1, "0000000000000000", "127.0.0.8")
			if !assert.ErrorIs(t, err, party.ErrInvite) {
				return
			}
		}
	}
	otherTenant, otherProject, otherPlayer := seedTenantProjectPlayer(t, pool, "other-project", "player")
	_, err := store.JoinCode(db.WithTenant(ctx, otherTenant), otherProject, otherPlayer, 1, "0000000000000000", "127.0.0.8")
	assert.ErrorIs(t, err, party.ErrCooldown)
}

func TestPartyCutoverBackfillsExistingSoloTickets(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, player := seedTenantProjectPlayer(t, pool, "cutover", "player")
	ctx := t.Context()
	down, err := os.ReadFile("../../../db/migrations/0046_party_queue_cutover.down.sql")
	if !assert.NoError(t, err) {
		return
	}
	_, err = pool.Exec(ctx, string(down))
	if !assert.NoError(t, err) {
		return
	}
	_, err = pool.Exec(ctx, `INSERT INTO matchmaking_tickets(tenant_id,project_id,player_id,mode) VALUES($1,$2,$3,'match_only')`, tenantID, projectID, player)
	if !assert.NoError(t, err) {
		return
	}
	up, err := os.ReadFile("../../../db/migrations/0046_party_queue_cutover.up.sql")
	if !assert.NoError(t, err) {
		return
	}
	_, err = pool.Exec(ctx, string(up))
	if !assert.NoError(t, err) {
		return
	}
	var backed bool
	err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM matchmaking_tickets t JOIN matchmaking_entries e ON e.id=t.entry_id WHERE t.player_id=$1 AND e.party_id IS NULL AND e.status='queued')`, player).Scan(&backed)
	assert.NoError(t, err)
	assert.True(t, backed)
}

func TestPartyExpiredPresencePreventsMatchCommitBeforeSweep(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "expired-commit", "leader")
	ctx := db.WithTenant(t.Context(), tenantID)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 1, MaxCount: 1, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Ready(ctx, projectID, p.ID, leader, p.Version, true, party.Member{})
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Queue(ctx, projectID, p.ID, leader, p.Version, "expiry", "", time.Minute)
	if !assert.NoError(t, err) {
		return
	}
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	buckets, err := q.ListReadyBuckets(ctx)
	if !assert.NoError(t, err) || !assert.Len(t, buckets, 1) {
		return
	}
	claim, err := q.ClaimBucket(ctx, buckets[0], 1, time.Minute)
	if !assert.NoError(t, err) || !assert.NotNil(t, claim) {
		return
	}
	_, err = pool.Exec(ctx, `UPDATE party_members SET disconnect_deadline=now()-interval '1 second' WHERE party_id=$1`, p.ID)
	if !assert.NoError(t, err) {
		return
	}
	m := &matchmaker.Match{ID: "mm_expired_presence", TenantID: tenantID, ProjectID: projectID, Mode: matchmaker.ModeMatchOnly, ExpiresAt: time.Now().Add(time.Minute)}

	_, err = q.CommitMatch(ctx, claim, []int64{claim.Tickets[0].ID}, m)

	assert.ErrorIs(t, err, matchmaker.ErrShortCommit)
	_, err = q.GetMatch(ctx, m.ID)
	assert.ErrorIs(t, err, matchmaker.ErrNotFound)
	assert.NoError(t, store.Sweep(ctx))
	var status string
	assert.NoError(t, pool.QueryRow(ctx, `SELECT status FROM matchmaking_entries WHERE id=$1`, p.CurrentQueueEntryID).Scan(&status))
	assert.Equal(t, "cancelled", status)
}

func TestPartyTenantIsolationUsesApplicationRole(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "party-owner", "leader")
	otherTenant, otherProject, otherPlayer := seedTenantProjectPlayer(t, pool, "party-other", "player")
	cfg := pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if !assert.NoError(t, err) {
		return
	}
	defer appPool.Close()
	store := party.NewStore(db.NewPool(appPool))
	ownerCtx := db.WithTenant(t.Context(), tenantID)
	p, err := store.Create(ownerCtx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 1, MaxCount: 1, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}

	_, err = store.Get(db.WithTenant(t.Context(), otherTenant), projectID, p.ID, leader)

	assert.ErrorIs(t, err, party.ErrNotFound)
	_, err = store.Create(ownerCtx, otherProject, otherPlayer, p.Settings)
	assert.ErrorIs(t, err, party.ErrNotFound)
}

func TestPartyMemberRemovalAndCommitStayConsistent(t *testing.T) {
	for _, action := range []string{"cancel", "kick", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			pool := startMigratedDB(t)
			tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "commit-race", "leader")
			_, _, friend := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "friend")
			ctx := db.WithTenant(t.Context(), tenantID)
			store := party.NewStore(db.NewPool(pool))
			p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 2, MaxCount: 2, CountMultiple: 1})
			if !assert.NoError(t, err) {
				return
			}
			code, err := store.CreateCode(ctx, projectID, p.ID, leader, p.Version, 1)
			if !assert.NoError(t, err) {
				return
			}
			p, err = store.JoinCode(ctx, projectID, friend, code.PartyVersion, code.Code, "127.0.0.1")
			if !assert.NoError(t, err) {
				return
			}
			for _, player := range []int64{leader, friend} {
				p, err = store.Ready(ctx, projectID, p.ID, player, p.Version, true, party.Member{})
				if !assert.NoError(t, err) {
					return
				}
			}
			p, err = store.Queue(ctx, projectID, p.ID, leader, p.Version, "race", "", time.Minute)
			if !assert.NoError(t, err) {
				return
			}
			q := matchmaker.NewPGQueue(db.NewPool(pool))
			buckets, err := q.ListReadyBuckets(ctx)
			if !assert.NoError(t, err) || !assert.Len(t, buckets, 1) {
				return
			}
			claim, err := q.ClaimBucket(ctx, buckets[0], 1, time.Minute)
			if !assert.NoError(t, err) || !assert.NotNil(t, claim) {
				return
			}
			if action == "disconnect" {
				_, err = pool.Exec(ctx, `UPDATE party_members SET disconnect_deadline=now()-interval '1 second' WHERE player_id=$1`, friend)
				if !assert.NoError(t, err) {
					return
				}
			}
			m := &matchmaker.Match{ID: "mm_race", TenantID: tenantID, ProjectID: projectID, Mode: matchmaker.ModeMatchOnly, ExpiresAt: time.Now().Add(time.Minute)}
			start := make(chan struct{})
			removed := make(chan error, 1)
			go func() {
				<-start
				var err error
				switch action {
				case "cancel":
					_, err = store.Cancel(ctx, projectID, p.ID, leader, p.Version)
				case "kick":
					_, err = store.Remove(ctx, projectID, p.ID, leader, p.Version, friend, false)
				case "disconnect":
					err = store.Sweep(ctx)
				}
				removed <- err
			}()

			close(start)
			n, commitErr := q.CommitMatch(ctx, claim, []int64{claim.Tickets[0].ID, claim.Tickets[1].ID}, m)
			removeErr := <-removed

			assert.True(t, commitErr == nil || errors.Is(commitErr, matchmaker.ErrShortCommit), "commit: %v", commitErr)
			assert.True(t, removeErr == nil || errors.Is(removeErr, party.ErrStale), "remove: %v", removeErr)
			var matched int64
			assert.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM matchmaking_tickets WHERE entry_id=$1 AND status='matched'`, p.CurrentQueueEntryID).Scan(&matched))
			assert.True(t, matched == 0 || matched == 2, "the party must never partly commit")
			assert.Equal(t, n, matched)
			_, err = q.GetMatch(ctx, m.ID)
			if matched == 0 {
				assert.ErrorIs(t, err, matchmaker.ErrNotFound)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
