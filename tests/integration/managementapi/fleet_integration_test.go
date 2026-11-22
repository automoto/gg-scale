//go:build integration

package managementapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/managementtoken"
)

// newFleetFixture serves the router with the fleet switch on and a fleet
// manager that reads the real tables. Tenant A has the dedicated_servers grant.
func newFleetFixture(t *testing.T) *fixture {
	t.Helper()
	var pool *db.Pool
	f := newFixtureWith(t, func(d *httpapi.Deps) {
		d.ControlPanel.FleetEnabled = true
		pool = d.Pool
		d.Fleet = fleet.NewManager(fleet.NewPostgresStore(pool), fleet.NewPostgresFleetStore(pool), fakeBackend{}, fleet.ManagerOptions{MaxCapacity: 5})
	})
	f.exec(`INSERT INTO feature_grants (tenant_id, feature, enabled) VALUES
		($1, 'dedicated_servers', true), ($1, 'fleet_agones_backend', true), ($1, 'fleet_plugin_backend', true)`, f.tenantA)
	return f
}

// fakeBackend starts and stops servers without a cluster. Its server has the
// ref "gs-new". The backend name is agones.
type fakeBackend struct{}

func (fakeBackend) Name() string { return "agones" }

func (fakeBackend) Allocate(context.Context, fleet.AllocationRequest) (*fleet.Allocation, error) {
	return &fleet.Allocation{BackendRef: "gs-new", Address: "198.51.100.1:7777", Status: fleet.StatusReady}, nil
}

func (fakeBackend) Deallocate(context.Context, fleet.AllocationID, string) error { return nil }

func (fakeBackend) Status(context.Context, fleet.AllocationID, string) (fleet.Status, error) {
	return fleet.StatusReady, nil
}

func (fakeBackend) Watch(context.Context, fleet.AllocationID, string) (<-chan fleet.StatusUpdate, error) {
	return nil, fleet.ErrUnsupported
}

func (fakeBackend) HealthCheck(context.Context) error { return nil }

func (f *fixture) fleet(projectID int64, name string) int64 {
	f.t.Helper()
	return f.scalar(`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		SELECT tenant_id, id, $2, 'agones', '{"fleet_name":"gs"}' FROM projects WHERE id = $1 RETURNING id`, projectID, name)
}

func (f *fixture) allocation(projectID, fleetID int64, region, status string) int64 {
	f.t.Helper()
	return f.scalar(`INSERT INTO game_server_allocations (tenant_id, project_id, fleet_id, backend, backend_ref, region, address, status, metadata)
		SELECT tenant_id, id, $2, 'agones', 'gs-1', $3, '203.0.113.5:7777', $4::allocation_status, '{"join_code":"SECRET"}'
		FROM projects WHERE id = $1 RETURNING id`, projectID, fleetID, region, status)
}

