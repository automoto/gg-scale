package enduser_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/enduser"
)

func newSigner(t *testing.T) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	require.NoError(t, err)
	return s
}

func reqWithTenant(tenantID int64) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	return r.WithContext(db.WithTenant(r.Context(), tenantID))
}

func captureEndUser(out *int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := enduser.IDFromContext(r.Context()); ok {
			*out = id
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddleware_returns_401_when_X_Session_Token_missing(t *testing.T) {
	mw := enduser.New(newSigner(t))

	rr := httptest.NewRecorder()
	req := reqWithTenant(1)
	mw(http.NotFoundHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestMiddleware_returns_401_when_token_signature_invalid(t *testing.T) {
	mw := enduser.New(newSigner(t))

	rr := httptest.NewRecorder()
	req := reqWithTenant(1)
	req.Header.Set("X-Session-Token", "not.a.valid.token")
	mw(http.NotFoundHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestMiddleware_returns_401_when_token_expired(t *testing.T) {
	signer := newSigner(t)
	tok, err := signer.Sign(auth.Claims{EndUserID: 5, TenantID: 1, ExpiresAt: time.Now().Add(-time.Hour)})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := reqWithTenant(1)
	req.Header.Set("X-Session-Token", tok)
	enduser.New(signer)(http.NotFoundHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestMiddleware_returns_403_when_token_tenant_does_not_match_context(t *testing.T) {
	signer := newSigner(t)
	tok, err := signer.Sign(auth.Claims{EndUserID: 5, TenantID: 999, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	req := reqWithTenant(1) // api_key resolved tenant 1; token claims tenant 999.
	req.Header.Set("X-Session-Token", tok)
	enduser.New(signer)(http.NotFoundHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestMiddleware_injects_end_user_id_on_success(t *testing.T) {
	signer := newSigner(t)
	tok, err := signer.Sign(auth.Claims{EndUserID: 42, TenantID: 7, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	var captured int64
	rr := httptest.NewRecorder()
	req := reqWithTenant(7)
	req.Header.Set("X-Session-Token", tok)
	enduser.New(signer)(captureEndUser(&captured)).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, int64(42), captured)
}

func TestIDFromContext_returns_false_on_bare_context(t *testing.T) {
	_, ok := enduser.IDFromContext(context.Background())

	assert.False(t, ok)
}

func TestMiddleware_returns_500_when_no_tenant_in_context(t *testing.T) {
	signer := newSigner(t)
	tok, err := signer.Sign(auth.Claims{EndUserID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	// Bare context — no api_key resolved upstream. This is a wiring bug;
	// fail closed with 500.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("X-Session-Token", tok)
	enduser.New(signer)(http.NotFoundHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}
