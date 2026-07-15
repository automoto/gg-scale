//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/tenant"
)

func changeRequestSubmitURL(base string, tenantID int64) string {
	return fmt.Sprintf("%s/v1/control-panel/tenants/%d/settings/change-requests", base, tenantID)
}

func tenantTierURL(base string, tenantID int64) string {
	return fmt.Sprintf("%s/v1/control-panel/tenants/%d/settings/tier", base, tenantID)
}

func adminChangeRequestURL(base string, requestID int64, action string) string {
	return fmt.Sprintf("%s/v1/control-panel/admin/change-requests/%d/%s", base, requestID, action)
}

func submitCPForm(t *testing.T, target, csrf string, cookie *http.Cookie, values url.Values) *http.Response {
	t.Helper()
	if values == nil {
		values = url.Values{}
	}
	values.Set("_csrf", csrf)
	return postForm(t, noRedirectClient(), target, values, cookie)
}

func TestBranchFollowup_feature_request_lifecycle_rejects_unavailable_and_already_enabled(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "change-feature")
	ownerID := seedControlPanelUser(t, c, "change-owner@example.test", "correct-horse-battery-staple", false)
	adminID := seedControlPanelUser(t, c, "change-pa@example.test", "correct-horse-battery-staple", true)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	srv, recorder := newControlPanelAndPlayerServerWithConfig(t, c, controlpanel.Config{
		Mount:        true,
		BaseURL:      "http://app.example.test",
		MailFrom:     "no-reply@example.test",
		FleetEnabled: true,
		RelayEnabled: true,
	})
	ownerCookie, ownerCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"change-owner@example.test", "correct-horse-battery-staple")
	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"change-pa@example.test", "correct-horse-battery-staple")

	resp := submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie, url.Values{
		"kind":    {"feature"},
		"feature": {"p2p_relay"},
		"note":    {"Need relay for peer-hosted sessions"},
	})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var relayRequestID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT id FROM tenant_change_requests
		WHERE tenant_id = $1 AND feature = 'p2p_relay' AND status = 'pending'`, tenantID).Scan(&relayRequestID))

	resp = submitCPForm(t, adminChangeRequestURL(srv.URL, relayRequestID, "approve"), adminCSRF, adminCookie, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var enabled bool
	var approvedBy int64
	var reason string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT enabled, approved_by_control_panel_user_id, reason
		FROM feature_grants
		WHERE tenant_id = $1 AND project_id IS NULL AND feature = 'p2p_relay'`, tenantID).
		Scan(&enabled, &approvedBy, &reason))
	assert.True(t, enabled)
	assert.Equal(t, adminID, approvedBy)
	assert.Contains(t, reason, strconv.FormatInt(relayRequestID, 10))
	require.Len(t, recorder.Sent, 1)
	assert.Contains(t, recorder.Sent[0].Subject, "approved")
	assert.Equal(t, []string{"change-owner@example.test"}, recorder.Sent[0].To)

	resp = submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie, url.Values{
		"kind": {"feature"}, "feature": {"p2p_relay"},
	})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var redundant int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM tenant_change_requests
		WHERE tenant_id = $1 AND feature = 'p2p_relay' AND status = 'pending'`, tenantID).Scan(&redundant))
	assert.Zero(t, redundant, "an already-enabled feature cannot be requested through a forged POST")

	resp = submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie, url.Values{
		"kind": {"feature"}, "feature": {"dedicated_servers"}, "note": {"Need managed fleets"},
	})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var fleetRequestID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT id FROM tenant_change_requests
		WHERE tenant_id = $1 AND feature = 'dedicated_servers' AND status = 'pending'`, tenantID).Scan(&fleetRequestID))
	resp = submitCPForm(t, adminChangeRequestURL(srv.URL, fleetRequestID, "deny"), adminCSRF, adminCookie,
		url.Values{"reason": {"Capacity review required"}})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var status, reviewReason string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT status, review_reason FROM tenant_change_requests WHERE id = $1`, fleetRequestID).
		Scan(&status, &reviewReason))
	assert.Equal(t, "denied", status)
	assert.Equal(t, "Capacity review required", reviewReason)
	var fleetGrants int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM feature_grants WHERE tenant_id = $1 AND feature = 'dedicated_servers'`, tenantID).
		Scan(&fleetGrants))
	assert.Zero(t, fleetGrants)
	require.Len(t, recorder.Sent, 2)
	assert.Contains(t, recorder.Sent[1].Body, "Capacity review required")

	for _, feature := range []string{"matchmaker", "unknown"} {
		resp = submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie, url.Values{
			"kind": {"feature"}, "feature": {feature},
		})
		resp.Body.Close()
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	}
	var invalidRequests int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM tenant_change_requests
		WHERE tenant_id = $1 AND status = 'pending'`, tenantID).Scan(&invalidRequests))
	assert.Zero(t, invalidRequests)
}

func TestBranchFollowup_upgrade_request_is_upward_authorized_and_unique(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "change-upgrade")
	otherTenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "change-upgrade-other")
	ownerID := seedControlPanelUser(t, c, "upgrade-owner@example.test", "correct-horse-battery-staple", false)
	managerID := seedControlPanelUser(t, c, "upgrade-admin@example.test", "correct-horse-battery-staple", false)
	memberID := seedControlPanelUser(t, c, "upgrade-member@example.test", "correct-horse-battery-staple", false)
	unrelatedID := seedControlPanelUser(t, c, "upgrade-unrelated@example.test", "correct-horse-battery-staple", false)
	paID := seedControlPanelUser(t, c, "upgrade-pa@example.test", "correct-horse-battery-staple", true)
	_ = paID
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedControlPanelMembership(t, c, managerID, tenantID, "admin")
	seedControlPanelMembership(t, c, memberID, tenantID, "member")
	seedControlPanelMembership(t, c, unrelatedID, otherTenantID, "owner")
	srv, _ := newControlPanelAndPlayerServer(t, c)
	ownerCookie, ownerCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"upgrade-owner@example.test", "correct-horse-battery-staple")
	managerCookie, managerCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"upgrade-admin@example.test", "correct-horse-battery-staple")
	memberCookie, memberCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"upgrade-member@example.test", "correct-horse-battery-staple")
	unrelatedCookie, unrelatedCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"upgrade-unrelated@example.test", "correct-horse-battery-staple")
	paCookie, paCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"upgrade-pa@example.test", "correct-horse-battery-staple")

	settings := getWithCookie(t, fmt.Sprintf("%s/v1/control-panel/tenants/%d/settings", srv.URL, tenantID), ownerCookie)
	assert.Contains(t, settings, "tier_1")
	assert.Contains(t, settings, "tier_3")
	resp := submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie, url.Values{
		"kind": {"tier_upgrade"}, "requested_tier": {"1"}, "note": {"Need more projects"},
	})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var requestID, requesterID int64
	var note, status string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT id, requested_by_user_id, note, status FROM tenant_change_requests
		WHERE tenant_id = $1 AND kind = 'tier_upgrade'`, tenantID).Scan(&requestID, &requesterID, &note, &status))
	assert.Equal(t, ownerID, requesterID)
	assert.Equal(t, "Need more projects", note)
	assert.Equal(t, "pending", status)
	assert.Contains(t, getWithCookie(t, fmt.Sprintf("%s/v1/control-panel/tenants/%d/settings", srv.URL, tenantID), ownerCookie), "Need more projects")
	queue := getWithCookie(t, srv.URL+"/v1/control-panel/admin/change-requests", paCookie)
	assert.Contains(t, queue, "Need more projects")
	assert.Contains(t, queue, "tier_1")
	assert.Contains(t, queue, "upgrade-owner@example.test")

	for _, target := range []string{"0", "-1", "4", "text"} {
		resp = submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie, url.Values{
			"kind": {"tier_upgrade"}, "requested_tier": {target},
		})
		resp.Body.Close()
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	}
	var requests int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_change_requests WHERE tenant_id = $1`, tenantID).Scan(&requests))
	assert.Equal(t, int64(1), requests)

	resp = submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), memberCSRF, memberCookie, url.Values{
		"kind": {"tier_upgrade"}, "requested_tier": {"2"},
	})
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp = submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), unrelatedCSRF, unrelatedCookie, url.Values{
		"kind": {"tier_upgrade"}, "requested_tier": {"2"},
	})
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp = postForm(t, noRedirectClient(), changeRequestSubmitURL(srv.URL, tenantID), url.Values{
		"kind": {"tier_upgrade"}, "requested_tier": {"2"},
	}, ownerCookie)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp = submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), "stale", ownerCookie, url.Values{
		"kind": {"tier_upgrade"}, "requested_tier": {"2"},
	})
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	ownerQueueResp := getResponseWithCookie(t, srv.URL+"/v1/control-panel/admin/change-requests", ownerCookie)
	ownerQueueResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, ownerQueueResp.StatusCode)

	resp = submitCPForm(t, adminChangeRequestURL(srv.URL, requestID, "deny"), paCSRF, paCookie, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	const attempts = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cookie, csrf := ownerCookie, ownerCSRF
			if i%2 == 1 {
				cookie, csrf = managerCookie, managerCSRF
			}
			got := submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), csrf, cookie, url.Values{
				"kind": {"tier_upgrade"}, "requested_tier": {"2"},
			})
			got.Body.Close()
			assert.Equal(t, http.StatusSeeOther, got.StatusCode)
		}(i)
	}
	close(start)
	wg.Wait()
	var pending int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM tenant_change_requests
		WHERE tenant_id = $1 AND kind = 'tier_upgrade' AND status = 'pending'`, tenantID).Scan(&pending))
	assert.Equal(t, int64(1), pending)
}

