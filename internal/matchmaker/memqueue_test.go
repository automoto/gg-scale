package matchmaker_test

import (
	"context"
	"testing"

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
	mine, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2})
	require.NoError(t, err)

	_, err = q.Get(tenantCtx(2), mine.ID)

	assert.ErrorIs(t, err, matchmaker.ErrNotFound)
}

func TestMemQueueCancelTransitionsQueuedToCancelled(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2})
	require.NoError(t, err)

	require.NoError(t, q.Cancel(tenantCtx(1), t1.ID))

	got, err := q.Get(tenantCtx(1), t1.ID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusCancelled, got.Status)
}

func TestMemQueueCancelRejectsTerminal(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, err := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	require.NoError(t, err)
	_, err = q.PopBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 1)
	require.NoError(t, err)

	err = q.Cancel(tenantCtx(1), t1.ID)

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

func TestMemQueuePopBucketReturnsNilWhenShort(t *testing.T) {
	q := matchmaker.NewMemQueue()
	_, _ = q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})

	tickets, err := q.PopBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 2)
	require.NoError(t, err)

	assert.Nil(t, tickets)
}

func TestMemQueuePopBucketReturnsOldestFirst(t *testing.T) {
	q := matchmaker.NewMemQueue()
	first, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	second, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})

	tickets, err := q.PopBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 2)

	require.NoError(t, err)
	require.Len(t, tickets, 2)
	assert.Equal(t, first.ID, tickets[0].ID)
	assert.Equal(t, second.ID, tickets[1].ID)
}

func TestMemQueueMarkMatchedSetsAddress(t *testing.T) {
	q := matchmaker.NewMemQueue()
	t1, _ := q.Enqueue(context.Background(), matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"})
	_, _ = q.PopBucket(context.Background(), matchmaker.Bucket{TenantID: 1, ProjectID: 2, Region: "r", GameMode: "g"}, 1)

	require.NoError(t, q.MarkMatched(context.Background(), []int64{t1.ID}, "10.0.0.1:7777"))

	got, err := q.Get(tenantCtx(1), t1.ID)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1:7777", got.MatchAddress)
	assert.Equal(t, matchmaker.StatusMatched, got.Status)
}
