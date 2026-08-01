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

// roleHandlerRequest builds a request carrying a session with the given
// membership role and the {tenantID} path param, so a handler's authorization
// guard can be exercised in isolation.
func roleHandlerRequest(t *testing.T, role string, form url.Values) (*rbac.Authorizer, *http.Request) {
	t.Helper()
	auth, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)
	require.NoError(t, auth.SetControlPanelMembershipRole(5, 7, role))

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

// adminHandlerRequest is roleHandlerRequest for a tenant admin, who holds
// project:manage (the shared route guard) but not the finer capabilities some
// handlers enforce.
func adminHandlerRequest(t *testing.T, form url.Values) (*rbac.Authorizer, *http.Request) {
	t.Helper()
	return roleHandlerRequest(t, "admin", form)
}

func TestCreateAPIKeyHandler_member_cannot_create_secret_key(t *testing.T) {
	auth, req := roleHandlerRequest(t, "member", url.Values{"key_type": {"secret"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.createAPIKeyHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "members must not create secret keys")
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
	auth, req := adminHandlerRequest(t, url.Values{"max_value_mb": {"2048"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateTenantStorageLimitHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "tenant storage ceiling is platform-admin only")
}

func TestUpdateQuotaOverrideHandler_non_platform_admin_denied(t *testing.T) {
	// adminHandlerRequest builds a non-platform-admin session; tenants must
	// not lift their own quota limits — they file a change request instead.
	auth, req := adminHandlerRequest(t, url.Values{"axis": {"open_sessions"}, "limit": {"9000"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateQuotaOverrideHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "quota overrides are platform-admin only")
}

func TestUpdateTenantTierHandler_non_platform_admin_denied(t *testing.T) {
	auth, req := adminHandlerRequest(t, url.Values{"tier": {"0"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateTenantTierHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "direct tenant tier changes are platform-admin only")
}

func TestUpdateRemoteConfigHandler_member_denied(t *testing.T) {
	auth, req := roleHandlerRequest(t, "member", url.Values{"config": {`{"maintenance_mode":true}`}})
	rctx := chi.RouteContext(req.Context())
	rctx.URLParams.Add("projectID", "8")
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateRemoteConfigHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "remote config requires project config permission")
}

func TestUpdateCustomTokenKeyHandler_member_denied(t *testing.T) {
	auth, req := roleHandlerRequest(t, "member", url.Values{"custom_token_public_key": {"irrelevant"}})
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateCustomTokenKeyHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "the signing key requires custom_token manage")
}

func TestUpdateSteamAuthHandler_member_denied(t *testing.T) {
	auth, req := roleHandlerRequest(t, "member", url.Values{
		"steam_app_id": {"480"}, "steam_web_api_key": {"0123456789ABCDEF0123456789ABCDEF"},
	})
	rctx := chi.RouteContext(req.Context())
	rctx.URLParams.Add("projectID", "8")
	h := &Handler{rbac: auth}

	rr := httptest.NewRecorder()
	h.updateSteamAuthHandler(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code, "steam auth settings require project config permission")
}