func TestBranchFollowup_change_request_review_races_have_one_terminal_effect(t *testing.T) {
	c := startCluster(t)
	ownerID := seedControlPanelUser(t, c, "race-owner@example.test", "correct-horse-battery-staple", false)
	pa1ID := seedControlPanelUser(t, c, "race-pa1@example.test", "correct-horse-battery-staple", true)
	pa2ID := seedControlPanelUser(t, c, "race-pa2@example.test", "correct-horse-battery-staple", true)
	_ = pa1ID
	_ = pa2ID
	tenantIDs := make([]int64, 3)
	for i := range tenantIDs {
		tenantIDs[i], _ = seedTenantWithAPIKey(t, c.bootstrapPool, 0, fmt.Sprintf("review-race-%d", i))
		seedControlPanelMembership(t, c, ownerID, tenantIDs[i], "owner")
	}
	srv, recorder := newControlPanelAndPlayerServer(t, c)
	ownerCookie, ownerCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"race-owner@example.test", "correct-horse-battery-staple")
	pa1Cookie, pa1CSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"race-pa1@example.test", "correct-horse-battery-staple")
	pa2Cookie, pa2CSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"race-pa2@example.test", "correct-horse-battery-staple")

	for i, actions := range [][2]string{{"approve", "deny"}, {"approve", "approve"}, {"deny", "deny"}} {
		tenantID := tenantIDs[i]
		resp := submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie, url.Values{
			"kind": {"tier_upgrade"}, "requested_tier": {"1"},
		})
		resp.Body.Close()
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		var requestID int64
		require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
			SELECT id FROM tenant_change_requests WHERE tenant_id = $1 AND status = 'pending'`, tenantID).Scan(&requestID))
		mailBefore := len(recorder.Sent)

		start := make(chan struct{})
		var wg sync.WaitGroup
		for j, action := range actions {
			wg.Add(1)
			go func(j int, action string) {
				defer wg.Done()
				<-start
				cookie, csrf := pa1Cookie, pa1CSRF
				if j == 1 {
					cookie, csrf = pa2Cookie, pa2CSRF
				}
				values := url.Values{}
				if action == "deny" {
					values.Set("reason", "race denial")
				}
				got := submitCPForm(t, adminChangeRequestURL(srv.URL, requestID, action), csrf, cookie, values)
				got.Body.Close()
				assert.Equal(t, http.StatusSeeOther, got.StatusCode)
			}(j, action)
		}
		close(start)
		wg.Wait()

		var terminal, reviewedBy string
		require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
			SELECT status, reviewed_by_user_id::text FROM tenant_change_requests WHERE id = $1`, requestID).
			Scan(&terminal, &reviewedBy))
		assert.Contains(t, []string{"approved", "denied"}, terminal)
		assert.NotEmpty(t, reviewedBy)
		var audits int64
		require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
			SELECT count(*) FROM platform_audit_log
			WHERE target = $1 AND action IN ('control_panel.change_request.approve', 'control_panel.change_request.deny')`,
			strconv.FormatInt(requestID, 10)).Scan(&audits))
		assert.Equal(t, int64(1), audits)
		assert.Equal(t, mailBefore+1, len(recorder.Sent))
		var tier int16
		require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
			`SELECT tier FROM tenants WHERE id = $1`, tenantID).Scan(&tier))
		if terminal == "approved" {
			assert.Equal(t, int16(1), tier)
		} else {
			assert.Equal(t, int16(0), tier)
		}
	}
}

func TestBranchFollowup_stale_upgrade_approval_rolls_back_request_audit_and_mail(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "stale-upgrade")
	ownerID := seedControlPanelUser(t, c, "stale-owner@example.test", "correct-horse-battery-staple", false)
	seedControlPanelUser(t, c, "stale-pa@example.test", "correct-horse-battery-staple", true)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	srv, recorder := newControlPanelAndPlayerServer(t, c)
	ownerCookie, ownerCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"stale-owner@example.test", "correct-horse-battery-staple")
	paCookie, paCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"stale-pa@example.test", "correct-horse-battery-staple")
	resp := submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie, url.Values{
		"kind": {"tier_upgrade"}, "requested_tier": {"1"},
	})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var requestID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM tenant_change_requests WHERE tenant_id = $1 AND status = 'pending'`, tenantID).Scan(&requestID))
	resp = submitCPForm(t, tenantTierURL(srv.URL, tenantID), paCSRF, paCookie, url.Values{"tier": {"2"}})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	mailBefore := len(recorder.Sent)
	resp = submitCPForm(t, adminChangeRequestURL(srv.URL, requestID, "approve"), paCSRF, paCookie, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var tier int16
	var status string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT tier FROM tenants WHERE id = $1`, tenantID).Scan(&tier))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT status FROM tenant_change_requests WHERE id = $1`, requestID).Scan(&status))
	assert.Equal(t, int16(2), tier)
	assert.Equal(t, "pending", status)
	assert.Equal(t, mailBefore, len(recorder.Sent))
	var approvalAudits int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM platform_audit_log
		WHERE target = $1 AND action = 'control_panel.change_request.approve'`, strconv.FormatInt(requestID, 10)).
		Scan(&approvalAudits))
	assert.Zero(t, approvalAudits)
}

func TestBranchFollowup_platform_admin_tier_change_authorization_and_audit_chain(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "tier-direct")
	deletedTenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 1, "tier-deleted")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET deleted_at = now() WHERE id = $1`, deletedTenantID)
	require.NoError(t, err)
	pa1ID := seedControlPanelUser(t, c, "tier-pa1@example.test", "correct-horse-battery-staple", true)
	pa2ID := seedControlPanelUser(t, c, "tier-pa2@example.test", "correct-horse-battery-staple", true)
	ownerID := seedControlPanelUser(t, c, "tier-owner@example.test", "correct-horse-battery-staple", false)
	adminID := seedControlPanelUser(t, c, "tier-admin@example.test", "correct-horse-battery-staple", false)
	memberID := seedControlPanelUser(t, c, "tier-member@example.test", "correct-horse-battery-staple", false)
	unrelatedID := seedControlPanelUser(t, c, "tier-unrelated@example.test", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	seedControlPanelMembership(t, c, memberID, tenantID, "member")
	otherTenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "tier-unrelated")
	seedControlPanelMembership(t, c, unrelatedID, otherTenantID, "owner")
	srv, _ := newControlPanelAndPlayerServer(t, c)
	pa1Cookie, pa1CSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"tier-pa1@example.test", "correct-horse-battery-staple")
	pa2Cookie, pa2CSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"tier-pa2@example.test", "correct-horse-battery-staple")
	ownerCookie, ownerCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"tier-owner@example.test", "correct-horse-battery-staple")
	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"tier-admin@example.test", "correct-horse-battery-staple")
	memberCookie, memberCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"tier-member@example.test", "correct-horse-battery-staple")
	unrelatedCookie, unrelatedCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"tier-unrelated@example.test", "correct-horse-battery-staple")

	settings := getWithCookie(t, fmt.Sprintf("%s/v1/control-panel/tenants/%d/settings", srv.URL, tenantID), pa1Cookie)
	assert.Contains(t, settings, `name="tier"`)
	assert.Contains(t, settings, "tier_0")
	resp := submitCPForm(t, tenantTierURL(srv.URL, tenantID), pa1CSRF, pa1Cookie, url.Values{"tier": {"0"}})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var tier int16
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT tier FROM tenants WHERE id = $1`, tenantID).Scan(&tier))
	assert.Equal(t, int16(0), tier)

	for _, actor := range []struct {
		cookie *http.Cookie
		csrf   string
	}{
		{ownerCookie, ownerCSRF},
		{adminCookie, adminCSRF},
		{memberCookie, memberCSRF},
		{unrelatedCookie, unrelatedCSRF},
	} {
		resp = submitCPForm(t, tenantTierURL(srv.URL, tenantID), actor.csrf, actor.cookie, url.Values{"tier": {"2"}})
		resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	}
	resp = postForm(t, noRedirectClient(), tenantTierURL(srv.URL, tenantID), url.Values{"tier": {"2"}}, pa1Cookie)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp = submitCPForm(t, tenantTierURL(srv.URL, tenantID), "stale", pa1Cookie, url.Values{"tier": {"2"}})
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	for _, target := range []string{"-1", "4", "text"} {
		resp = submitCPForm(t, tenantTierURL(srv.URL, tenantID), pa1CSRF, pa1Cookie, url.Values{"tier": {target}})
		resp.Body.Close()
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	}
	resp = submitCPForm(t, tenantTierURL(srv.URL, 9_999_999), pa1CSRF, pa1Cookie, url.Values{"tier": {"1"}})
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = submitCPForm(t, tenantTierURL(srv.URL, deletedTenantID), pa1CSRF, pa1Cookie, url.Values{"tier": {"1"}})
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	var auditBefore int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM platform_audit_log
		WHERE action = 'control_panel.tenant.tier_change' AND target = $1`, strconv.FormatInt(tenantID, 10)).
		Scan(&auditBefore))
	resp = submitCPForm(t, tenantTierURL(srv.URL, tenantID), pa1CSRF, pa1Cookie, url.Values{"tier": {"0"}})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var auditAfter int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM platform_audit_log
		WHERE action = 'control_panel.tenant.tier_change' AND target = $1`, strconv.FormatInt(tenantID, 10)).
		Scan(&auditAfter))
	assert.Equal(t, auditBefore, auditAfter)

	const attempts = 30
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cookie, csrf, target := pa1Cookie, pa1CSRF, "1"
			if i%2 == 1 {
				cookie, csrf, target = pa2Cookie, pa2CSRF, "3"
			}
			got := submitCPForm(t, tenantTierURL(srv.URL, tenantID), csrf, cookie, url.Values{"tier": {target}})
			got.Body.Close()
			assert.Equal(t, http.StatusSeeOther, got.StatusCode)
		}(i)
	}
	close(start)
	wg.Wait()

	type auditRow struct {
		actor             int64
		oldTier, newTier  int16
		direction, target string
	}
	rows, err := c.bootstrapPool.Query(context.Background(), `
		SELECT actor_user_id, (payload->>'old_tier')::smallint, (payload->>'new_tier')::smallint,
		       payload->>'direction', target
		FROM platform_audit_log
		WHERE action = 'control_panel.tenant.tier_change' AND target = $1
		ORDER BY id`, strconv.FormatInt(tenantID, 10))
	require.NoError(t, err)
	defer rows.Close()
	var chain []auditRow
	for rows.Next() {
		var row auditRow
		require.NoError(t, rows.Scan(&row.actor, &row.oldTier, &row.newTier, &row.direction, &row.target))
		chain = append(chain, row)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, chain)
	previous := int16(2)
	for _, row := range chain {
		assert.Equal(t, previous, row.oldTier)
		assert.Contains(t, []int64{pa1ID, pa2ID}, row.actor)
		assert.Equal(t, strconv.FormatInt(tenantID, 10), row.target)
		if row.newTier < row.oldTier {
			assert.Equal(t, "downgrade", row.direction)
		} else {
			assert.Equal(t, "upgrade", row.direction)
		}
		previous = row.newTier
	}
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT tier FROM tenants WHERE id = $1`, tenantID).Scan(&tier))
	assert.Equal(t, previous, tier)
}

