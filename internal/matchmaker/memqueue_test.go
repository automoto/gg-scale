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

func tenantCtx(tenantID int64) context.Context {
	return db.WithTenant(context.Background(), tenantID)
}

// fakeFailureRecorder records the per-reason failure counts the queue reports.
type fakeFailureRecorder struct{ byReason map[string]int }

func newFakeFailureRecorder() *fakeFailureRecorder {
	return &fakeFailureRecorder{byReason: map[string]int{}}
}

func (r *fakeFailureRecorder) MatchmakerTicketFailure(reason string, n int) {
	r.byReason[reason] += n
}

func TestMemQueueEnqueueExpiredStaleRecordsFailure(t *testing.T) {
	rec := newFakeFailureRecorder()
	q := matchmaker.NewMemQueue().WithFailureRecorder(rec)
	past := time.Now().UTC().Add(-time.Minute)
	_, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g", ExpiresAt: &past})
	require.NoError(t, err)

	_, err = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	require.NoError(t, err)

	assert.Equal(t, 1, rec.byReason["expired"])
	assert.Zero(t, rec.byReason["attempts_exhausted"])
}

func TestMemQueueReleaseAtCapRecordsAttemptsExhausted(t *testing.T) {
	rec := newFakeFailureRecorder()
	q := matchmaker.NewMemQueue().WithFailureRecorder(rec)
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	require.NoError(t, err)
	bucket := matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, GameMode: "g"}

	claim, err := q.ClaimBucket(context.Background(), bucket, 1, time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.ReleaseTickets(context.Background(), claim, []int64{t1.ID}, 1))

	assert.Equal(t, 1, rec.byReason["attempts_exhausted"])
	assert.Zero(t, rec.byReason["expired"])
}

func TestMemQueueSweepRecordsFailuresByReason(t *testing.T) {
	rec := newFakeFailureRecorder()
	q := matchmaker.NewMemQueue().WithFailureRecorder(rec)
	past := time.Now().UTC().Add(-time.Minute)
	// One expired (TTL) ticket and one claimed-then-lease-expired ticket at cap.
	_, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g", ExpiresAt: &past})
	require.NoError(t, err)
	t2, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 4, Region: "r", GameMode: "g"})
	require.NoError(t, err)
	_, err = q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, GameMode: "g"}, 1, 0)
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)

	_, err = q.SweepStaleClaims(context.Background(), 1)
	require.NoError(t, err)

	assert.Equal(t, 1, rec.byReason["expired"])
	assert.Equal(t, 1, rec.byReason["attempts_exhausted"])
	_ = t2
}

func TestMemQueueStatsGroupsByBucket(t *testing.T) {
	q := matchmaker.NewMemQueue()
	_, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Mode: matchmaker.ModeMatchOnly, Region: "r", GameMode: "g"})
	require.NoError(t, err)
	_, err = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 4, Mode: matchmaker.ModeMatchOnly, Region: "x", GameMode: "g"})
	require.NoError(t, err)

	stats, err := q.QueueStats(context.Background())
	require.NoError(t, err)

	// match_only merges regions into one bucket (region blanked).
	require.Len(t, stats, 1)
	assert.Equal(t, "match_only", stats[0].Mode)
	assert.Equal(t, "", stats[0].Region)
	assert.Equal(t, int64(2), stats[0].Depth)
	assert.GreaterOrEqual(t, stats[0].OldestAgeSeconds, 0.0)
}

func TestMemQueueEnqueueAssignsIdAndQueuedStatus(t *testing.T) {
	q := matchmaker.NewMemQueue()

	ticket, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})

	require.NoError(t, err)
	assert.NotZero(t, ticket.ID)
	assert.Equal(t, matchmaker.StatusQueued, ticket.Status)
}

func TestMemQueueEnqueueRejectsSecondActiveTicket(t *testing.T) {
	q := matchmaker.NewMemQueue()
	first, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	require.NoError(t, err)

	_, err = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})

	require.ErrorIs(t, err, matchmaker.ErrTicketActive)
	var active *matchmaker.TicketActiveError
	require.ErrorAs(t, err, &active)
	assert.Equal(t, first.ID, active.ActiveTicketID)
}

