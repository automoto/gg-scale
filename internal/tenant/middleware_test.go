package tenant_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/tenant"
)

func nopHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func captureCtx(out *context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*out = r.Context()
		w.WriteHeader(http.StatusOK)
	})
}

func ptr[T any](v T) *T { return &v }

func hashOf(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func TestMiddleware_returns_401_when_authorization_header_missing(t *testing.T) {
	mw := tenant.New(func(_ context.Context, _ []byte) (*tenant.APIKey, error) {
		t.Fatal("lookup must not run when header is missing")
		return nil, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestMiddleware_returns_401_when_scheme_is_not_Bearer(t *testing.T) {
	mw := tenant.New(func(_ context.Context, _ []byte) (*tenant.APIKey, error) {
		t.Fatal("lookup must not run for non-Bearer scheme")
		return nil, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Basic abc")
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestMiddleware_returns_401_when_token_is_empty(t *testing.T) {
	mw := tenant.New(func(_ context.Context, _ []byte) (*tenant.APIKey, error) {
		t.Fatal("lookup must not run when token is empty")
		return nil, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer  ")
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestMiddleware_returns_401_when_lookup_finds_no_match(t *testing.T) {
	mw := tenant.New(func(_ context.Context, _ []byte) (*tenant.APIKey, error) {
		return nil, tenant.ErrUnknownKey
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer ghostkey")
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestMiddleware_returns_403_when_key_is_revoked(t *testing.T) {
	mw := tenant.New(func(_ context.Context, _ []byte) (*tenant.APIKey, error) {
		return &tenant.APIKey{TenantID: 7, Revoked: true}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer revoked")
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestMiddleware_returns_500_when_lookup_errors(t *testing.T) {
	boom := errors.New("db down")
	mw := tenant.New(func(_ context.Context, _ []byte) (*tenant.APIKey, error) {
		return nil, boom
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestMiddleware_passes_tenant_and_project_into_context_on_success(t *testing.T) {
	token := "valid-token"
	mw := tenant.New(func(_ context.Context, h []byte) (*tenant.APIKey, error) {
		require.Equal(t, hashOf(token), h)
		return &tenant.APIKey{TenantID: 42, ProjectID: ptr(int64(99))}, nil
	})

	var captured context.Context
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mw(captureCtx(&captured)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	tid, err := db.TenantFromContext(captured)
	require.NoError(t, err)
	assert.Equal(t, int64(42), tid)
	pid, ok := db.ProjectFromContext(captured)
	assert.True(t, ok)
	assert.Equal(t, int64(99), pid)
}

func TestMiddleware_skips_project_when_key_has_no_project_scope(t *testing.T) {
	mw := tenant.New(func(_ context.Context, _ []byte) (*tenant.APIKey, error) {
		return &tenant.APIKey{TenantID: 5}, nil
	})

	var captured context.Context
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer t")
	rr := httptest.NewRecorder()
	mw(captureCtx(&captured)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	_, ok := db.ProjectFromContext(captured)
	assert.False(t, ok)
}

// Adversarial cases: anything that looks like an attempt to spoof tenant_id
// from outside the resolver path must fail closed.

func TestMiddleware_ignores_X_Tenant_Id_header_spoof(t *testing.T) {
	mw := tenant.New(func(_ context.Context, _ []byte) (*tenant.APIKey, error) {
		return &tenant.APIKey{TenantID: 1}, nil
	})

	var captured context.Context
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("X-Tenant-Id", "999")
	rr := httptest.NewRecorder()
	mw(captureCtx(&captured)).ServeHTTP(rr, req)

	tid, err := db.TenantFromContext(captured)
	require.NoError(t, err)
	assert.Equal(t, int64(1), tid, "must use resolver value, not the spoofed header")
}

func TestMiddleware_hashes_token_with_sha256_before_lookup(t *testing.T) {
	var seen [][]byte
	mw := tenant.New(func(_ context.Context, h []byte) (*tenant.APIKey, error) {
		seen = append(seen, h)
		return nil, tenant.ErrUnknownKey
	})

	for _, token := range []string{"aaaa", "aaab"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		mw(nopHandler()).ServeHTTP(rr, req)
	}

	require.Len(t, seen, 2)
	assert.Equal(t, hashOf("aaaa"), seen[0])
	assert.Equal(t, hashOf("aaab"), seen[1])
}