func TestBranchFollowup_platform_admin_downgrade_preserves_data_and_blocks_only_growth(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "tier-effects")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = true WHERE id = $1`, tenantID)
	require.NoError(t, err)
	seedControlPanelUser(t, c, "tier-effects-pa@example.test", "correct-horse-battery-staple", true)
	srv, _ := newControlPanelAndPlayerServer(t, c)
	paCookie, paCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL,
		"tier-effects-pa@example.test", "correct-horse-battery-staple")

	access := anonymousLogin(t, srv.URL, "tier-effects")
	storageTarget := srv.URL + "/v1/storage/objects/existing"
	stored := storageRequestStatus(http.MethodPut, storageTarget, "tier-effects", access, []byte(`{"state":"existing"}`), "")
	require.NoError(t, stored.err)
	require.Equal(t, http.StatusOK, stored.status, stored.body)
	_, err = c.bootstrapPool.Exec(context.Background(), `
		INSERT INTO projects (tenant_id, name)
		VALUES ($1, 'extra-1'), ($1, 'extra-2'), ($1, 'extra-3')`, tenantID)
	require.NoError(t, err)
	fillPlayersTo(t, c, tenantID, projectID, branchPlayerLimit+1, "tier-effects-over")
	tier0StorageLimit := quota.LimitsForClass(tenant.Tier0).StorageBytes
	_, err = c.bootstrapPool.Exec(context.Background(), `
		INSERT INTO tenant_storage_usage (tenant_id, total_bytes)
		VALUES ($1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET total_bytes = EXCLUDED.total_bytes`, tenantID, tier0StorageLimit+100)
	require.NoError(t, err)

	resp := submitCPForm(t, tenantTierURL(srv.URL, tenantID), paCSRF, paCookie, url.Values{"tier": {"0"}})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var tier int16
	var projects, players, objects int64
	var enforced bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT tier, enforce_quotas,
		       (SELECT count(*) FROM projects WHERE tenant_id = $1 AND deleted_at IS NULL),
		       (SELECT count(*) FROM project_players WHERE tenant_id = $1 AND deleted_at IS NULL),
		       (SELECT count(*) FROM storage_objects WHERE tenant_id = $1 AND deleted_at IS NULL)
		FROM tenants WHERE id = $1`, tenantID).Scan(&tier, &enforced, &projects, &players, &objects))
	assert.Equal(t, int16(0), tier)
	assert.True(t, enforced)
	assert.Equal(t, int64(4), projects)
	assert.Equal(t, branchPlayerLimit+1, players)
	assert.Equal(t, int64(1), objects)

	resp, body := authedReq(t, http.MethodGet, srv.URL+"/v1/profile", "tier-effects", access, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	read := storageRequestStatus(http.MethodGet, storageTarget, "tier-effects", access, nil, "")
	require.NoError(t, read.err)
	assert.Equal(t, http.StatusOK, read.status, read.body)
	resp, projectBody := createProjectRequest(t, noRedirectClient(), tenantProjectCreateURL(srv.URL, tenantID),
		paCSRF, "blocked-after-downgrade", paCookie)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, projectBody)
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "tier-effects", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	growth := storageRequestStatus(http.MethodPut, storageTarget, "tier-effects", access,
		[]byte(`{"state":"existing-and-larger"}`), "")
	require.NoError(t, growth.err)
	assert.Equal(t, http.StatusForbidden, growth.status, growth.body)
	shrunk := storageRequestStatus(http.MethodPut, storageTarget, "tier-effects", access, []byte(`0`), "")
	require.NoError(t, shrunk.err)
	assert.Equal(t, http.StatusOK, shrunk.status, shrunk.body)

	resp = submitCPForm(t, tenantTierURL(srv.URL, tenantID), paCSRF, paCookie, url.Values{"tier": {"2"}})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, projectBody = createProjectRequest(t, noRedirectClient(), tenantProjectCreateURL(srv.URL, tenantID),
		paCSRF, "allowed-after-upgrade", paCookie)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, projectBody)
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "tier-effects", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	growth = storageRequestStatus(http.MethodPut, storageTarget, "tier-effects", access,
		[]byte(`{"state":"growth-restored"}`), "")
	require.NoError(t, growth.err)
	assert.Equal(t, http.StatusOK, growth.status, growth.body)
}

