package controlpanel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/rbac"
)

// adminHandlerRequest builds a request carrying a tenant-admin session and the
// {tenantID} path param, so a handler's authorization guard can be exercised in
// isolation. A tenant admin holds project:manage (the shared route guard) but
// not the finer capabilities these handlers now enforce.
func adminHandlerRequest(t *testing.T, form url.Values) (*rbac.Authorizer, *http.Request) {
	t.Helper()
	auth, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)
	require.NoError(t, auth.SetControlPanelMembershipRole(5, 7, "admin"))

	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("tenantID", "7")
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = contextWithSession(ctx, controlPanelSession{User: controlPanelUser{ID: 5}})
	return auth, req.WithContext(ctx)
}

func TestCreateAPIKeyHandler_admin_cannot_create_secret_key(t *testing.T) {
	auth, req := adminHandlerRequest(t, url.Values{"key_type": {"secret"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.createAPIKeyHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "tenant admin must not create secret keys")
}

func TestInviteTeammateHandler_admin_denied(t *testing.T) {
	auth, req := adminHandlerRequest(t, url.Values{"email": {"x@example.com"}, "role": {"admin"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.inviteTeammateHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "team invites are owner-only")
}

func TestUpdateMemberRoleHandler_admin_denied(t *testing.T) {
	auth, req := adminHandlerRequest(t, url.Values{"action": {"grant"}, "role": {rbac.RoleFleetOperator}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateMemberRoleHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "granting roles (e.g. fleet_operator) is owner-only")
}

func TestUpdateTenantStorageLimitHandler_non_platform_admin_denied(t *testing.T) {
	// adminHandlerRequest builds a non-platform-admin session; raising the
	// tenant-wide storage ceiling is platform-admin only.
	auth, req := adminHandlerRequest(t, url.Values{"max_value_bytes": {"2048"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateTenantStorageLimitHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "tenant storage ceiling is platform-admin only")
}

func TestUpdateTenantTierHandler_non_platform_admin_denied(t *testing.T) {
	auth, req := adminHandlerRequest(t, url.Values{"tier": {"0"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateTenantTierHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "direct tenant tier changes are platform-admin only")
}
