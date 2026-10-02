//go:build integration

// e2e:bucket b

package matchmaker_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/party"
	"github.com/automoto/gg-scale/internal/realtime"
)

type pushed struct {
	tenantID, playerID int64
	msg                realtime.Message
}

type recordingPusher struct {
	mu   sync.Mutex
	sent []pushed
}

func (r *recordingPusher) PushMany(_ context.Context, tenantID int64, playerIDs []int64, msg realtime.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range playerIDs {
		r.sent = append(r.sent, pushed{tenantID, id, msg})
	}
	return nil
}

func (r *recordingPusher) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = nil
}

// players returns who got an event of type typ, sorted.
func (r *recordingPusher) players(typ string) []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int64
	for _, p := range r.sent {
		if p.msg.Type == typ {
			out = append(out, p.playerID)
		}
	}
	slices.Sort(out)
	return out
}

func (r *recordingPusher) last(typ string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.sent) - 1; i >= 0; i-- {
		if r.sent[i].msg.Type == typ {
			var out map[string]any
			_ = json.Unmarshal(r.sent[i].msg.Payload, &out)
			return out
		}
	}
	return nil
}

type partyFixture struct {
	pool              *pgxpool.Pool
	ctx               context.Context
	store             *party.Store
	pusher            *recordingPusher
	tenantID, project int64
	leader, friend    int64
	party             *party.Party
}

// newTwoMemberParty makes a party of a leader and a friend, then clears the
// events that the setup sent.
func newTwoMemberParty(t *testing.T, name string) *partyFixture {
	t.Helper()
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, name, "leader")
	_, _, friend := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "friend")
	ctx := db.WithTenant(context.Background(), tenantID)
	pusher := &recordingPusher{}
	store := party.NewStore(db.NewPool(pool)).WithPusher(pusher)
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 2, MaxCount: 2, CountMultiple: 1})
	require.NoError(t, err)
	code, err := store.CreateCode(ctx, projectID, p.ID, leader, p.Version, 1)
	require.NoError(t, err)
	p, err = store.JoinCode(ctx, projectID, friend, code.Code, "127.0.0.1")
	require.NoError(t, err)
	pusher.reset()
	return &partyFixture{pool: pool, ctx: ctx, store: store, pusher: pusher, tenantID: tenantID, project: projectID, leader: leader, friend: friend, party: p}
}

func (f *partyFixture) both() []int64 {
	out := []int64{f.leader, f.friend}
	slices.Sort(out)
	return out
}

func (f *partyFixture) readyAndQueue(t *testing.T) {
	t.Helper()
	var err error
	for _, player := range []int64{f.leader, f.friend} {
		f.party, err = f.store.Ready(f.ctx, f.project, f.party.ID, player, f.party.Version, true, party.Member{})
		require.NoError(t, err)
	}
	f.party, err = f.store.Queue(f.ctx, f.project, f.party.ID, f.leader, f.party.Version, "queue", "", time.Minute)
	require.NoError(t, err)
	f.pusher.reset()
}

func TestPartyEvents_should_send_changed_to_each_member_on_join(t *testing.T) {
	pool := startMigratedDB(t)
	tenantID, projectID, leader := seedTenantProjectPlayer(t, pool, "events-join", "leader")
	_, _, friend := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "friend")
	ctx := db.WithTenant(context.Background(), tenantID)
	pusher := &recordingPusher{}
	store := party.NewStore(db.NewPool(pool)).WithPusher(pusher)
	p, err := store.Create(ctx, projectID, leader, party.Settings{Mode: "match_only", MinCount: 2, MaxCount: 2, CountMultiple: 1})
	require.NoError(t, err)
	code, err := store.CreateCode(ctx, projectID, p.ID, leader, p.Version, 1)
	require.NoError(t, err)
	pusher.reset()

	_, err = store.JoinCode(ctx, projectID, friend, code.Code, "127.0.0.1")

	require.NoError(t, err)
	want := []int64{leader, friend}
	slices.Sort(want)
	assert.Equal(t, want, pusher.players(party.EventChanged))
}

func TestPartyEvents_should_carry_new_version_and_state(t *testing.T) {
	f := newTwoMemberParty(t, "events-payload")

	p, err := f.store.Ready(f.ctx, f.project, f.party.ID, f.leader, f.party.Version, true, party.Member{})

	require.NoError(t, err)
	assert.Equal(t, map[string]any{"party_id": float64(p.ID), "version": float64(p.Version), "state": "idle"}, f.pusher.last(party.EventChanged))
}

