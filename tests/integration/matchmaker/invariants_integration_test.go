//go:build integration

package matchmaker_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/matchmaker"
)

// matchOnlyBucket is the region-blanked bucket for match_only tickets.
func matchOnlyBucket(tenantID, projectID int64, gameMode string) matchmaker.Bucket {
	return matchmaker.Bucket{TenantID: tenantID, ProjectID: projectID, Mode: matchmaker.ModeMatchOnly, GameMode: gameMode}
}

// TestPGQueueDuplicateEnqueueRejectedByOneActiveIndex is the GA exit criterion
// "a player can't hold two queued tickets", verified against the real
// 0021 partial unique index: the second enqueue surfaces a *TicketActiveError
// carrying the active ticket id (mapped to 409 at the HTTP layer).
func TestPGQueueDuplicateEnqueueRejectedByOneActiveIndex(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, playerID := seedTenantProjectPlayer(t, pool, "mm-dup", "dup-p1")
	tctx := db.WithTenant(context.Background(), tenantID)

	first, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1",
	})
	require.NoError(t, err)

	_, err = queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1",
	})
	require.ErrorIs(t, err, matchmaker.ErrTicketActive)
	var active *matchmaker.TicketActiveError
	require.ErrorAs(t, err, &active)
	assert.Equal(t, first.ID, active.ActiveTicketID, "409 points at the ticket to cancel")
}

// TestPGQueueExpiredTicketReQueueSucceeds covers the recent enqueue-path fix on
// the real index: an expired-but-unswept ticket is TTL-expired in-tx so the
// player can re-queue immediately; the stale row is failed/expired, the new
// row queued.
func TestPGQueueExpiredTicketReQueueSucceeds(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, playerID := seedTenantProjectPlayer(t, pool, "mm-req", "req-p1")
	tctx := db.WithTenant(context.Background(), tenantID)

	past := time.Now().UTC().Add(-time.Minute)
	first, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1", ExpiresAt: &past,
	})
	require.NoError(t, err)

	second, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1",
	})
	require.NoError(t, err, "re-queue must succeed once the stale ticket is TTL-expired")
	assert.NotEqual(t, first.ID, second.ID)

	stale, err := queue.Get(tctx, first.ID, playerID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, stale.Status)
	assert.Equal(t, "expired", stale.FailureReason)

	fresh, err := queue.Get(tctx, second.ID, playerID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, fresh.Status)
}

// TestPGQueueClaimedExpiredTicketStillBlocksReQueue is the claim_id IS NULL
// guard: a ticket that was claimed before its TTL lapsed is NOT auto-expired by
// a re-enqueue (the claim path must settle it), so the second enqueue is still
// rejected. Prevents a mid-negotiation player from opening a second ticket.
func TestPGQueueClaimedExpiredTicketStillBlocksReQueue(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, playerID := seedTenantProjectPlayer(t, pool, "mm-clx", "clx-p1")
	tctx := db.WithTenant(context.Background(), tenantID)

	soon := time.Now().UTC().Add(250 * time.Millisecond)
	first, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1", ExpiresAt: &soon,
	})
	require.NoError(t, err)

	// Claim it while still live, then let its TTL lapse: now claimed + expired.
	claim, err := queue.ClaimBucket(context.Background(), matchOnlyBucket(tenantID, projectID, "1v1"), 1, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	time.Sleep(400 * time.Millisecond)

	_, err = queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1",
	})
	require.ErrorIs(t, err, matchmaker.ErrTicketActive)
	var active *matchmaker.TicketActiveError
	require.ErrorAs(t, err, &active)
	assert.Equal(t, first.ID, active.ActiveTicketID)
}

// TestPGQueueCancelDuringFinalizeIsAllOrNone is the partial-commit invariant: a
// member cancelled between claim and commit rolls the whole commit back — no
// ticket flips to matched, and the survivor stays queued to rematch.
func TestPGQueueCancelDuringFinalizeIsAllOrNone(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, keepPlayer := seedTenantProjectPlayer(t, pool, "mm-cxl", "cxl-keep")
	_, _, dropPlayer := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "cxl-drop")
	tctx := db.WithTenant(context.Background(), tenantID)

	keep, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: keepPlayer,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1", MinCount: 2, MaxCount: 2,
	})
	require.NoError(t, err)
	drop, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: dropPlayer,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1", MinCount: 2, MaxCount: 2,
	})
	require.NoError(t, err)

	claim, err := queue.ClaimBucket(context.Background(), matchOnlyBucket(tenantID, projectID, "1v1"), 2, time.Minute)
	require.NoError(t, err)
	require.Len(t, claim.Tickets, 2)

	// One member cancels between claim and commit.
	require.NoError(t, queue.Cancel(tctx, drop.ID, dropPlayer))

	n, err := queue.CommitTickets(context.Background(), claim, []int64{keep.ID, drop.ID}, "mm_partial", "", "")
	require.ErrorIs(t, err, matchmaker.ErrShortCommit)
	assert.Equal(t, int64(1), n, "would-be commit count")

	keepGot, err := queue.Get(tctx, keep.ID, keepPlayer)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, keepGot.Status, "survivor is not matched")
	assert.Empty(t, keepGot.MatchID)

	dropGot, err := queue.Get(tctx, drop.ID, dropPlayer)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusCancelled, dropGot.Status)
}

