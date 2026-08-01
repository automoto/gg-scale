//go:build integration

package controlpanel_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSteamKey = "0123456789ABCDEF0123456789ABCDEF"

func steamAuthPath(tenantID, projectID int64) string {
	return pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/steam-auth"
}

func projectSettingsPath(tenantID, projectID int64) string {
	return pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/settings"
}

func steamConfigRow(t *testing.T, raw *pgxpool.Pool, projectID int64) (string, []byte) {
	t.Helper()
	var appID string
	var key []byte
	require.NoError(t, raw.QueryRow(context.Background(),
		`SELECT steam_app_id, steam_web_api_key FROM projects WHERE id = $1`, projectID).
		Scan(&appID, &key))
	return appID, key
}

func TestSteamAuth_save_persists_sealed_key_and_audits(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, body := tfPostForm(t, admin, srv.URL+steamAuthPath(tenantID, projectA),
		url.Values{"_csrf": {csrf}, "steam_app_id": {"480"}, "steam_web_api_key": {testSteamKey}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)

	appID, key := steamConfigRow(t, raw, projectA)
	assert.Equal(t, "480", appID)
	require.NotEmpty(t, key)
	assert.NotEqual(t, []byte(testSteamKey), key, "the key must be sealed at rest")

	var payload string
	require.NoError(t, raw.QueryRow(context.Background(), `
		SELECT payload::text FROM platform_audit_log
		WHERE action = 'control_panel.steam_auth.update' AND target = $1
		ORDER BY id DESC LIMIT 1`, strconv.FormatInt(projectA, 10)).Scan(&payload))
	assert.NotContains(t, payload, testSteamKey, "audit payload must not carry key material")
	assert.Contains(t, payload, `"app_id": "480"`)
}

func TestSteamAuth_settings_page_never_shows_the_key(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := tfPostForm(t, admin, srv.URL+steamAuthPath(tenantID, projectA),
		url.Values{"_csrf": {csrf}, "steam_app_id": {"480"}, "steam_web_api_key": {testSteamKey}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	resp, page := tfGet(t, admin, srv.URL+projectSettingsPath(tenantID, projectA))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, page, testSteamKey)
	assert.Contains(t, page, "configured — leave blank to keep")
}

func TestSteamAuth_blank_key_keeps_stored_key(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := tfPostForm(t, admin, srv.URL+steamAuthPath(tenantID, projectA),
		url.Values{"_csrf": {csrf}, "steam_app_id": {"480"}, "steam_web_api_key": {testSteamKey}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, before := steamConfigRow(t, raw, projectA)

	resp, _ = tfPostForm(t, admin, srv.URL+steamAuthPath(tenantID, projectA),
		url.Values{"_csrf": {csrf}, "steam_app_id": {"730"}, "steam_web_api_key": {""}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	appID, after := steamConfigRow(t, raw, projectA)
	assert.Equal(t, "730", appID)
	assert.Equal(t, before, after, "a blank key field must keep the stored key")
}

func TestSteamAuth_enable_without_any_key_is_rejected(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, body := tfPostForm(t, admin, srv.URL+steamAuthPath(tenantID, projectA),
		url.Values{"_csrf": {csrf}, "steam_app_id": {"480"}, "steam_web_api_key": {""}})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, "Enter your publisher Web API key")

	appID, key := steamConfigRow(t, raw, projectA)
	assert.Empty(t, appID)
	assert.Empty(t, key)
}

func TestSteamAuth_clear_disables_and_deletes_key(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := tfPostForm(t, admin, srv.URL+steamAuthPath(tenantID, projectA),
		url.Values{"_csrf": {csrf}, "steam_app_id": {"480"}, "steam_web_api_key": {testSteamKey}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	resp, _ = tfPostForm(t, admin, srv.URL+steamAuthPath(tenantID, projectA),
		url.Values{"_csrf": {csrf}, "steam_clear": {"1"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	appID, key := steamConfigRow(t, raw, projectA)
	assert.Empty(t, appID)
	assert.Empty(t, key)
}

func TestSteamAuth_bad_app_id_rejected_and_nothing_persists(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, body := tfPostForm(t, admin, srv.URL+steamAuthPath(tenantID, projectA),
		url.Values{"_csrf": {csrf}, "steam_app_id": {"not-a-number"}, "steam_web_api_key": {testSteamKey}})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, "Enter the numeric Steam App ID")

	appID, key := steamConfigRow(t, raw, projectA)
	assert.Empty(t, appID)
	assert.Empty(t, key)
}
