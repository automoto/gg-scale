//go:build integration

package managementapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/cache/memory"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/ratelimit"
)

// bucketHandlers returns the token middleware for buckets "mcp" and "api"
// on one limiter, each with a burst of one request.
func (f *fixture) bucketHandlers() map[string]http.Handler {
	f.t.Helper()
	d := managementtoken.Deps{
		Pool:               f.pool,
		RBAC:               f.authz,
		Limiter:            ratelimit.NewCacheLimiter(memory.New()),
		TokenRatePerSecond: 0.001,
		TokenBurst:         1,
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return map[string]http.Handler{
		"mcp": managementtoken.Middleware(d, "mcp")(ok),
		"api": managementtoken.Middleware(d, "api")(ok),
	}
}

func serveToken(h http.Handler, token string) int {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestTokenBuckets_should_be_independent(t *testing.T) {
	f := newFixture(t)
	cases := map[string]string{"api limit keeps mcp": "api", "mcp limit keeps api": "mcp"}
	for name, full := range cases {
		t.Run(name, func(t *testing.T) {
			handlers := f.bucketHandlers()
			other := "mcp"
			if full == "mcp" {
				other = "api"
			}
			tok := f.ownerToken()
			serveToken(handlers[full], tok)

			codes := []int{serveToken(handlers[full], tok), serveToken(handlers[other], tok)}

			assert.Equal(t, []int{http.StatusTooManyRequests, http.StatusOK}, codes)
		})
	}
}

func TestTokenBuckets_should_share_the_ip_failure_bucket(t *testing.T) {
	// Arrange
	f := newFixture(t)
	d := managementtoken.Deps{
		Pool:                  f.pool,
		RBAC:                  f.authz,
		Limiter:               ratelimit.NewCacheLimiter(memory.New()),
		TokenRatePerSecond:    1000,
		TokenBurst:            1000,
		AuthFailuresPerMinute: 1,
		AuthFailureBurst:      1,
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	serveToken(managementtoken.Middleware(d, "mcp")(ok), "ggm_wrong")

	// Act
	code := serveToken(managementtoken.Middleware(d, "api")(ok), "ggm_wrong")

	// Assert
	assert.Equal(t, http.StatusTooManyRequests, code)
}
