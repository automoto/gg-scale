//go:build integration

package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/controlpanel"
)

func getRemoteConfig(t *testing.T, baseURL, apiKey, ifNoneMatch string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/config", nil)
	require.NoError(t, err)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, body
}

func TestRemoteConfig_controlPanelEditIsVisibleBeforePlayerLoginAndSupportsRevalidation(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "config-secret")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "config-publishable", "publishable")
	adminID := seedControlPanelUser(t, c, "config-admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")

	panel := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	api := newServerForCluster(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, panel.URL, "config-admin@example.com", "correct-horse-battery-staple")

	resp, body := getRemoteConfig(t, api.URL, "config-publishable", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.JSONEq(t, `{}`, string(body))
	initialETag := resp.Header.Get("ETag")
	require.NotEmpty(t, initialETag)
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	configPath := panel.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/config"
	form := url.Values{
		"_csrf":  {csrf},
		"config": {`{"minimum_client_version":"1.4.0","maintenance_mode":false}`},
	}
	saveResp := postForm(t, noRedirectClient(), configPath, form, cookie)
	require.NoError(t, saveResp.Body.Close())
	require.Equal(t, http.StatusSeeOther, saveResp.StatusCode)

	resp, body = getRemoteConfig(t, api.URL, "config-publishable", initialETag)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.JSONEq(t, `{"minimum_client_version":"1.4.0","maintenance_mode":false}`, string(body))
	updatedETag := resp.Header.Get("ETag")
	assert.NotEqual(t, initialETag, updatedETag)

	resp, body = getRemoteConfig(t, api.URL, "config-publishable", updatedETag)
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Equal(t, updatedETag, resp.Header.Get("ETag"))
	assert.Empty(t, body)
}

func TestRemoteConfig_rejectsMissingAndUnpinnedAPIKeys(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "config-project")
	seedAPIKey(t, c.bootstrapPool, tenantID, nil, "config-tenant", "publishable")
	api := newServerForCluster(t, c)

	resp, _ := getRemoteConfig(t, api.URL, "", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, body := getRemoteConfig(t, api.URL, "config-tenant", "")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "api key has no project pin")
}

func TestRemoteConfig_controlPanelRejectsInvalidJSONWithoutChangingConfig(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "config-invalid")
	ownerID := seedControlPanelUser(t, c, "config-invalid@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	_, err := c.bootstrapPool.Exec(t.Context(), `UPDATE projects SET remote_config = '{"enabled": true}' WHERE id = $1`, projectID)
	require.NoError(t, err)

	panel := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, panel.URL, "config-invalid@example.com", "correct-horse-battery-staple")
	configPath := panel.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/config"
	resp := postForm(t, noRedirectClient(), configPath, url.Values{
		"_csrf":  {csrf},
		"config": {`["not", "an", "object"]`},
	}, cookie)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))

	var stored map[string]bool
	require.NoError(t, c.bootstrapPool.QueryRow(t.Context(),
		`SELECT remote_config FROM projects WHERE id = $1`, projectID).Scan(&stored))
	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	assert.JSONEq(t, `{"enabled":true}`, string(encoded))
}