// TestPGQueueAbandonedClaimReclaimedBySweeper is the worker-kill-at-the-claim
// -boundary invariant: a claim staked but never committed (crashed worker) is
// reclaimed once its lease lapses, the ticket returns to queued with no partial
// match, and the next worker can pick it up.
func TestPGQueueAbandonedClaimReclaimedBySweeper(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, playerID := seedTenantProjectPlayer(t, pool, "mm-kill", "kill-p1")
	tctx := db.WithTenant(context.Background(), tenantID)

	ticket, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1",
	})
	require.NoError(t, err)

	// Worker claims with a short lease, then "dies" — never commits or releases.
	claim, err := queue.ClaimBucket(context.Background(), matchOnlyBucket(tenantID, projectID, "1v1"), 1, 50*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, claim)
	time.Sleep(120 * time.Millisecond)

	n, err := queue.SweepStaleClaims(context.Background(), 5)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	got, err := queue.Get(tctx, ticket.ID, playerID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status, "reclaimed ticket is queued again")
	assert.Empty(t, got.MatchID, "no partial match was produced")

	// Re-claimable by the next worker.
	reclaim, err := queue.ClaimBucket(context.Background(), matchOnlyBucket(tenantID, projectID, "1v1"), 1, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reclaim)
	assert.Len(t, reclaim.Tickets, 1)
}

// TestPGQueueMissedPushRecoveredByPolling is the poll-recovery invariant: a
// match committed with the realtime hub suppressed (nil) is still recoverable —
// every peer's ClaimMatch returns the full roster, so a dropped WebSocket push
// never strands a match.
func TestPGQueueMissedPushRecoveredByPolling(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, p1 := seedTenantProjectPlayer(t, pool, "mm-poll", "poll-p1")
	_, _, p2 := seedTenantProjectPlayerInto(t, pool, tenantID, projectID, "poll-p2")
	tctx := db.WithTenant(context.Background(), tenantID)

	for _, pid := range []int64{p1, p2} {
		_, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
			TenantID: tenantID, ProjectID: projectID, PlayerID: pid,
			Mode: matchmaker.ModeMatchOnly, GameMode: "1v1", MinCount: 2, MaxCount: 2,
		})
		require.NoError(t, err)
	}

	// nil hub: pushes are suppressed, exactly as a fully offline roster.
	w := matchmaker.NewWorker(queue, nil, nil, matchmaker.WorkerConfig{})
	require.NoError(t, w.Tick(context.Background()))

	// Every peer's ticket resolved to the same match, recoverable by polling.
	matchIDs := map[string]struct{}{}
	for _, pid := range []int64{p1, p2} {
		var ticketID int64
		require.NoError(t, pool.QueryRow(context.Background(),
			`SELECT id FROM matchmaking_tickets WHERE tenant_id = $1 AND player_id = $2`,
			tenantID, pid).Scan(&ticketID))
		got, err := queue.Get(tctx, ticketID, pid)
		require.NoError(t, err)
		require.Equal(t, matchmaker.StatusMatched, got.Status)
		require.NotEmpty(t, got.MatchID)

		match, err := queue.ClaimMatch(tctx, got.MatchID)
		require.NoError(t, err, "poll recovery returns the match despite the missed push")
		assert.Len(t, match.Roster, 2)
		assert.False(t, match.ClaimedAt.IsZero())
		matchIDs[got.MatchID] = struct{}{}
	}
	assert.Len(t, matchIDs, 1, "both peers share one match")
}

// TestPGQueueExpiredTicketPollSurfacesFailureReason is the machine-readable
// failure-reason exit criterion: an expired ticket's poll response carries
// failure_reason = "expired" so clients can distinguish a timeout from a
// resolver that never succeeded.
func TestPGQueueExpiredTicketPollSurfacesFailureReason(t *testing.T) {
	pool := startMigratedDB(t)
	appPool := db.NewPool(pool)
	queue := matchmaker.NewPGQueue(appPool)

	tenantID, projectID, playerID := seedTenantProjectPlayer(t, pool, "mm-reason", "reason-p1")
	tctx := db.WithTenant(context.Background(), tenantID)

	past := time.Now().UTC().Add(-time.Minute)
	ticket, err := queue.Enqueue(tctx, matchmaker.EnqueueRequest{
		TenantID: tenantID, ProjectID: projectID, PlayerID: playerID,
		Mode: matchmaker.ModeMatchOnly, GameMode: "1v1", ExpiresAt: &past,
	})
	require.NoError(t, err)

	_, err = queue.SweepStaleClaims(context.Background(), 3)
	require.NoError(t, err)

	got, err := queue.Get(tctx, ticket.ID, playerID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status)
	assert.Equal(t, "expired", got.FailureReason)
}