func TestBranchFollowup_audit_hygiene_and_final_database_invariants(t *testing.T) {
	c := startCluster(t)
	const (
		apiKey      = "ops-secret-api-key"
		password    = "ops-secret-password"
		customToken = "ops-secret-custom-token-material"
	)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, apiKey)
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = true, custom_token_secret = $2 WHERE id = $1`, tenantID, []byte(customToken))
	require.NoError(t, err)
	ownerID := seedControlPanelUser(t, c, "ops-owner@example.test", password, false)
	paID := seedControlPanelUser(t, c, "ops-pa@example.test", password, true)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	srv, _ := newControlPanelAndPlayerServerWithConfig(t, c, controlpanel.Config{
		Mount:        true,
		BaseURL:      "http://app.example.test",
		MailFrom:     "no-reply@example.test",
		FleetEnabled: true,
		RelayEnabled: true,
	})
	ownerCookie, ownerCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "ops-owner@example.test", password)
	paCookie, paCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "ops-pa@example.test", password)

	submitFeature := func(feature string) int64 {
		resp := submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie,
			url.Values{"kind": {"feature"}, "feature": {feature}})
		resp.Body.Close()
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		var id int64
		require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
			SELECT id FROM tenant_change_requests
			WHERE tenant_id = $1 AND feature = $2 AND status = 'pending'`, tenantID, feature).Scan(&id))
		return id
	}
	approvedID := submitFeature("p2p_relay")
	resp := submitCPForm(t, adminChangeRequestURL(srv.URL, approvedID, "approve"), paCSRF, paCookie, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	deniedID := submitFeature("dedicated_servers")
	resp = submitCPForm(t, adminChangeRequestURL(srv.URL, deniedID, "deny"), paCSRF, paCookie,
		url.Values{"reason": {"Capacity review required"}})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	for _, target := range []string{"1", "0"} {
		resp = submitCPForm(t, tenantTierURL(srv.URL, tenantID), paCSRF, paCookie, url.Values{"tier": {target}})
		resp.Body.Close()
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	}
	resp = submitCPForm(t, changeRequestSubmitURL(srv.URL, tenantID), ownerCSRF, ownerCookie,
		url.Values{"kind": {"tier_upgrade"}, "requested_tier": {"1"}})
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	access := anonymousLogin(t, srv.URL, apiKey)
	stored := storageRequestStatus(http.MethodPut, srv.URL+"/v1/storage/objects/invariant",
		apiKey, access, []byte(`{"ok":true}`), "")
	require.NoError(t, stored.err)
	require.Equal(t, http.StatusOK, stored.status, stored.body)

	rows, err := c.bootstrapPool.Query(context.Background(), `
		SELECT actor_user_id, action, target, payload::text
		FROM platform_audit_log
		WHERE action IN ('control_panel.change_request.approve', 'control_panel.change_request.deny',
		                 'control_panel.tenant.tier_change')
		ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	counts := map[string]int{}
	knownTargets := map[string]bool{
		strconv.FormatInt(approvedID, 10): true,
		strconv.FormatInt(deniedID, 10):   true,
		strconv.FormatInt(tenantID, 10):   true,
	}
	secrets := []string{apiKey, password, customToken, ownerCSRF, paCSRF, ownerCookie.Value, paCookie.Value}
	for rows.Next() {
		var actor int64
		var action, target, payload string
		require.NoError(t, rows.Scan(&actor, &action, &target, &payload))
		assert.Equal(t, paID, actor)
		assert.True(t, knownTargets[target], target)
		assert.False(t, containsAnySecret(payload, secrets...), payload)
		counts[action]++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]int{
		"control_panel.change_request.approve": 1,
		"control_panel.change_request.deny":    1,
		"control_panel.tenant.tier_change":     2,
	}, counts)

	var quotaViolations, meteringMismatches, duplicatePending int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM (
			SELECT t.tier,
			       (SELECT count(*) FROM projects p WHERE p.tenant_id = t.id AND p.deleted_at IS NULL) projects,
			       (SELECT count(*) FROM project_players pp WHERE pp.tenant_id = t.id AND pp.deleted_at IS NULL) players
			FROM tenants t WHERE t.enforce_quotas AND t.deleted_at IS NULL
		) usage
		WHERE (tier = 0 AND (projects > 3 OR players > 250000))
		   OR (tier = 1 AND (projects > 10 OR players > 1000000))
		   OR (tier = 2 AND (projects > 20 OR players > 5000000))`).Scan(&quotaViolations))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		WITH actual AS (
			SELECT tenant_id, COALESCE(sum(octet_length(value::text)) FILTER (WHERE deleted_at IS NULL), 0) bytes
			FROM storage_objects GROUP BY tenant_id
		)
		SELECT count(*) FROM tenants t
		LEFT JOIN tenant_storage_usage u ON u.tenant_id = t.id
		LEFT JOIN actual a ON a.tenant_id = t.id
		WHERE COALESCE(u.total_bytes, 0) <> COALESCE(a.bytes, 0)`).Scan(&meteringMismatches))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM (
			SELECT tenant_id, kind, COALESCE(feature, '')
			FROM tenant_change_requests WHERE status = 'pending'
			GROUP BY tenant_id, kind, COALESCE(feature, '')
			HAVING count(*) > 1
		) duplicates`).Scan(&duplicatePending))
	assert.Zero(t, quotaViolations)
	assert.Zero(t, meteringMismatches)
	assert.Zero(t, duplicatePending)
}

func responseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return string(raw)
}

func containsAnySecret(body string, secrets ...string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(body, secret) {
			return true
		}
	}
	return false
}

var _ = sync.WaitGroup{}
