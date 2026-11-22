//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/secretseal"
)

const fleetSealMarker = "tok-SEALMARKER-7f3a"

// sealedFleetPanel is a control panel with a credential cipher and one
// plugin fleet created through the dashboard form.
type sealedFleetPanel struct {
	c        *cluster
	mgr      *fleet.Manager
	cookie   *http.Cookie
	csrf     string
	tenantID int64
	base     string
	fleetID  int64
}

func newSealedFleetPanel(t *testing.T) sealedFleetPanel {
	t.Helper()
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-seal")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetPluginBackend)
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	cipher, err := secretseal.NewCipherKeyring(key)
	require.NoError(t, err)
	srv, mgr := newControlPanelFleetServerWithCipher(t, c, newStubBackend("plugin:x"), nil, cipher)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	p := sealedFleetPanel{
		c: c, mgr: mgr, cookie: cookie, csrf: csrf, tenantID: tenantID,
		base: srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
			"/projects/" + strconv.FormatInt(projectID, 10) + "/fleets",
	}
	resp := postFormWithCookie(t, p.base, url.Values{
		"_csrf": {csrf}, "name": {"sealed"}, "backend": {"plugin:x"},
		"config_key[]": {"api_token", "region"}, "config_value[]": {fleetSealMarker, "GRA9"},
	}, cookie)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_ = resp.Body.Close()
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM fleets WHERE tenant_id = $1 AND name = 'sealed'`, tenantID).Scan(&p.fleetID))
	return p
}

func (p sealedFleetPanel) storedConfig(t *testing.T) string {
	t.Helper()
	var raw string
	require.NoError(t, p.c.bootstrapPool.QueryRow(context.Background(),
		`SELECT config::text FROM fleets WHERE id = $1`, p.fleetID).Scan(&raw))
	return raw
}

func (p sealedFleetPanel) config(t *testing.T) map[string]string {
	t.Helper()
	f, err := p.mgr.Fleets().GetByID(db.WithTenant(context.Background(), p.tenantID), p.fleetID)
	require.NoError(t, err)
	return f.Config
}

func (p sealedFleetPanel) update(t *testing.T, name string, keys, values []string) (int, string) {
	t.Helper()
	resp := postFormWithCookie(t, p.base+"/"+strconv.FormatInt(p.fleetID, 10), url.Values{
		"_csrf": {p.csrf}, "name": {name}, "config_key[]": keys, "config_value[]": values,
	}, p.cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func TestControlPanelFleet_create_should_not_store_a_plain_plugin_value(t *testing.T) {
	p := newSealedFleetPanel(t)

	raw := p.storedConfig(t)

	assert.NotContains(t, raw, fleetSealMarker)
}

func TestControlPanelFleet_list_should_not_show_a_plugin_config_value(t *testing.T) {
	p := newSealedFleetPanel(t)

	body := getWithCookie(t, p.base, p.cookie)

	assert.NotContains(t, body, fleetSealMarker)
	assert.Contains(t, body, "api_token")
}

func TestControlPanelFleet_edit_page_should_not_show_a_plugin_config_value(t *testing.T) {
	p := newSealedFleetPanel(t)

	body := getWithCookie(t, p.base+"/"+strconv.FormatInt(p.fleetID, 10), p.cookie)

	assert.NotContains(t, body, fleetSealMarker)
	assert.Contains(t, body, `value="api_token"`)
}

func TestControlPanelFleet_update_with_an_empty_value_should_keep_the_stored_value(t *testing.T) {
	p := newSealedFleetPanel(t)

	status, _ := p.update(t, "sealed", []string{"api_token", "region"}, []string{"", "SBG5"})

	require.Equal(t, http.StatusSeeOther, status)
	assert.Equal(t, map[string]string{"api_token": fleetSealMarker, "region": "SBG5"}, p.config(t))
}

func TestControlPanelFleet_update_with_a_new_value_should_seal_it(t *testing.T) {
	p := newSealedFleetPanel(t)
	next := fleetSealMarker + "-new"

	status, _ := p.update(t, "sealed", []string{"api_token", "region"}, []string{next, ""})

	require.Equal(t, http.StatusSeeOther, status)
	assert.NotContains(t, p.storedConfig(t), next)
	assert.Equal(t, next, p.config(t)["api_token"])
}

func TestControlPanelFleet_update_without_a_key_row_should_remove_the_key(t *testing.T) {
	p := newSealedFleetPanel(t)

	status, _ := p.update(t, "sealed", []string{"region"}, []string{""})

	require.Equal(t, http.StatusSeeOther, status)
	assert.Equal(t, map[string]string{"region": "GRA9"}, p.config(t))
}

func TestControlPanelFleet_update_with_no_key_should_clear_the_config(t *testing.T) {
	p := newSealedFleetPanel(t)

	status, _ := p.update(t, "sealed", []string{"", ""}, []string{"", ""})

	require.Equal(t, http.StatusSeeOther, status)
	assert.Empty(t, p.config(t))
}

func TestControlPanelFleet_failed_update_should_not_show_a_plugin_value(t *testing.T) {
	p := newSealedFleetPanel(t)
	typed := fleetSealMarker + "-typed"

	status, body := p.update(t, "", []string{"api_token", "region"}, []string{typed, ""})

	require.Equal(t, http.StatusUnprocessableEntity, status)
	assert.NotContains(t, body, typed)
	assert.NotContains(t, body, fleetSealMarker)
}

func TestControlPanelFleet_failed_create_should_not_show_a_plugin_value(t *testing.T) {
	p := newSealedFleetPanel(t)
	typed := fleetSealMarker + "-typed"

	resp := postFormWithCookie(t, p.base, url.Values{
		"_csrf": {p.csrf}, "name": {"sealed"}, "backend": {"plugin:x"},
		"config_key[]": {"api_token"}, "config_value[]": {typed},
	}, p.cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.NotContains(t, string(body), typed)
}
