//go:build integration

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

func TestPlatformAdmin_can_downgrade_tenant_tier(t *testing.T) {
	srv, raw, userID, tenantID, _, _ := newLeaderboardServer(t)
	ctx := context.Background()
	_, err := raw.Exec(ctx, `UPDATE tenants SET tier = 2 WHERE id = $1`, tenantID)
	require.NoError(t, err)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := tfPostForm(t, admin,
		srv.URL+pathControlPanel+"/tenants/"+strconv.FormatInt(tenantID, 10)+"/settings/tier",
		url.Values{"_csrf": {csrf}, "tier": {"0"}})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var gotTier int16
	require.NoError(t, raw.QueryRow(ctx, `SELECT tier FROM tenants WHERE id = $1`, tenantID).Scan(&gotTier))
	assert.Equal(t, int16(0), gotTier)

	var actorID int64
	var oldTier, newTier, direction string
	require.NoError(t, raw.QueryRow(ctx, `
		SELECT actor_user_id, payload->>'old_tier', payload->>'new_tier', payload->>'direction'
		FROM platform_audit_log
		WHERE action = 'control_panel.tenant.tier_change' AND target = $1
		ORDER BY id DESC LIMIT 1`, strconv.FormatInt(tenantID, 10)).Scan(&actorID, &oldTier, &newTier, &direction))
	assert.Equal(t, userID, actorID)
	assert.Equal(t, "2", oldTier)
	assert.Equal(t, "0", newTier)
	assert.Equal(t, "downgrade", direction)
}
