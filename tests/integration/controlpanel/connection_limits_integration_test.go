//go:build integration

// e2e:bucket a

package controlpanel_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlatformAdmin_can_set_and_clear_tenant_connection_limits(t *testing.T) {
	srv, raw, userID, tenantID, _, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")
	endpoint := srv.URL + pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10) + "/rate-limits/connections"

	resp, _ := tfPostForm(t, admin, endpoint, url.Values{
		"_csrf": {csrf}, "sustained": {""}, "ceiling": {""},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var noOpClears int
	require.NoError(t, raw.QueryRow(context.Background(), `
		SELECT count(*) FROM platform_audit_log
		WHERE target = $1 AND action = 'control_panel.connection_limit.clear'`, strconv.FormatInt(tenantID, 10),
	).Scan(&noOpClears))
	assert.Zero(t, noOpClears, "clearing a missing override must not create a false audit event")

	resp, _ = tfPostForm(t, admin, endpoint, url.Values{
		"_csrf": {csrf}, "sustained": {"250000"}, "ceiling": {"500000"},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var sustained, ceiling int64
	require.NoError(t, raw.QueryRow(context.Background(), `
		SELECT sustained, ceiling FROM connection_limit_overrides WHERE tenant_id = $1`, tenantID,
	).Scan(&sustained, &ceiling))
	assert.Equal(t, int64(250_000), sustained)
	assert.Equal(t, int64(500_000), ceiling)

	var action string
	require.NoError(t, raw.QueryRow(context.Background(), `
		SELECT action FROM platform_audit_log
		WHERE target = $1 AND action = 'control_panel.connection_limit.set'
		ORDER BY id DESC LIMIT 1`, strconv.FormatInt(tenantID, 10),
	).Scan(&action))
	assert.Equal(t, "control_panel.connection_limit.set", action)

	resp, _ = tfPostForm(t, admin, endpoint, url.Values{
		"_csrf": {csrf}, "sustained": {""}, "ceiling": {""},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var remaining int
	require.NoError(t, raw.QueryRow(context.Background(), `
		SELECT count(*) FROM connection_limit_overrides WHERE tenant_id = $1`, tenantID,
	).Scan(&remaining))
	assert.Zero(t, remaining)

	var clears int
	require.NoError(t, raw.QueryRow(context.Background(), `
		SELECT count(*) FROM platform_audit_log
		WHERE target = $1 AND action = 'control_panel.connection_limit.clear'`, strconv.FormatInt(tenantID, 10),
	).Scan(&clears))
	assert.Equal(t, 1, clears)
}
