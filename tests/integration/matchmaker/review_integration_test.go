//go:build integration

// e2e:bucket b

package matchmaker_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/jobs"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/party"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
)

func TestMatchedPartyCanReturnToIdle(t *testing.T) {
	pool := startMigratedDB(t)
	tenant, project, leader := seedTenantProjectPlayer(t, pool, "return-idle", "leader")
	_, _, friend := seedTenantProjectPlayerInto(t, pool, tenant, project, "friend")
	ctx := db.WithTenant(t.Context(), tenant)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, project, leader, party.Settings{Mode: "match_only", MinCount: 1, MaxCount: 2, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Ready(ctx, project, p.ID, leader, p.Version, true, party.Member{})
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Queue(ctx, project, p.ID, leader, p.Version, "match", "", time.Minute)
	if !assert.NoError(t, err) {
		return
	}
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	if !assert.NoError(t, matchmaker.NewWorker(q, nil, nil, matchmaker.WorkerConfig{}).Tick(ctx)) {
		return
	}
	p, err = store.Get(ctx, project, p.ID, leader)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, "matched", p.State)
	previous := p.LastMatchID

	p, err = store.Cancel(ctx, project, p.ID, leader, p.Version)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, "idle", p.State)
	assert.Equal(t, previous, p.LastMatchID)
	assert.Nil(t, p.CurrentQueueEntryID)
	p, err = store.Update(ctx, project, p.ID, leader, p.Version, p.Settings)
	if !assert.NoError(t, err) {
		return
	}
	code, err := store.CreateCode(ctx, project, p.ID, leader, p.Version, 1)
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.JoinCode(ctx, project, friend, code.Code, "127.0.0.1")
	if assert.NoError(t, err) {
		assert.Len(t, p.Members, 2)
	}
	_, err = q.GetMatch(ctx, previous)
	assert.NoError(t, err)
}

func TestPartyQueueRejectsTerminalReplay(t *testing.T) {
	for _, status := range []string{"cancelled", "matched", "failed"} {
		t.Run(status, func(t *testing.T) {
			pool := startMigratedDB(t)
			tenant, project, leader := seedTenantProjectPlayer(t, pool, "replay", "leader")
			ctx := db.WithTenant(t.Context(), tenant)
			store := party.NewStore(db.NewPool(pool))
			p, err := store.Create(ctx, project, leader, party.Settings{Mode: "match_only", MinCount: 1, MaxCount: 1, CountMultiple: 1})
			if !assert.NoError(t, err) {
				return
			}
			p, err = store.Ready(ctx, project, p.ID, leader, p.Version, true, party.Member{})
			if !assert.NoError(t, err) {
				return
			}
			version := p.Version
			p, err = store.Queue(ctx, project, p.ID, leader, version, "replay", "", time.Minute)
			if !assert.NoError(t, err) {
				return
			}
			p, err = store.Cancel(ctx, project, p.ID, leader, p.Version)
			if !assert.NoError(t, err) {
				return
			}
			_, err = pool.Exec(ctx, `UPDATE matchmaking_entries SET status=$2::ticket_status WHERE party_id=$1`, p.ID, status)
			if !assert.NoError(t, err) {
				return
			}
			for _, v := range []int64{version, p.Version} {
				_, err = store.Queue(ctx, project, p.ID, leader, v, "replay", "", time.Minute)
				assert.ErrorIs(t, err, party.ErrStale)
			}
		})
	}
}

func TestClaimBucketBoundsPlayersWithoutSplittingParties(t *testing.T) {
	pool := startMigratedDB(t)
	tenant, project, _ := seedTenantProjectPlayer(t, pool, "claim-budget", "unused")
	ctx := db.WithTenant(t.Context(), tenant)
	// Ten entries with eight tickets each model a full party queue.
	for entry := range 10 {
		var id int64
		if !assert.NoError(t, pool.QueryRow(ctx, `INSERT INTO matchmaking_entries(tenant_id,project_id) VALUES($1,$2) RETURNING id`, tenant, project).Scan(&id)) {
			return
		}
		for member := range 8 {
			_, _, player := seedTenantProjectPlayerInto(t, pool, tenant, project, fmt.Sprintf("p-%d-%d", entry, member))
			_, err := pool.Exec(ctx, `INSERT INTO matchmaking_tickets(tenant_id,project_id,player_id,entry_id,mode) VALUES($1,$2,$3,$4,'match_only')`, tenant, project, player, id)
			if !assert.NoError(t, err) {
				return
			}
		}
	}
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	bucket := matchmaker.Bucket{TenantID: tenant, ProjectID: project, Mode: matchmaker.ModeMatchOnly}
	claim, err := q.ClaimBucket(ctx, bucket, 17, time.Minute)
	if !assert.NoError(t, err) || !assert.NotNil(t, claim) {
		return
	}
	assert.Len(t, claim.Tickets, 16)
	counts := map[int64]int{}
	for _, ticket := range claim.Tickets {
		counts[ticket.EntryID]++
	}
	for _, count := range counts {
		assert.Equal(t, 8, count)
	}
	second, err := q.ClaimBucket(ctx, bucket, 17, time.Minute)
	if assert.NoError(t, err) && assert.NotNil(t, second) {
		assert.Len(t, second.Tickets, 16)
	}
}

