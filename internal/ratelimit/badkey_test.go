package ratelimit_test

import (
	"context"
	"crypto/sha256"
	"fmt"
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

const (
	attackerAddr = "203.0.113.7:50000"
	playerAddr   = "198.51.100.9:50000"
	validToken   = "valid-key"
)

// keyLookup knows only validToken and counts every call (a database query in
// production).
func keyLookup(calls *int) tenant.Lookup {
	valid := tenant.APIKey{ID: 1, TenantID: 1}
	validHash := hashToken(validToken)
	return func(_ context.Context, h []byte) (*tenant.APIKey, error) {
		*calls++
		if string(h) == string(validHash) {
			k := valid
			return &k, nil
		}
		return nil, tenant.ErrUnknownKey
	}
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func newBadKeyMiddleware(t *testing.T, calls *int) http.Handler {
	t.Helper()
	store := memory.New()
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	limit := ratelimit.NewBadKeyLimiter(ratelimit.NewCacheLimiter(store), nil, prometheus.NewRegistry())
	require.NotNil(t, limit)
	lookup := tenant.NewCachedLookup(keyLookup(calls), tenant.APIKeyCacheTTL, tenant.APIKeyCacheMaxEntries)
	return tenant.NewLimited(lookup, limit)(nopHandler())
}

func send(h http.Handler, addr, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	r.RemoteAddr = addr
	r.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// exhaust spends addr's budget with distinct unknown keys, so each one misses
// the cache and would reach the database.
func exhaust(h http.Handler, addr string) {
	for i := range int(ratelimit.BadKeyBurst) {
		send(h, addr, fmt.Sprintf("wrong-key-%d", i))
	}
}

func TestBadKeyLimiter_should_return_429_without_lookup_when_ip_is_over_budget(t *testing.T) {
	calls := 0
	h := newBadKeyMiddleware(t, &calls)
	exhaust(h, attackerAddr)
	before := calls

	rr := send(h, attackerAddr, "another-wrong-key")

	require.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Equal(t, before, calls, "the lookup must not run")
}

func TestBadKeyLimiter_should_send_retry_after_and_json_body_when_limited(t *testing.T) {
	calls := 0
	h := newBadKeyMiddleware(t, &calls)
	exhaust(h, attackerAddr)

	rr := send(h, attackerAddr, "another-wrong-key")

	require.NotEmpty(t, rr.Header().Get("Retry-After"))
	assert.Contains(t, rr.Body.String(), `"error":"too_many_unknown_keys"`)
}

func TestBadKeyLimiter_should_allow_valid_key_from_other_ip_when_one_ip_is_over_budget(t *testing.T) {
	calls := 0
	h := newBadKeyMiddleware(t, &calls)
	exhaust(h, attackerAddr)

	rr := send(h, playerAddr, validToken)

	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestBadKeyLimiter_should_not_spend_budget_on_valid_keys(t *testing.T) {
	calls := 0
	h := newBadKeyMiddleware(t, &calls)

	codes := map[int]int{}
	for range 3 * int(ratelimit.BadKeyBurst) {
		codes[send(h, playerAddr, validToken).Code]++
	}

	assert.Equal(t, map[int]int{http.StatusOK: 3 * int(ratelimit.BadKeyBurst)}, codes)
}

func TestNewBadKeyLimiter_should_return_nil_when_limiter_cannot_refund(t *testing.T) {
	limit := ratelimit.NewBadKeyLimiter(&fakeLimiter{}, nil, prometheus.NewRegistry())

	assert.Nil(t, limit)
}

func TestBadKeyLimiter_should_admit_cached_valid_key_from_ip_over_budget(t *testing.T) {
	calls := 0
	h := newBadKeyMiddleware(t, &calls)
	require.Equal(t, http.StatusOK, send(h, attackerAddr, validToken).Code)
	exhaust(h, attackerAddr)

	rr := send(h, attackerAddr, validToken)

	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestBadKeyLimiter_should_refuse_uncached_valid_key_from_ip_over_budget(t *testing.T) {
	calls := 0
	h := newBadKeyMiddleware(t, &calls)
	exhaust(h, attackerAddr)

	rr := send(h, attackerAddr, validToken)

	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
}
