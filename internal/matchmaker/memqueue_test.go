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

func TestMemQueueEnqueueAssignsIdAndQueuedStatus(t *testing.T) {
	q := matchmaker.NewMemQueue()

	ticket, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, EndUserID: 3, Region: "r", GameMode: "g"})

	require.NoError(t, err)
	assert.NotZero(t, ticket.ID)
	assert.Equal(t, matchmaker.StatusQueued, ticket.Status)
}

func TestMemQueueGetIsTenantScoped(t *testing.T) {
	q := matchmaker.NewMemQueue()
	mine, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, EndUserID: 3})
	require.NoError(t, err)

	_, err = q.Get(tenantCtx(2), mine.ID, 3)

	assert.ErrorIs(t, err, matchmaker.ErrNotFound)
}

func TestMemQueueGetIsEndUserScoped(t *testing.T) {
	q := matchmaker.NewMemQueue()
	mine, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, EndUserID: 3})
	require.NoError(t, err)

	_, err = q.Get(tenantCtx(1), mine.ID, 4)

	assert.ErrorIs(t, err, matchmaker.ErrNotFound)
}

func TestMemQueueCancelTransitionsQueuedToCancelled(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, EndUserID: 3})
	require.NoError(t, err)

	require.NoError(t, q.Cancel(tenantCtx(1), t1.ID, 3))

	got, err := q.Get(tenantCtx(1), t1.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusCancelled, got.Status)
}

func TestMemQueueCancelIsEndUserScoped(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, EndUserID: 3})
	require.NoError(t, err)

	err = q.Cancel(tenantCtx(1), t1.ID, 4)

	assert.ErrorIs(t, err, matchmaker.ErrNotFound)
}

func TestMemQueueCancelRejectsTerminal(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	require.NoError(t, err)
	claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 1, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	_, err = q.CommitClaim(context.Background(), claim, "10.0.0.1:7777", "")
	require.NoError(t, err)

	err = q.Cancel(tenantCtx(1), t1.ID, 0)

	assert.ErrorIs(t, err, matchmaker.ErrAlreadyTerminal)
}

func TestMemQueueListReadyBucketsRespectsMinCount(t *testing.T) {
	q := matchmaker.NewMemQueue()
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "x", GameMode: "g"})

	buckets, err := q.ListReadyBuckets(context.Background(), 2)
	require.NoError(t, err)

	require.Len(t, buckets, 1)
	assert.Equal(t, "r", buckets[0].Region)
}

func TestMemQueueListReadyBucketsSkipsClaimed(t *testing.T) {
	q := matchmaker.NewMemQueue()
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	bucket := matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}
	_, err := q.ClaimBucket(context.Background(), bucket, 2, time.Minute)
	require.NoError(t, err)

	buckets, err := q.ListReadyBuckets(context.Background(), 1)
	require.NoError(t, err)

	assert.Empty(t, buckets, "claimed tickets should not surface as ready")
}

func TestMemQueueClaimBucketReturnsNilWhenShort(t *testing.T) {
	q := matchmaker.NewMemQueue()
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})

	claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 2, time.Minute)
	require.NoError(t, err)

	assert.Nil(t, claim)
}

func TestMemQueueClaimBucketReturnsOldestFirst(t *testing.T) {
	q := matchmaker.NewMemQueue()
	first, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	second, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})

	claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 2, time.Minute)

	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Len(t, claim.Tickets, 2)
	assert.Equal(t, first.ID, claim.Tickets[0].ID)
	assert.Equal(t, second.ID, claim.Tickets[1].ID)
}

func TestMemQueueClaimBucketKeepsStatusQueued(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	_, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 1, time.Minute)
	require.NoError(t, err)

	got, err := q.Get(tenantCtx(1), t1.ID, 0)

	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status, "claimed tickets stay 'queued' until CommitClaim")
}

func TestMemQueueCommitClaimSetsAddress(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	claim, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 1, time.Minute)
	require.NoError(t, err)

	n, err := q.CommitClaim(context.Background(), claim, "10.0.0.1:7777", "tcp")
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
	claim, _ := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 1, time.Minute)
	require.NoError(t, q.Cancel(tenantCtx(1), t1.ID, 0))

	n, err := q.CommitClaim(context.Background(), claim, "10.0.0.1:7777", "tcp")
	require.NoError(t, err)

	assert.Equal(t, int64(0), n, "commit must return 0 when the claim's tickets were cancelled mid-flight")
}

func TestMemQueueReleaseClaimBumpsAttemptsAndFailsAtCap(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	bucket := matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}

	// First release with maxAttempts=2 -> attempts becomes 1, still queued.
	claim, _ := q.ClaimBucket(context.Background(), bucket, 1, time.Minute)
	require.NoError(t, q.ReleaseClaim(context.Background(), claim, 2))
	got, _ := q.Get(tenantCtx(1), t1.ID, 0)
	assert.Equal(t, matchmaker.StatusQueued, got.Status)

	// Second release -> attempts becomes 2 == cap, flips to failed.
	claim, _ = q.ClaimBucket(context.Background(), bucket, 1, time.Minute)
	require.NoError(t, q.ReleaseClaim(context.Background(), claim, 2))
	got, _ = q.Get(tenantCtx(1), t1.ID, 0)
	assert.Equal(t, matchmaker.StatusFailed, got.Status)
}

func TestMemQueueSweepStaleClaimsReleasesExpired(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	// Zero TTL → claim is immediately expired.
	_, err := q.ClaimBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 1, 0)
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)

	n, err := q.SweepStaleClaims(context.Background(), 5)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	got, _ := q.Get(tenantCtx(1), t1.ID, 0)
	assert.Equal(t, matchmaker.StatusQueued, got.Status, "swept ticket re-enters the queue (under attempt cap)")
}
