//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
)

const relayAllocPassword = "correct-horse-battery-staple"

type relayAllocOverrideFixture struct {
	c        *cluster
	srv      string
	tenantID int64
}

// newRelayAllocOverrideFixture seeds one tenant with an owner, an admin, and a
// platform admin who is not a member.
func newRelayAllocOverrideFixture(t *testing.T, name string) *relayAllocOverrideFixture {
	t.Helper()
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, name)
	ownerID := seedControlPanelUser(t, c, name+"-owner@example.test", relayAllocPassword, false)
	adminID := seedControlPanelUser(t, c, name+"-admin@example.test", relayAllocPassword, false)
	seedControlPanelUser(t, c, name+"-pa@example.test", relayAllocPassword, true)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	srv, _ := newControlPanelAndPlayerServerWithConfig(t, c, controlpanel.Config{
		Mount:        true,
		BaseURL:      "http://app.example.test",
		MailFrom:     "no-reply@example.test",
		RelayEnabled: true,
	})
	return &relayAllocOverrideFixture{c: c, srv: srv.URL, tenantID: tenantID}
}

// post submits a control-panel form as the user with the given email.
func (f *relayAllocOverrideFixture) post(t *testing.T, email, path string, values url.Values) int {
	t.Helper()
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, f.srv, email, relayAllocPassword)
	target := fmt.Sprintf("%s/v1/control-panel/tenants/%d%s", f.srv, f.tenantID, path)
	resp := submitCPForm(t, target, csrf, cookie, values)
	resp.Body.Close()
	return resp.StatusCode
}

func (f *relayAllocOverrideFixture) overrideRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM tenant_quota_overrides
		WHERE tenant_id = $1 AND axis = 'relay_allocations'`, f.tenantID).Scan(&n))
	return n
}

func TestRelayAllocationsOverride_should_refuse_tenant_roles_setting_it_directly(t *testing.T) {
	f := newRelayAllocOverrideFixture(t, "relay-alloc-direct")

	for _, role := range []string{"owner", "admin"} {
		code := f.post(t, "relay-alloc-direct-"+role+"@example.test", "/rate-limits/quotas",
			url.Values{"axis": {"relay_allocations"}, "limit": {"100000"}})
		assert.Equal(t, http.StatusForbidden, code, role)
	}

	assert.Zero(t, f.overrideRows(t))
}

func TestRelayAllocationsOverride_should_refuse_tenant_change_request(t *testing.T) {
	f := newRelayAllocOverrideFixture(t, "relay-alloc-request")

	code := f.post(t, "relay-alloc-request-owner@example.test", "/settings/change-requests", url.Values{
		"kind": {"quota_override"}, "axis": {"relay_allocations"}, "requested_limit": {"100000"},
	})
	require.Equal(t, http.StatusSeeOther, code)

	var requests int64
	require.NoError(t, f.c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM tenant_change_requests
		WHERE tenant_id = $1 AND feature = 'relay_allocations'`, f.tenantID).Scan(&requests))
	assert.Zero(t, requests)
}

func TestRelayAllocationsOverride_should_reject_change_request_rows_in_the_database(t *testing.T) {
	f := newRelayAllocOverrideFixture(t, "relay-alloc-db")

	_, err := f.c.bootstrapPool.Exec(context.Background(), `
		INSERT INTO tenant_change_requests (tenant_id, kind, feature, requested_limit)
		VALUES ($1, 'quota_override', 'relay_allocations', 100000)`, f.tenantID)

	assert.ErrorContains(t, err, "tenant_change_requests_feature_check")
}

func TestRelayAllocationsOverride_should_let_platform_admin_set_it(t *testing.T) {
	f := newRelayAllocOverrideFixture(t, "relay-alloc-pa")

	code := f.post(t, "relay-alloc-pa-pa@example.test", "/rate-limits/quotas",
		url.Values{"axis": {"relay_allocations"}, "limit": {"5000"}})
	require.Equal(t, http.StatusSeeOther, code)

	var limit int64
	require.NoError(t, f.c.bootstrapPool.QueryRow(context.Background(), `
		SELECT "limit" FROM tenant_quota_overrides
		WHERE tenant_id = $1 AND axis = 'relay_allocations'`, f.tenantID).Scan(&limit))
	assert.Equal(t, int64(5000), limit)
}

func TestRelayAllocationsOverride_should_let_platform_admin_clear_it(t *testing.T) {
	f := newRelayAllocOverrideFixture(t, "relay-alloc-clear")
	require.Equal(t, http.StatusSeeOther, f.post(t, "relay-alloc-clear-pa@example.test", "/rate-limits/quotas",
		url.Values{"axis": {"relay_allocations"}, "limit": {"5000"}}))

	code := f.post(t, "relay-alloc-clear-pa@example.test", "/rate-limits/quotas",
		url.Values{"axis": {"relay_allocations"}, "limit": {""}})
	require.Equal(t, http.StatusSeeOther, code)

	assert.Zero(t, f.overrideRows(t))
}
