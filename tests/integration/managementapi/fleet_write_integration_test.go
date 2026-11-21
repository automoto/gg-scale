//go:build integration

package managementapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/managementtoken"
)

const fleetsPath = "/v1/project/fleets"

func TestFleetWrite_should_return_403_without_the_fleet_write_scope(t *testing.T) {
	f := newFleetFixture(t)
	fid := f.fleet(f.projectA, "alpha")
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	create := f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"agones","config":{"fleet_name":"gs"}}`)
	update := f.do(tok, http.MethodPut, fleetsPath+"/"+id(fid), `{"name":"y"}`)
	del := f.do(tok, http.MethodDelete, fleetsPath+"/"+id(fid), "")
	restore := f.do(tok, http.MethodPost, fleetsPath+"/"+id(fid)+"/restore", "")

	assert.Equal(t, []int{403, 403, 403, 403}, []int{create.status, update.status, del.status, restore.status})
}

func TestFleetWrite_should_return_403_without_the_dedicated_servers_grant(t *testing.T) {
	f := newFleetFixture(t)
	f.exec(`UPDATE feature_grants SET enabled = false`)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	got := f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"agones","config":{"fleet_name":"gs"}}`)

	assert.Equal(t, http.StatusForbidden, got.status, got.raw)
}

func TestFleetWrite_should_create_update_and_audit_with_the_token(t *testing.T) {
	f := newFleetFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{managementtoken.ScopeFleetWrite}, time.Hour)

	created := f.do(tok, http.MethodPost, fleetsPath, `{"name":"alpha","backend":"agones","config":{"fleet_name":"gs","selector.zone":"eu"}}`)
	fleetID := int64(created.body["id"].(float64))
	updated := f.do(tok, http.MethodPut, fleetsPath+"/"+id(fleetID), `{"name":"alpha2","config":{"fleet_name":"gs2"}}`)

	service, createAudit := f.audit("fleet.template.create", fleetID)
	_, updateAudit := f.audit("fleet.template.update", fleetID)
	assert.Equal(t,
		[]any{200, "gs", 200, "alpha2", "gs2", 1, 1, "management_token:" + id(tokenID)},
		[]any{created.status, created.body["config"].(map[string]any)["fleet_name"], updated.status, updated.body["name"],
			updated.body["config"].(map[string]any)["fleet_name"], createAudit, updateAudit, service},
		created.raw+updated.raw)
}

func TestFleetWrite_should_not_echo_plugin_config_or_put_its_values_in_the_audit_row(t *testing.T) {
	f := newFleetFixture(t)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	created := f.do(tok, http.MethodPost, fleetsPath, `{"name":"p","backend":"plugin:demo","config":{"api_key":"TOPSECRET"}}`)
	fleetID := int64(created.body["id"].(float64))
	updated := f.do(tok, http.MethodPut, fleetsPath+"/"+id(fleetID), `{"config":{"api_key":"TOPSECRET2"}}`)

	var payloads string
	err := f.owner.QueryRow(t.Context(),
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM platform_audit_log WHERE action LIKE 'fleet.template.%' AND target = $1`, id(fleetID)).Scan(&payloads)
	assert.NoError(t, err)
	assert.Equal(t, []any{200, 200, false, false, false, false},
		[]any{created.status, updated.status,
			strings.Contains(created.raw, "TOPSECRET"), strings.Contains(updated.raw, "TOPSECRET"), strings.Contains(payloads, "TOPSECRET"),
			updated.body["config"] != nil}, created.raw+updated.raw)
}

func TestFleetWrite_should_return_422_for_a_bad_config_without_echoing_it(t *testing.T) {
	f := newFleetFixture(t)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	missing := f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"agones","config":{"namespace":"SECRETNS"}}`)
	backend := f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"nope","config":{}}`)

	assert.Equal(t, []any{422, 422, false}, []any{missing.status, backend.status, strings.Contains(missing.raw, "SECRETNS")}, missing.raw)
}

func TestFleetWrite_create_should_return_409_for_a_name_in_use(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	got := f.do(tok, http.MethodPost, fleetsPath, `{"name":"alpha","backend":"agones","config":{"fleet_name":"gs"}}`)

	assert.Equal(t, http.StatusConflict, got.status, got.raw)
}

func TestFleetWrite_should_delete_then_restore_the_same_fleet(t *testing.T) {
	f := newFleetFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{managementtoken.ScopeFleetWrite, managementtoken.ScopeFleetRead}, time.Hour)
	created := f.do(tok, http.MethodPost, fleetsPath, `{"name":"alpha","backend":"agones","config":{"fleet_name":"gs"}}`)
	fleetID := int64(created.body["id"].(float64))
	path := fleetsPath + "/" + id(fleetID)

	del := f.do(tok, http.MethodDelete, path, "")
	whileDeleted := f.do(tok, http.MethodGet, path, "")
	updateDeleted := f.do(tok, http.MethodPut, path, `{"name":"zzz"}`)
	deleteAgain := f.do(tok, http.MethodDelete, path, "")
	restored := f.do(tok, http.MethodPost, path+"/restore", "")
	again := f.do(tok, http.MethodGet, path, "")

	service, delAudit := f.audit("fleet.template.delete", fleetID)
	_, restoreAudit := f.audit("fleet.template.restore", fleetID)
	assert.Equal(t,
		[]any{200, 404, 404, 404, 200, 200, "alpha", "agones", "gs", 1, 1, "management_token:" + id(tokenID)},
		[]any{del.status, whileDeleted.status, updateDeleted.status, deleteAgain.status, restored.status, again.status,
			again.body["name"], again.body["backend"], again.body["config"].(map[string]any)["fleet_name"], delAudit, restoreAudit, service},
		del.raw+restored.raw)
}

func TestFleetWrite_restore_should_return_409_when_a_live_fleet_has_the_name(t *testing.T) {
	f := newFleetFixture(t)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)
	old := f.fleet(f.projectA, "alpha")
	f.exec(`UPDATE fleets SET deleted_at = now() WHERE id = $1`, old)
	f.fleet(f.projectA, "alpha")

	got := f.do(tok, http.MethodPost, fleetsPath+"/"+id(old)+"/restore", "")

	assert.Equal(t, http.StatusConflict, got.status, got.raw)
}

func TestFleetWrite_should_return_404_for_a_fleet_of_another_project_or_a_live_restore(t *testing.T) {
	f := newFleetFixture(t)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)
	other := f.fleet(f.projectB, "beta")
	live := f.fleet(f.projectA, "alpha")

	del := f.do(tok, http.MethodDelete, fleetsPath+"/"+id(other), "")
	update := f.do(tok, http.MethodPut, fleetsPath+"/"+id(other), `{"name":"q"}`)
	restoreLive := f.do(tok, http.MethodPost, fleetsPath+"/"+id(live)+"/restore", "")

	assert.Equal(t, []int{404, 404, 404}, []int{del.status, update.status, restoreLive.status})
}

func TestFleetWrite_should_return_403_without_the_backend_grant(t *testing.T) {
	f := newFleetFixture(t)
	f.exec(`UPDATE feature_grants SET enabled = false WHERE feature = 'fleet_agones_backend'`)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	got := f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"agones","config":{"fleet_name":"gs"}}`)

	assert.Equal(t, http.StatusForbidden, got.status, got.raw)
}