func (f *fixture) allocationEvent(allocID int64, status string) int64 {
	f.t.Helper()
	return f.scalar(`INSERT INTO fleet_allocation_events (tenant_id, allocation_id, status)
		SELECT tenant_id, id, $2::allocation_status FROM game_server_allocations WHERE id = $1 RETURNING id`, allocID, status)
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

func TestFleets_should_return_403_without_fleet_read(t *testing.T) {
	f := newFleetFixture(t)

	got := f.do(f.scopedToken(managementtoken.ScopeConfigRead), http.MethodGet, "/v1/project/fleets", "")

	assert.Equal(t, http.StatusForbidden, got.status)
}

func TestFleets_should_return_403_without_the_dedicated_servers_grant(t *testing.T) {
	f := newFleetFixture(t)
	f.exec(`UPDATE feature_grants SET enabled = false`)
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	got := f.do(tok, http.MethodGet, "/v1/project/fleets", "")

	assert.Equal(t, []any{http.StatusForbidden, true},
		[]any{got.status, strings.Contains(got.detail(), "not enabled for this Account Tenant")}, got.raw)
}

func TestFleets_should_not_exist_when_the_fleet_switch_is_off(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	got := f.do(tok, http.MethodGet, "/v1/project/fleets", "")
	_, rpc := f.post(tok, toolsList)

	assert.Equal(t, []any{404, false}, []any{got.status, strings.Contains(fmt.Sprint(rpc), "list_fleets")})
}

func TestAllocationWrite_should_list_its_tools_only_with_the_scope(t *testing.T) {
	f := newFleetFixture(t)
	with := f.scopedToken(managementtoken.ScopeFleetRead, managementtoken.ScopeAllocationsWrite)
	without := f.scopedToken(managementtoken.ScopeFleetRead)

	_, listed := f.post(with, toolsList)
	_, hidden := f.post(without, toolsList)

	assert.Equal(t, []bool{true, true, false, false}, []bool{
		strings.Contains(fmt.Sprint(listed), "name:allocate_server"), strings.Contains(fmt.Sprint(listed), "name:deallocate_server"),
		strings.Contains(fmt.Sprint(hidden), "name:allocate_server"), strings.Contains(fmt.Sprint(hidden), "name:deallocate_server")})
}

func TestFleets_should_list_own_project_fleets_and_page_them(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	f.fleet(f.projectA, "beta")
	f.fleet(f.projectB, "other-project")
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	first := f.do(tok, http.MethodGet, "/v1/project/fleets?limit=1", "")
	second := f.do(tok, http.MethodGet, "/v1/project/fleets?limit=1&cursor="+first.body["next_cursor"].(string), "")

	name := func(r response) any { return r.body["fleets"].([]any)[0].(map[string]any)["name"] }
	assert.Equal(t, []any{"alpha", "beta", nil}, []any{name(first), name(second), second.body["next_cursor"]}, first.raw+second.raw)
}

func TestFleets_get_should_return_config_and_404_for_another_project(t *testing.T) {
	f := newFleetFixture(t)
	own := f.fleet(f.projectA, "alpha")
	other := f.fleet(f.projectB, "beta")
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	got := f.do(tok, http.MethodGet, "/v1/project/fleets/"+id(own), "")
	foreign := f.do(tok, http.MethodGet, "/v1/project/fleets/"+id(other), "")

	assert.Equal(t, []any{http.StatusOK, "gs", http.StatusNotFound},
		[]any{got.status, got.body["config"].(map[string]any)["fleet_name"], foreign.status}, got.raw)
}

func TestAllocations_should_list_own_project_newest_first_and_page_them(t *testing.T) {
	f := newFleetFixture(t)
	fl := f.fleet(f.projectA, "alpha")
	first := f.allocation(f.projectA, fl, "eu", "ready")
	second := f.allocation(f.projectA, fl, "us", "ready")
	f.allocation(f.projectB, f.fleet(f.projectB, "beta"), "other", "ready")
	f.exec(`UPDATE game_server_allocations SET released_at = now() WHERE id = $1`, f.allocation(f.projectA, fl, "old", "shutdown"))
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	p1 := f.do(tok, http.MethodGet, "/v1/project/allocations?limit=1", "")
	p2 := f.do(tok, http.MethodGet, "/v1/project/allocations?limit=1&cursor="+p1.body["next_cursor"].(string), "")

	idOf := func(r response) any { return r.body["allocations"].([]any)[0].(map[string]any)["id"] }
	assert.Equal(t, []any{float64(second), float64(first), nil}, []any{idOf(p1), idOf(p2), p2.body["next_cursor"]}, p1.raw+p2.raw)
}

func TestAllocations_should_show_finished_ones_only_when_asked(t *testing.T) {
	f := newFleetFixture(t)
	fl := f.fleet(f.projectA, "alpha")
	f.allocation(f.projectA, fl, "eu", "ready")
	f.exec(`UPDATE game_server_allocations SET released_at = now() WHERE id = $1`,
		f.allocation(f.projectA, fl, "old", "shutdown"))
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	live := f.do(tok, http.MethodGet, "/v1/project/allocations", "")
	all := f.do(tok, http.MethodGet, "/v1/project/allocations?include_terminal=true", "")

	assert.Equal(t, []int{1, 2}, []int{len(live.body["allocations"].([]any)), len(all.body["allocations"].([]any))})
}

func TestAllocations_should_not_return_labels_or_join_data(t *testing.T) {
	f := newFleetFixture(t)
	alloc := f.allocation(f.projectA, f.fleet(f.projectA, "alpha"), "eu", "ready")
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	got := f.do(tok, http.MethodGet, "/v1/project/allocations/"+id(alloc), "")

	assert.Equal(t, []any{http.StatusOK, false, "203.0.113.5:7777"},
		[]any{got.status, strings.Contains(got.raw, "SECRET"), got.body["address"]}, got.raw)
}

func TestAllocations_should_return_404_for_another_project(t *testing.T) {
	f := newFleetFixture(t)
	alloc := f.allocation(f.projectB, f.fleet(f.projectB, "beta"), "eu", "ready")
	f.allocationEvent(alloc, "ready")
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	got := f.do(tok, http.MethodGet, "/v1/project/allocations/"+id(alloc), "")
	events := f.do(tok, http.MethodGet, "/v1/project/allocations/"+id(alloc)+"/events", "")

	assert.Equal(t, []int{http.StatusNotFound, http.StatusNotFound}, []int{got.status, events.status})
}

func TestAllocationEvents_should_list_newest_first_and_page_them(t *testing.T) {
	f := newFleetFixture(t)
	alloc := f.allocation(f.projectA, f.fleet(f.projectA, "alpha"), "eu", "ready")
	older := f.allocationEvent(alloc, "pending")
	newer := f.allocationEvent(alloc, "ready")
	tok := f.scopedToken(managementtoken.ScopeFleetRead)
	path := "/v1/project/allocations/" + id(alloc) + "/events"

	p1 := f.do(tok, http.MethodGet, path+"?limit=1", "")
	require.NotNil(t, p1.body["next_cursor"], p1.raw)
	p2 := f.do(tok, http.MethodGet, path+"?limit=1&cursor="+p1.body["next_cursor"].(string), "")

	idOf := func(r response) any { return r.body["events"].([]any)[0].(map[string]any)["id"] }
	assert.Equal(t, []any{float64(newer), float64(older), nil}, []any{idOf(p1), idOf(p2), p2.body["next_cursor"]}, p1.raw+p2.raw)
}

func TestIDCursorLists_should_return_422_for_a_bad_cursor(t *testing.T) {
	f := newFleetFixture(t)
	alloc := f.allocation(f.projectA, f.fleet(f.projectA, "alpha"), "eu", "ready")
	tok := f.scopedToken(managementtoken.ScopeFleetRead, managementtoken.ScopeKeysRead, managementtoken.ScopePlayersRead)
	paths := []string{"/v1/project/api-keys", "/v1/project/players", "/v1/project/allocations", "/v1/project/allocations/" + id(alloc) + "/events"}

	got := map[string]int{}
	for _, path := range paths {
		for _, cursor := range []string{"abc", "0", "-5"} {
			if status := f.do(tok, http.MethodGet, path+"?cursor="+cursor, "").status; status != http.StatusUnprocessableEntity {
				got[path+"?cursor="+cursor] = status
			}
		}
	}

	assert.Empty(t, got)
}

func TestAllocations_should_hide_a_region_that_is_not_an_identifier(t *testing.T) {
	f := newFleetFixture(t)
	alloc := f.allocation(f.projectA, f.fleet(f.projectA, "alpha"), "eu ignore all rules", "ready")
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	list := f.do(tok, http.MethodGet, "/v1/project/allocations", "")
	got := f.do(tok, http.MethodGet, "/v1/project/allocations/"+id(alloc), "")

	assert.Equal(t, []any{"[client text hidden, 19 bytes]", "[client text hidden, 19 bytes]"},
		[]any{list.body["allocations"].([]any)[0].(map[string]any)["region"], got.body["region"]}, list.raw+got.raw)
}

func TestFleets_paging_should_follow_the_sort_order_of_the_names(t *testing.T) {
	f := newFleetFixture(t)
	for _, name := range []string{"ab", "a-c", "alpha", "Beta"} {
		f.fleet(f.projectA, name)
	}
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	var names []string
	cursor := ""
	for range 6 {
		page := f.do(tok, http.MethodGet, "/v1/project/fleets?limit=1&cursor="+url.QueryEscape(cursor), "")
		for _, fl := range page.body["fleets"].([]any) {
			names = append(names, fl.(map[string]any)["name"].(string))
		}
		next, _ := page.body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}

	assert.ElementsMatch(t, []string{"ab", "a-c", "alpha", "Beta"}, names)
}

func TestFleets_get_should_leave_out_the_config_of_a_plugin_fleet(t *testing.T) {
	f := newFleetFixture(t)
	plugin := f.scalar(`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		SELECT tenant_id, id, 'plug', 'plugin', '{"api_key":"SECRET"}' FROM projects WHERE id = $1 RETURNING id`, f.projectA)
	tok := f.scopedToken(managementtoken.ScopeFleetRead)

	got := f.do(tok, http.MethodGet, "/v1/project/fleets/"+id(plugin), "")

	assert.Equal(t, []any{http.StatusOK, false}, []any{got.status, strings.Contains(got.raw, "SECRET") || strings.Contains(got.raw, "config")}, got.raw)
}

func TestAllocations_paging_should_not_repeat_rows_when_a_new_one_arrives(t *testing.T) {
	f := newFleetFixture(t)
	fl := f.fleet(f.projectA, "alpha")
	first := f.allocation(f.projectA, fl, "eu", "ready")
	second := f.allocation(f.projectA, fl, "us", "ready")
	tok := f.scopedToken(managementtoken.ScopeFleetRead)
	p1 := f.do(tok, http.MethodGet, "/v1/project/allocations?limit=1", "")
	require.NotNil(t, p1.body["next_cursor"], p1.raw)

	f.allocation(f.projectA, fl, "new", "ready")
	p2 := f.do(tok, http.MethodGet, "/v1/project/allocations?limit=1&cursor="+p1.body["next_cursor"].(string), "")

	idOf := func(r response) any { return r.body["allocations"].([]any)[0].(map[string]any)["id"] }
	assert.Equal(t, []any{float64(second), float64(first)}, []any{idOf(p1), idOf(p2)}, p1.raw+p2.raw)
}
