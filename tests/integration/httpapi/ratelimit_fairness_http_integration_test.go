//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-API-key HTTP rate limit: one tenant can be throttled while another at
// moderate request rate is not; throttled responses carry Retry-After.

func TestRatelimit_bursting_tenant_throttled_other_tenant_unaffected(t *testing.T) {
	c := startCluster(t)
	_, _ = seedTenantWithAPIKey(t, c.bootstrapPool, "free", "ka")
	_, _ = seedTenantWithAPIKey(t, c.bootstrapPool, "free", "kb")
	srv := newServerForCluster(t, c)

	jwtA := anonymousLogin(t, srv.URL, "ka")
	jwtB := anonymousLogin(t, srv.URL, "kb")

	client := &http.Client{Timeout: 3 * time.Second}
	profileA := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/profile/", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer ka")
		req.Header.Set("X-Session-Token", jwtA)
		return client.Do(req)
	}
	profileB := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/profile/", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer kb")
		req.Header.Set("X-Session-Token", jwtB)
		return client.Do(req)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var first429Ns atomic.Int64
	var count429A atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				resp, err := profileA()
				if err != nil {
					continue
				}
				code := resp.StatusCode
				_ = resp.Body.Close()
				if code == http.StatusTooManyRequests {
					count429A.Add(1)
					first429Ns.CompareAndSwap(0, time.Now().UnixNano())
				}
			}
		}()
	}

	var count429B atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				resp, err := profileB()
				if err != nil {
					continue
				}
				code := resp.StatusCode
				_ = resp.Body.Close()
				if code == http.StatusTooManyRequests {
					count429B.Add(1)
				}
			}
		}
	}()

	<-ctx.Done()
	wg.Wait()

	assert.Greater(t, count429A.Load(), int64(0), "tenant A should be throttled")
	first429 := first429Ns.Load()
	require.NotZero(t, first429)
	assert.Less(t, time.Duration(first429-start.UnixNano()), 2*time.Second,
		"first 429 should arrive within the burst window")

	assert.Equal(t, int64(0), count429B.Load(), "tenant B should not be throttled at 50 Hz")

	resp, err := profileA()
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		ra := resp.Header.Get("Retry-After")
		assert.NotEmpty(t, ra)
	}
}
