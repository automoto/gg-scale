//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/ratelimit"
)

type fixedAPIOverride struct {
	limits ratelimit.Limits
}

func (o fixedAPIOverride) APILimit(context.Context, int64) (ratelimit.Limits, bool, error) {
	return o.limits, true, nil
}

func (fixedAPIOverride) InviteLimit(context.Context, int64, int64, string) (ratelimit.Limits, bool, error) {
	return ratelimit.Limits{}, false, nil
}

func TestRatelimit_bursting_tenant_throttled_other_tenant_unaffected(t *testing.T) {
	c := startCluster(t)
	_, _ = seedTenantWithAPIKey(t, c.bootstrapPool, 0, "ka")
	_, _ = seedTenantWithAPIKey(t, c.bootstrapPool, 0, "kb")
	srv := newServerWithRateLimitOverrides(t, c, fixedAPIOverride{
		limits: ratelimit.Limits{RatePerSecond: 0.001, Burst: 2},
	})

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

	resp, err := profileA()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp, err = profileA()
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	require.NoError(t, resp.Body.Close())

	resp, err = profileB()
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "tenant B should have an independent bucket")
	require.NoError(t, resp.Body.Close())
}