func TestMemQueueEnqueueReplacesExpiredTicket(t *testing.T) {
	q := matchmaker.NewMemQueue()
	past := time.Now().UTC().Add(-time.Minute)
	first, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g", ExpiresAt: &past})
	require.NoError(t, err)

	// An expired-but-unswept ticket must not block a re-queue.
	second, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)

	// The stale ticket is TTL-expired in place, exactly as the sweeper would.
	stale, err := q.Get(tenantCtx(1), first.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, stale.Status)
	assert.Equal(t, "expired", stale.FailureReason)
}

func TestMemQueueEnqueueAllowsReQueueAfterTerminal(t *testing.T) {
	terminal := []struct {
		name   string
		settle func(t *testing.T, q *matchmaker.MemQueue, ticketID int64)
	}{
		{"cancelled", func(t *testing.T, q *matchmaker.MemQueue, id int64) {
			require.NoError(t, q.Cancel(tenantCtx(1), id, 3))
		}},
		{"matched", func(t *testing.T, q *matchmaker.MemQueue, id int64) {
			claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, GameMode: "g"}, 1, time.Minute)
			require.NoError(t, err)
			_, err = q.CommitTickets(context.Background(), claim, []int64{id}, "m", "", "")
			require.NoError(t, err)
		}},
	}
	for _, tc := range terminal {
		t.Run(tc.name, func(t *testing.T) {
			q := matchmaker.NewMemQueue()
			first, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
			require.NoError(t, err)
			tc.settle(t, q, first.ID)

			_, err = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
			assert.NoError(t, err, "a new ticket is allowed once the previous one is terminal")
		})
	}
}

func TestMemQueueGetIsTenantScoped(t *testing.T) {
	q := matchmaker.NewMemQueue()
	mine, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3})
	require.NoError(t, err)

	_, err = q.Get(tenantCtx(2), mine.ID, 3)

	assert.ErrorIs(t, err, matchmaker.ErrNotFound)
}

func TestMemQueueGetIsPlayerScoped(t *testing.T) {
	q := matchmaker.NewMemQueue()
	mine, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3})
	require.NoError(t, err)

	_, err = q.Get(tenantCtx(1), mine.ID, 4)

	assert.ErrorIs(t, err, matchmaker.ErrNotFound)
}

func TestMemQueueCancelTransitionsQueuedToCancelled(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3})
	require.NoError(t, err)

	require.NoError(t, q.Cancel(tenantCtx(1), t1.ID, 3))

	got, err := q.Get(tenantCtx(1), t1.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusCancelled, got.Status)
}

func TestMemQueueCancelIsPlayerScoped(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3})
	require.NoError(t, err)

	err = q.Cancel(tenantCtx(1), t1.ID, 4)

	assert.ErrorIs(t, err, matchmaker.ErrNotFound)
}

func TestMemQueueCancelRejectsTerminal(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	require.NoError(t, err)
	claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}, 1, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	_, err = q.CommitTickets(context.Background(), claim, []int64{t1.ID}, "", "10.0.0.1:7777", "")
	require.NoError(t, err)

	err = q.Cancel(tenantCtx(1), t1.ID, 0)

	assert.ErrorIs(t, err, matchmaker.ErrAlreadyTerminal)
}

func TestMemQueueListReadyBucketsMergesRegionsForMatchOnly(t *testing.T) {
	q := matchmaker.NewMemQueue()
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 4, Region: "x", GameMode: "g"})

	buckets, err := q.ListReadyBuckets(context.Background())
	require.NoError(t, err)

	// Region is a bucket dimension only for fleet_allocation; match_only
	// tickets share one bucket and the worker applies the soft-region rules.
	require.Len(t, buckets, 1)
	assert.Equal(t, "", buckets[0].Region)
}

