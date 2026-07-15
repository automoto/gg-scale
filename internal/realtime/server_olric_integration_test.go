//go:build integration

package realtime_test

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cacheolric "github.com/ggscale/ggscale/internal/cache/olric"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/realtime"
)

func freeRealtimeOlricPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

type websocketDialResult struct {
	conn *websocket.Conn
	resp *http.Response
	err  error
}

func TestBranchFollowup_two_servers_share_olric_tenant_hard_cap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := cacheolric.New(ctx, cacheolric.Config{
		BindPort:           freeRealtimeOlricPort(t),
		MemberlistBindPort: freeRealtimeOlricPort(t),
		LogLevel:           "ERROR",
		StartTimeout:       20 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		require.NoError(t, store.Close(closeCtx))
	})

	hub := realtime.NewHub()
	cap := ratelimit.NewCacheConnectionCap(store, nil)
	opts := realtime.Options{
		Hub:               hub,
		TenantCap:         cap,
		EnvMaxPerTenant:   4,
		HeartbeatInterval: time.Hour,
		SlotTTL:           time.Hour,
	}
	urlA, stopA := newTestServer(t, hub, opts, 101, 1001)
	defer stopA()
	urlB, stopB := newTestServer(t, hub, opts, 101, 1002)
	defer stopB()

	const attempts = 100
	results := make([]websocketDialResult, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			target := urlA
			if i%2 == 1 {
				target = urlB
			}
			conn, resp, err := websocket.Dial(ctx, target, nil)
			results[i] = websocketDialResult{conn: conn, resp: resp, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var admitted []*websocket.Conn
	var rejected int
	for _, result := range results {
		if result.conn != nil {
			admitted = append(admitted, result.conn)
			continue
		}
		require.Error(t, result.err)
		require.NotNil(t, result.resp)
		assert.Equal(t, http.StatusServiceUnavailable, result.resp.StatusCode)
		assert.Equal(t, "5", result.resp.Header.Get("Retry-After"))
		require.NoError(t, result.resp.Body.Close())
		rejected++
	}
	require.Len(t, admitted, 4)
	assert.Equal(t, attempts-4, rejected)

	closing := admitted[0]
	closing.CloseNow()
	admitted = admitted[1:]
	var replacement *websocket.Conn
	require.Eventually(t, func() bool {
		conn, resp, dialErr := websocket.Dial(ctx, urlA, nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if dialErr != nil {
			return false
		}
		replacement = conn
		return true
	}, 20*time.Second, 20*time.Millisecond)
	admitted = append(admitted, replacement)

	urlTenantB, stopTenantB := newTestServer(t, hub, opts, 202, 2001)
	defer stopTenantB()
	var tenantB []*websocket.Conn
	for range 4 {
		conn, resp, dialErr := websocket.Dial(ctx, urlTenantB, nil)
		require.NoError(t, dialErr)
		assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
		tenantB = append(tenantB, conn)
	}

	for _, conn := range append(admitted, tenantB...) {
		conn.CloseNow()
	}
	require.Eventually(t, func() bool { return hub.Count() == 0 }, 8*time.Second, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
}