func TestPartyEvents_should_send_changed_to_kicked_member(t *testing.T) {
	f := newTwoMemberParty(t, "events-kick")

	_, err := f.store.Remove(f.ctx, f.project, f.party.ID, f.leader, f.party.Version, f.friend, false)

	require.NoError(t, err)
	assert.Equal(t, f.both(), f.pusher.players(party.EventChanged))
}

func TestPartyEvents_should_send_changed_to_all_on_disband(t *testing.T) {
	f := newTwoMemberParty(t, "events-disband")

	_, err := f.store.Remove(f.ctx, f.project, f.party.ID, f.leader, f.party.Version, f.leader, true)

	require.NoError(t, err)
	assert.Equal(t, f.both(), f.pusher.players(party.EventChanged))
}

func TestPartyEvents_should_not_send_on_heartbeat(t *testing.T) {
	f := newTwoMemberParty(t, "events-heartbeat")

	_, err := f.store.Heartbeat(f.ctx, f.project, f.party.ID, f.friend, f.party.Version)

	require.NoError(t, err)
	assert.Empty(t, f.pusher.players(party.EventChanged))
}

func TestPartyEvents_should_not_send_when_write_fails(t *testing.T) {
	f := newTwoMemberParty(t, "events-stale")

	_, err := f.store.Ready(f.ctx, f.project, f.party.ID, f.leader, f.party.Version-1, true, party.Member{})

	require.ErrorIs(t, err, party.ErrStale)
	assert.Empty(t, f.pusher.players(party.EventChanged))
}

func TestPartyEvents_should_send_invite_to_friend(t *testing.T) {
	f := newTwoMemberParty(t, "events-invite")
	_, _, guest := seedTenantProjectPlayerInto(t, f.pool, f.tenantID, f.project, "guest")
	for _, player := range []int64{f.leader, guest} {
		_, err := f.pool.Exec(f.ctx, `WITH account AS (INSERT INTO player_accounts(email,password_hash) VALUES('party-'||$1::bigint::text||'@example.test','test'::bytea) RETURNING id) UPDATE project_players SET player_account_id=(SELECT id FROM account) WHERE id=$1`, player)
		require.NoError(t, err)
	}
	_, err := f.pool.Exec(f.ctx, `INSERT INTO friend_edges(from_account_id,to_account_id,status) SELECT a.player_account_id,b.player_account_id,'accepted' FROM project_players a,project_players b WHERE a.id=$1 AND b.id=$2`, f.leader, guest)
	require.NoError(t, err)

	invite, err := f.store.InviteFriend(f.ctx, f.project, f.party.ID, f.leader, f.party.Version, guest)

	require.NoError(t, err)
	assert.Equal(t, []int64{guest}, f.pusher.players(party.EventInvite))
	assert.Equal(t, map[string]any{"invite_id": float64(invite.ID), "party_id": float64(f.party.ID), "from_player_id": float64(f.leader)}, f.pusher.last(party.EventInvite))
}

func TestPartyEvents_should_send_changed_to_removed_member_on_sweep(t *testing.T) {
	f := newTwoMemberParty(t, "events-sweep")
	_, err := f.pool.Exec(f.ctx, `UPDATE party_members SET disconnect_deadline=now()-interval '1 second' WHERE player_id=$1`, f.friend)
	require.NoError(t, err)

	err = f.store.Sweep(f.ctx)

	require.NoError(t, err)
	assert.Equal(t, f.both(), f.pusher.players(party.EventChanged))
}

func TestPartyEvents_should_send_changed_when_queue_entry_expires(t *testing.T) {
	f := newTwoMemberParty(t, "events-expire")
	f.readyAndQueue(t)
	_, err := f.pool.Exec(f.ctx, `UPDATE matchmaking_tickets SET expires_at=now()-interval '1 second' WHERE party_id=$1`, f.party.ID)
	require.NoError(t, err)
	q := matchmaker.NewPGQueue(db.NewPool(f.pool)).WithPartyEvents(f.pusher)

	_, err = q.SweepStaleClaims(f.ctx, 3)

	require.NoError(t, err)
	assert.Equal(t, f.both(), f.pusher.players(party.EventChanged))
	assert.Equal(t, "idle", f.pusher.last(party.EventChanged)["state"])
}

func TestPartyEvents_should_send_changed_when_party_matches(t *testing.T) {
	f := newTwoMemberParty(t, "events-match")
	f.readyAndQueue(t)
	q := matchmaker.NewPGQueue(db.NewPool(f.pool)).WithPartyEvents(f.pusher)
	worker := matchmaker.NewWorker(q, nil, nil, matchmaker.WorkerConfig{})

	err := worker.Tick(context.Background())

	require.NoError(t, err)
	assert.Equal(t, f.both(), f.pusher.players(party.EventChanged))
	assert.Equal(t, "matched", f.pusher.last(party.EventChanged)["state"])
}