func TestMemQueueListReadyBucketsSkipsClaimed(t *testing.T) {
	q := matchmaker.NewMemQueue()
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 4, Region: "r", GameMode: "g"})
	bucket := matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}
	_, err := q.ClaimBucket(context.Background(), bucket, 2, time.Minute)
	require.NoError(t, err)

	buckets, err := q.ListReadyBuckets(context.Background())
	require.NoError(t, err)

	assert.Empty(t, buckets, "claimed tickets should not surface as ready")
}

func TestMemQueueClaimBucketClaimsUpToMax(t *testing.T) {
	q := matchmaker.NewMemQueue()
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})

	claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}, 2, time.Minute)
	require.NoError(t, err)

	// max is a cap, not a requirement: the worker forms groups from
	// whatever was claimable.
	require.NotNil(t, claim)
	assert.Len(t, claim.Tickets, 1)
}

func TestMemQueueClaimBucketReturnsOldestFirst(t *testing.T) {
	q := matchmaker.NewMemQueue()
	first, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	second, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 4, Region: "r", GameMode: "g"})

	claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}, 2, time.Minute)

	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Len(t, claim.Tickets, 2)
	assert.Equal(t, first.ID, claim.Tickets[0].ID)
	assert.Equal(t, second.ID, claim.Tickets[1].ID)
}

func TestMemQueueClaimBucketKeepsStatusQueued(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	_, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}, 1, time.Minute)
	require.NoError(t, err)

	got, err := q.Get(tenantCtx(1), t1.ID, 0)

	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status, "claimed tickets stay 'queued' until CommitTickets")
}

func TestMemQueueCommitClaimSetsAddress(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}, 1, time.Minute)
	require.NoError(t, err)

	n, err := q.CommitTickets(context.Background(), claim, []int64{t1.ID}, "", "10.0.0.1:7777", "tcp")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	got, err := q.Get(tenantCtx(1), t1.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:7777", got.MatchAddress)
	assert.Equal(t, "tcp", got.MatchProtocol)
	assert.Equal(t, matchmaker.StatusMatched, got.Status)
}

func TestMemQueueCommitClaimZeroAfterCancel(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	claim, _ := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}, 1, time.Minute)
	require.NoError(t, q.Cancel(tenantCtx(1), t1.ID, 0))

	n, err := q.CommitTickets(context.Background(), claim, []int64{t1.ID}, "", "10.0.0.1:7777", "tcp")
	require.NoError(t, err)

	assert.Equal(t, int64(0), n, "commit must return 0 when the claim's tickets were cancelled mid-flight")
}

func TestMemQueueCommitTicketsIsAllOrNoneOnShortCommit(t *testing.T) {
	q := matchmaker.NewMemQueue()
	keep, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	drop, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 4, Region: "r", GameMode: "g"})
	bucket := matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, GameMode: "g"}
	claim, err := q.ClaimBucket(context.Background(), bucket, 2, time.Minute)
	require.NoError(t, err)
	require.Len(t, claim.Tickets, 2)

	// One member cancels between claim and commit.
	require.NoError(t, q.Cancel(tenantCtx(1), drop.ID, 4))

	n, err := q.CommitTickets(context.Background(), claim, []int64{keep.ID, drop.ID}, "m", "", "")

	require.ErrorIs(t, err, matchmaker.ErrShortCommit)
	assert.Equal(t, int64(1), n, "returns the would-be commit count")
	// All or none: the surviving ticket must NOT have been flipped to matched.
	got, err := q.Get(tenantCtx(1), keep.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status, "no ticket flips when the commit is short")
}

func TestMemQueueReturnTicketsUnclaimsSurvivorsPenaltyFree(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	bucket := matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, GameMode: "g"}
	claim, err := q.ClaimBucket(context.Background(), bucket, 1, time.Minute)
	require.NoError(t, err)

	require.NoError(t, q.ReturnTickets(context.Background(), claim, []int64{t1.ID}))

	// Un-claimed with no attempt penalty → immediately claimable again.
	got, err := q.Get(tenantCtx(1), t1.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status)
	reclaim, err := q.ClaimBucket(context.Background(), bucket, 1, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reclaim, "returned ticket is claimable on the next pass")
	assert.Len(t, reclaim.Tickets, 1)
}

