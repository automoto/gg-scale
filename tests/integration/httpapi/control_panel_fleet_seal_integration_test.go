//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	c         *cluster
	mgr       *fleet.Manager
	cookie    *http.Cookie
	csrf      string
	tenantID  int64
	projectID int64
	base      string
	fleetID   int64
}

func newSealedFleetPanel(t *testing.T) sealedFleetPanel {
	t.Helper()
	return newSealedFleetPanelWith(t, newStubBackend("plugin:x"))
}

func newSealedFleetPanelWith(t *testing.T, backend fleet.Backend) sealedFleetPanel {
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
	srv, mgr := newControlPanelFleetServerWithCipher(t, c, backend, nil, cipher)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	p := sealedFleetPanel{
		c: c, mgr: mgr, cookie: cookie, csrf: csrf, tenantID: tenantID, projectID: projectID,
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

// breakStoredToken replaces the stored api_token with a value sealed by
// another key, as after a lost or changed credential key.
func (p sealedFleetPanel) breakStoredToken(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	other, err := secretseal.NewCipherKeyring(key)
	require.NoError(t, err)
	frame, err := other.Encrypt([]byte(fleetSealMarker))
	require.NoError(t, err)
	_, err = p.c.bootstrapPool.Exec(context.Background(),
		`UPDATE fleets SET config = jsonb_set(config, '{api_token}', to_jsonb($2::text)) WHERE id = $1`,
		p.fleetID, "ggseal1:"+base64.StdEncoding.EncodeToString(frame))
	require.NoError(t, err)
}

func TestControlPanelFleet_update_should_refuse_to_keep_a_value_that_does_not_open(t *testing.T) {
	p := newSealedFleetPanel(t)
	p.breakStoredToken(t)
	before := p.storedConfig(t)

	status, body := p.update(t, "sealed", []string{"api_token", "region"}, []string{"", "SBG5"})

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true, before},
		[]any{status, strings.Contains(body, "cannot be opened"), p.storedConfig(t)})
}

func TestControlPanelFleet_update_should_accept_new_values_when_a_value_does_not_open(t *testing.T) {
	p := newSealedFleetPanel(t)
	p.breakStoredToken(t)

	status, _ := p.update(t, "sealed", []string{"api_token", "region"}, []string{"fresh", "SBG5"})

	require.Equal(t, http.StatusSeeOther, status)
	assert.Equal(t, map[string]string{"api_token": "fresh", "region": "SBG5"}, p.config(t))
}

func TestControlPanelFleet_list_should_render_when_a_value_does_not_open(t *testing.T) {
	p := newSealedFleetPanel(t)
	p.breakStoredToken(t)

	status := getStatusWithCookie(t, p.base, p.cookie)
	body := getWithCookie(t, p.base, p.cookie)

	assert.Equal(t, []any{http.StatusOK, true}, []any{status, strings.Contains(body, "api_token")})
}

func TestControlPanelFleet_update_should_refuse_a_key_in_two_rows(t *testing.T) {
	p := newSealedFleetPanel(t)

	status, _ := p.update(t, "sealed", []string{"api_token", "api_token"}, []string{"next", ""})

	assert.Equal(t, []any{http.StatusUnprocessableEntity, fleetSealMarker}, []any{status, p.config(t)["api_token"]})
}

func TestControlPanelFleet_failed_update_should_ask_for_the_values_again(t *testing.T) {
	p := newSealedFleetPanel(t)

	status, body := p.update(t, "", []string{"api_token", "region"}, []string{"typed", ""})

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true}, []any{status, strings.Contains(body, "Enter each value again")})
}

// cleanerStub is stubBackend with resolution cleanup, so a deallocate reads
// the fleet config.
type cleanerStub struct {
	*stubBackend
	called bool
}

func (b *cleanerStub) CleanupResolution(context.Context, int64, string, map[string]string) error {
	b.called = true
	return nil
}

func TestControlPanelFleet_deallocate_should_answer_422_when_a_plugin_value_does_not_open(t *testing.T) {
	backend := &cleanerStub{stubBackend: newStubBackend("plugin:x")}
	p := newSealedFleetPanelWith(t, backend)
	p.breakStoredToken(t)
	var allocID int64
	require.NoError(t, p.c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO game_server_allocations (tenant_id, project_id, fleet_id, backend, backend_ref, region, address, status, metadata)
		 VALUES ($1, $2, $3, 'plugin:x', 'gs-1', 'eu', '203.0.113.5:7777', 'ready', '{"ggscale.dev/resolution-id":"mm_sealed"}')
		 RETURNING id`, p.tenantID, p.projectID, p.fleetID).Scan(&allocID))
	deallocURL := strings.TrimSuffix(p.base, "/fleets") + "/allocations/" + strconv.FormatInt(allocID, 10) + "/deallocate"

	resp := postFormWithCookie(t, deallocURL, url.Values{"_csrf": {p.csrf}, "confirm_id": {strconv.FormatInt(allocID, 10)}}, p.cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true, false},
		[]any{resp.StatusCode, strings.Contains(string(body), "cannot be opened"), backend.called})
}
