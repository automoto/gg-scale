//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/cache/memory"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

func TestRatelimitShare_tier_zero_key_gets_half_burst_then_429(t *testing.T) {
	// Arrange
	store := memory.New()
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	handler := ratelimit.New(ratelimit.NewCacheLimiter(store), nil, prometheus.NewRegistry(), 0.5)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	req := httptest.NewRequest(http.MethodGet, "/v1/profile", nil)
	req = req.WithContext(tenant.WithAPIKey(req.Context(), tenant.APIKey{
		ID: 1, TenantID: 1, Tier: tenant.Tier0,
	}))

	// Act
	for i := range 1250 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equalf(t, http.StatusOK, rec.Code, "request %d", i+1)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Assert
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	var body struct {
		Error             string  `json:"error"`
		RetryAfterSeconds float64 `json:"retry_after_seconds"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, "rate_limit_exceeded", body.Error)
	assert.Positive(t, body.RetryAfterSeconds)
}