func TestMemQueueReleaseClaimBumpsAttemptsAndFailsAtCap(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	bucket := matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}

	// First release with maxAttempts=2 -> attempts becomes 1, still queued.
	claim, _ := q.ClaimBucket(context.Background(), bucket, 1, time.Minute)
	require.NoError(t, q.ReleaseTickets(context.Background(), claim, []int64{t1.ID}, 2))
	got, _ := q.Get(tenantCtx(1), t1.ID, 0)
	assert.Equal(t, matchmaker.StatusQueued, got.Status)

	// Second release -> attempts becomes 2 == cap, flips to failed.
	claim, _ = q.ClaimBucket(context.Background(), bucket, 1, time.Minute)
	require.NoError(t, q.ReleaseTickets(context.Background(), claim, []int64{t1.ID}, 2))
	got, _ = q.Get(tenantCtx(1), t1.ID, 0)
	assert.Equal(t, matchmaker.StatusFailed, got.Status)
}

func TestMemQueueSweepExpiredSetsFailureReasonExpired(t *testing.T) {
	q := matchmaker.NewMemQueue()
	past := time.Now().UTC().Add(-time.Minute)
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g", ExpiresAt: &past})
	require.NoError(t, err)

	_, err = q.SweepStaleClaims(context.Background(), 3)
	require.NoError(t, err)

	got, err := q.Get(tenantCtx(1), t1.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status)
	assert.Equal(t, "expired", got.FailureReason)
}

func TestMemQueueReleaseAtCapSetsFailureReasonAttemptsExhausted(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g"})
	require.NoError(t, err)
	bucket := matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, GameMode: "g"}

	claim, err := q.ClaimBucket(context.Background(), bucket, 1, time.Minute)
	require.NoError(t, err)
	require.NoError(t, q.ReleaseTickets(context.Background(), claim, []int64{t1.ID}, 1))

	got, err := q.Get(tenantCtx(1), t1.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status)
	assert.Equal(t, "attempts_exhausted", got.FailureReason)
}

func TestMemQueueSweepStaleClaimsReleasesExpired(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	// Zero TTL → claim is immediately expired.
	_, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, Region: "", GameMode: "g"}, 1, 0)
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)

	n, err := q.SweepStaleClaims(context.Background(), 5)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	got, _ := q.Get(tenantCtx(1), t1.ID, 0)
	assert.Equal(t, matchmaker.StatusQueued, got.Status, "swept ticket re-enters the queue (under attempt cap)")
}

func TestMemQueueSweepFailsReleasedTicketPastTTL(t *testing.T) {
	rec := newFakeFailureRecorder()
	q := matchmaker.NewMemQueue().WithFailureRecorder(rec)
	// A ticket claimed just before its TTL, whose lease AND TTL then both
	// expire. Releasing it (under the attempt cap) un-claims it, and the same
	// sweep must go on to fail it as 'expired' — mirroring PGQueue, which runs
	// ExpireMatchmakerTickets in the same pass — rather than leave it queued.
	soon := time.Now().UTC().Add(20 * time.Millisecond)
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{
		TenantID: 1, ProjectID: 2, PlayerID: 3, Region: "r", GameMode: "g", ExpiresAt: &soon,
	})
	require.NoError(t, err)
	// Lease TTL 0 → the claim is stale the moment the sweep runs.
	_, err = q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Mode: matchmaker.ModeMatchOnly, GameMode: "g"}, 1, 0)
	require.NoError(t, err)
	time.Sleep(40 * time.Millisecond)

	_, err = q.SweepStaleClaims(context.Background(), 5)
	require.NoError(t, err)

	got, err := q.Get(tenantCtx(1), t1.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status)
	assert.Equal(t, "expired", got.FailureReason)
	assert.Equal(t, 1, rec.byReason["expired"])
}
