//go:build integration

package ratelimit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/ratelimit"
)

func TestValkeyConnectionCap_acquire_within_limit_succeeds(t *testing.T) {
	client := startValkey(t)
	cap := ratelimit.NewValkeyConnectionCap(client)
	key := ratelimit.ConnectionCapKey(1001)

	dec, err := cap.Acquire(context.Background(), key, 5)

	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.Equal(t, int64(1), dec.Current)
}

func TestValkeyConnectionCap_acquire_at_limit_rejects_and_rolls_back(t *testing.T) {
	client := startValkey(t)
	cap := ratelimit.NewValkeyConnectionCap(client)
	key := ratelimit.ConnectionCapKey(1002)

	for i := 0; i < 5; i++ {
		dec, err := cap.Acquire(context.Background(), key, 5)
		require.NoError(t, err)
		require.True(t, dec.Allowed)
	}

	dec, err := cap.Acquire(context.Background(), key, 5)
	require.NoError(t, err)
	assert.False(t, dec.Allowed)
	assert.Equal(t, int64(5), dec.Current, "counter must roll back to limit, not 6")
}

func TestValkeyConnectionCap_release_frees_a_slot(t *testing.T) {
	client := startValkey(t)
	cap := ratelimit.NewValkeyConnectionCap(client)
	key := ratelimit.ConnectionCapKey(1003)

	for i := 0; i < 3; i++ {
		_, err := cap.Acquire(context.Background(), key, 3)
		require.NoError(t, err)
	}
	dec, err := cap.Acquire(context.Background(), key, 3)
	require.NoError(t, err)
	require.False(t, dec.Allowed, "third connection over limit is rejected")

	require.NoError(t, cap.Release(context.Background(), key))

	dec, err = cap.Acquire(context.Background(), key, 3)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "after release a new connection fits")
	assert.Equal(t, int64(3), dec.Current)
}

func TestValkeyConnectionCap_isolates_counts_by_key(t *testing.T) {
	client := startValkey(t)
	cap := ratelimit.NewValkeyConnectionCap(client)
	keyA := ratelimit.ConnectionCapKey(2001)
	keyB := ratelimit.ConnectionCapKey(2002)

	for i := 0; i < 5; i++ {
		_, err := cap.Acquire(context.Background(), keyA, 5)
		require.NoError(t, err)
	}
	dec, err := cap.Acquire(context.Background(), keyA, 5)
	require.NoError(t, err)
	require.False(t, dec.Allowed)

	dec, err = cap.Acquire(context.Background(), keyB, 5)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "tenant B's counter is independent of A's")
	assert.Equal(t, int64(1), dec.Current)
}
