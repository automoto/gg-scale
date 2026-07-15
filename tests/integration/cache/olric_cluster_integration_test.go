//go:build integration

package cache_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cacheolric "github.com/ggscale/ggscale/internal/cache/olric"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

func TestOlricCluster_two_node_burst_lock_is_atomic_and_survives_one_node_stop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	memberlistA := freePort(t)
	storeA, err := cacheolric.New(ctx, cacheolric.Config{
		BindPort:           freePort(t),
		MemberlistBindPort: memberlistA,
		ReplicaCount:       2,
		LogLevel:           "ERROR",
		StartTimeout:       20 * time.Second,
	})
	require.NoError(t, err)
	storeB, err := cacheolric.New(ctx, cacheolric.Config{
		BindPort:           freePort(t),
		MemberlistBindPort: freePort(t),
		Peers:              []string{fmt.Sprintf("127.0.0.1:%d", memberlistA)},
		ReplicaCount:       2,
		LogLevel:           "ERROR",
		StartTimeout:       20 * time.Second,
	})
	require.NoError(t, err)
	closedA := false
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if !closedA {
			_ = storeA.Close(closeCtx)
		}
		require.NoError(t, storeB.Close(closeCtx))
	})

	require.NoError(t, storeA.Set(ctx, "branch:cluster:ready", []byte("ready"), time.Minute))
	require.Eventually(t, func() bool {
		got, getErr := storeB.Get(ctx, "branch:cluster:ready")
		return getErr == nil && string(got) == "ready"
	}, 10*time.Second, 50*time.Millisecond)

	const callers = 128
	stores := []*cacheolric.Store{storeA, storeB}
	start := make(chan struct{})
	errs := make(chan error, callers)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, _, acquireErr := stores[i%len(stores)].AcquireSlotBurst(
				ctx, "branch:cluster:burst", 2, 4, time.Minute, time.Hour)
			if acquireErr != nil {
				errs <- acquireErr
				return
			}
			if ok {
				allowed.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for acquireErr := range errs {
		require.NoError(t, acquireErr)
	}
	assert.Equal(t, int64(4), allowed.Load())

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	require.NoError(t, storeA.Close(closeCtx))
	closeCancel()
	closedA = true
	time.Sleep(500 * time.Millisecond)
	ok, current, err := storeB.AcquireSlotBurst(ctx, "branch:cluster:burst", 2, 4, time.Minute, time.Hour)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, int64(4), current)

	for range 4 {
		require.NoError(t, storeB.ReleaseSlotBurst(ctx, "branch:cluster:burst"))
	}
	ok, current, err = storeB.AcquireSlotBurst(ctx, "branch:cluster:burst", 2, 4, time.Minute, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int64(1), current)
}