func TestListReadyBucketsSurvivesPartySweepFailure(t *testing.T) {
	pool := startMigratedDB(t)
	tenant, project, leader := seedTenantProjectPlayer(t, pool, "sweep-failure", "leader")
	_, _, solo := seedTenantProjectPlayerInto(t, pool, tenant, project, "solo")
	ctx := db.WithTenant(t.Context(), tenant)
	store := party.NewStore(db.NewPool(pool))
	_, err := store.Create(ctx, project, leader, party.Settings{Mode: "match_only", MinCount: 1, MaxCount: 1, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	_, err = pool.Exec(ctx, `UPDATE party_members SET disconnect_deadline=now()-interval '1 second';
 CREATE FUNCTION reject_party_sweep() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test sweep failure'; END $$;
 CREATE TRIGGER reject_party_sweep BEFORE UPDATE ON parties FOR EACH ROW EXECUTE FUNCTION reject_party_sweep()`)
	if !assert.NoError(t, err) {
		return
	}
	q := matchmaker.NewPGQueue(db.NewPool(pool))
	_, err = q.Enqueue(ctx, matchmaker.EnqueueRequest{TenantID: tenant, ProjectID: project, PlayerID: solo, Mode: matchmaker.ModeMatchOnly})
	if !assert.NoError(t, err) {
		return
	}
	assert.Error(t, store.Sweep(ctx))

	buckets, err := q.ListReadyBuckets(ctx)
	assert.NoError(t, err)
	assert.Len(t, buckets, 1)
}

func TestMatchmakerGCRetainsLiveEntriesAndDeletesOldClosedParties(t *testing.T) {
	pool := startMigratedDB(t)
	tenant, project, leader := seedTenantProjectPlayer(t, pool, "entry-gc", "leader")
	ctx := db.WithTenant(t.Context(), tenant)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, project, leader, party.Settings{Mode: "match_only", MinCount: 1, MaxCount: 1, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	code, err := store.CreateCode(ctx, project, p.ID, leader, p.Version, 1)
	if !assert.NoError(t, err) {
		return
	}
	p, err = store.Remove(ctx, project, p.ID, leader, code.PartyVersion, leader, true)
	if !assert.NoError(t, err) {
		return
	}
	_, err = pool.Exec(ctx, `UPDATE parties SET closed_at=now()-interval '25 hours' WHERE id=$1`, p.ID)
	if !assert.NoError(t, err) {
		return
	}
	for _, status := range []string{"cancelled", "failed", "matched", "queued"} {
		_, err = pool.Exec(ctx, `INSERT INTO matchmaking_entries(tenant_id,project_id,status,created_at) VALUES($1,$2,$3::ticket_status,now()-interval '25 hours')`, tenant, project, status)
		if !assert.NoError(t, err) {
			return
		}
	}
	_, err = pool.Exec(ctx, `INSERT INTO matchmaking_entries(tenant_id,project_id,status) VALUES($1,$2,'cancelled')`, tenant, project)
	if !assert.NoError(t, err) {
		return
	}
	// A recent ticket keeps its old entry until ticket retention has elapsed.
	var retained int64
	if !assert.NoError(t, pool.QueryRow(ctx, `INSERT INTO matchmaking_entries(tenant_id,project_id,status,created_at) VALUES($1,$2,'matched',now()-interval '25 hours') RETURNING id`, tenant, project).Scan(&retained)) {
		return
	}
	_, err = pool.Exec(ctx, `INSERT INTO matchmaking_tickets(tenant_id,project_id,player_id,entry_id,mode,status,matched_at) VALUES($1,$2,$3,$4,'match_only','matched',now())`, tenant, project, leader, retained)
	if !assert.NoError(t, err) {
		return
	}

	appConfig := pool.Config().Copy()
	appConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appPool, err := pgxpool.NewWithConfig(ctx, appConfig)
	if !assert.NoError(t, err) {
		return
	}
	defer appPool.Close()
	assert.NoError(t, jobs.SweepMatchmakerRecords(ctx, db.NewPool(appPool), nil))
	var entries, parties int
	assert.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM matchmaking_entries`).Scan(&entries))
	assert.Equal(t, 3, entries)
	assert.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM parties`).Scan(&parties))
	assert.Zero(t, parties)
}

func TestSuccessfulPartyCodeJoinClearsPlayerFailures(t *testing.T) {
	pool := startMigratedDB(t)
	tenant, project, leader := seedTenantProjectPlayer(t, pool, "reset-failures", "leader")
	_, _, friend := seedTenantProjectPlayerInto(t, pool, tenant, project, "friend")
	ctx := db.WithTenant(t.Context(), tenant)
	store := party.NewStore(db.NewPool(pool))
	p, err := store.Create(ctx, project, leader, party.Settings{Mode: "match_only", MinCount: 1, MaxCount: 2, CountMultiple: 1})
	if !assert.NoError(t, err) {
		return
	}
	code, err := store.CreateCode(ctx, project, p.ID, leader, p.Version, 1)
	if !assert.NoError(t, err) {
		return
	}
	for range 9 {
		_, err = store.JoinCode(ctx, project, friend, "invalid", "127.0.0.1")
		if !assert.ErrorIs(t, err, party.ErrInvite) {
			return
		}
	}
	_, err = store.JoinCode(ctx, project, friend, code.Code, "127.0.0.1")
	if !assert.NoError(t, err) {
		return
	}
	var failures int
	assert.NoError(t, pool.QueryRow(ctx, `SELECT failures FROM party_code_attempts WHERE project_id=$1 AND subject=$2`, project, fmt.Sprintf("player:%d", friend)).Scan(&failures))
	assert.Zero(t, failures)
}
