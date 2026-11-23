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

func TestFleetWrite_should_return_422_for_a_long_name_or_backend(t *testing.T) {
	f := newFleetFixture(t)
	fid := f.fleet(f.projectA, "alpha")
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)
	long := strings.Repeat("n", 300)

	got := []int{
		f.do(tok, http.MethodPost, fleetsPath, `{"name":"`+long+`","backend":"agones","config":{"fleet_name":"gs"}}`).status,
		f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"plugin`+long+`","config":{}}`).status,
		f.do(tok, http.MethodPut, fleetsPath+"/"+id(fid), `{"name":"`+long+`"}`).status,
	}

	assert.Equal(t, []int{422, 422, 422}, got)
}

// huma copies a refused value, or the whole body, into a 422. A plugin
// config may hold a secret, so no fleet write error may carry it.
func TestFleetWrite_should_keep_the_plugin_config_out_of_a_validation_error(t *testing.T) {
	f := newFleetFixture(t)
	fid := f.scalar(`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		SELECT tenant_id, id, 'plug', 'plugin:demo', '{}' FROM projects WHERE id = $1 RETURNING id`, f.projectA)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)
	path := fleetsPath + "/" + id(fid)
	cfg := map[string]any{"api_key": "TOPSECRET"}

	create := f.do(tok, http.MethodPost, fleetsPath, `{"name":"p","backend":"plugin:demo","config":{"api_key":"TOPSECRET"},"extra":1}`)
	update := f.do(tok, http.MethodPut, path, `{"config":{"api_key":"TOPSECRET"},"extra":1}`)
	outputs := []string{
		create.raw, update.raw,
		f.do(tok, http.MethodPost, fleetsPath, `{"name":5,"backend":"plugin:demo","config":{"api_key":"TOPSECRET"}}`).raw,
		f.do(tok, http.MethodPut, path, `{"config":{"api_key":"TOPSECRET"}`).raw,
		f.rpcError(tok, "create_fleet", map[string]any{"name": "p", "backend": "plugin:demo", "config": cfg, "extra": 1}),
		f.rpcError(tok, "update_fleet", map[string]any{"fleet_id": fid, "config": cfg, "extra": 1}),
		resultText(f.call(tok, "create_fleet", map[string]any{"name": 5, "backend": "plugin:demo", "config": cfg})),
		resultText(f.call(tok, "update_fleet", map[string]any{"fleet_id": fid, "name": 5, "config": cfg})),
	}

	leaked := []int{}
	for i, out := range outputs {
		if strings.Contains(out, "TOPSECRET") {
			leaked = append(leaked, i)
		}
	}
	assert.Equal(t, []any{422, 422, []int{}}, []any{create.status, update.status, leaked}, strings.Join(outputs, "\n"))
}

// With Agones project isolation on, create_fleet and update_fleet refuse the
// agones keys that the server sets, over HTTP and MCP, and write nothing.
func TestFleetWrite_should_refuse_server_set_agones_keys_when_isolation_is_on(t *testing.T) {
	f := newFleetFixtureWithStore(t, nil, fakeBackend{}, true)
	fid := f.fleet(f.projectA, "alpha")
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)
	path := fleetsPath + "/" + id(fid)
	const msg = "Agones project isolation is on"
	ns := map[string]any{"fleet_name": "gs", "namespace": "MARKNS"}
	sel := map[string]any{"fleet_name": "gs", "selector.ggscale.dev/project": "MARKSEL"}

	httpOut := []response{
		f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"agones","config":{"fleet_name":"gs","namespace":"MARKNS"}}`),
		f.do(tok, http.MethodPost, fleetsPath, `{"name":"y","backend":"agones","config":{"fleet_name":"gs","selector.ggscale.dev/project":"MARKSEL"}}`),
		f.do(tok, http.MethodPut, path, `{"config":{"fleet_name":"gs","namespace":"MARKNS"}}`),
		f.do(tok, http.MethodPut, path, `{"config":{"fleet_name":"gs","selector.ggscale.dev/project":"MARKSEL"}}`),
	}
	mcpOut := []map[string]any{
		f.call(tok, "create_fleet", map[string]any{"name": "x", "backend": "agones", "config": ns}),
		f.call(tok, "create_fleet", map[string]any{"name": "y", "backend": "agones", "config": sel}),
		f.call(tok, "update_fleet", map[string]any{"fleet_id": fid, "config": ns}),
		f.call(tok, "update_fleet", map[string]any{"fleet_id": fid, "config": sel}),
	}

	got := []any{}
	for _, r := range httpOut {
		got = append(got, r.status, strings.Contains(r.detail(), msg), strings.Contains(r.raw, "MARK"))
	}
	for _, r := range mcpOut {
		got = append(got, isError(r), strings.Contains(resultDetail(r), msg), strings.Contains(resultText(r), "MARK"))
	}
	got = append(got, f.count(`SELECT count(*) FROM fleets`), f.count(`SELECT count(*) FROM fleets WHERE config::text LIKE '%MARK%'`))
	want := []any{}
	for range httpOut {
		want = append(want, 422, true, false)
	}
	for range mcpOut {
		want = append(want, true, true, false)
	}
	want = append(want, 1, 0)
	assert.Equal(t, want, got)
}

func TestFleetWrite_should_accept_other_agones_keys_and_a_rename_of_an_old_row_when_isolation_is_on(t *testing.T) {
	f := newFleetFixtureWithStore(t, nil, fakeBackend{}, true)
	old := f.scalar(`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		SELECT tenant_id, id, 'old', 'agones', '{"fleet_name":"gs","namespace":"legacy"}' FROM projects WHERE id = $1 RETURNING id`, f.projectA)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	created := f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"agones","config":{"fleet_name":"gs","selector.zone":"eu"}}`)
	renamed := f.do(tok, http.MethodPut, fleetsPath+"/"+id(old), `{"name":"old2"}`)
	plugin := f.do(tok, http.MethodPost, fleetsPath, `{"name":"p","backend":"plugin:demo","config":{"namespace":"n"}}`)

	assert.Equal(t, []int{200, 200, 200}, []int{created.status, renamed.status, plugin.status}, created.raw+renamed.raw+plugin.raw)
}

func TestFleetWrite_should_accept_a_namespace_when_isolation_is_off(t *testing.T) {
	f := newFleetFixture(t)
	tok := f.scopedToken(managementtoken.ScopeFleetWrite)

	got := f.do(tok, http.MethodPost, fleetsPath, `{"name":"x","backend":"agones","config":{"fleet_name":"gs","namespace":"n","selector.ggscale.dev/project":"1"}}`)

	assert.Equal(t, http.StatusOK, got.status, got.raw)
}
