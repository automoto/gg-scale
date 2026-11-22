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

const allocationsPath = "/v1/project/allocations"

func TestAllocationWrite_should_return_403_without_the_allocations_write_scope(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	alloc := f.allocation(f.projectA, f.scalar(`SELECT id FROM fleets LIMIT 1`), "eu", "ready")
	tok := f.scopedToken(managementtoken.ScopeFleetRead, managementtoken.ScopeFleetWrite)

	start := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"alpha","region":"eu"}`)
	stop := f.do(tok, http.MethodPost, allocationsPath+"/"+id(alloc)+"/deallocate", "")

	assert.Equal(t, []int{403, 403}, []int{start.status, stop.status}, start.raw+stop.raw)
}

func TestAllocationWrite_should_return_403_without_the_dedicated_servers_grant(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	f.exec(`UPDATE feature_grants SET enabled = false WHERE feature = 'dedicated_servers'`)
	tok := f.scopedToken(managementtoken.ScopeAllocationsWrite)

	got := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"alpha","region":"eu"}`)

	assert.Equal(t, http.StatusForbidden, got.status, got.raw)
}

func TestAllocationWrite_should_return_403_when_the_creator_lacks_the_allocation_pairs(t *testing.T) {
	f := newFleetFixture(t)
	fid := f.fleet(f.projectA, "alpha")
	alloc := f.allocation(f.projectA, fid, "eu", "ready")
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{managementtoken.ScopeAllocationsWrite}, time.Hour)

	start := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"alpha","region":"eu"}`)
	stop := f.do(tok, http.MethodPost, allocationsPath+"/"+id(alloc)+"/deallocate", "")

	assert.Equal(t, []int{403, 403}, []int{start.status, stop.status}, start.raw+stop.raw)
}

func TestAllocationWrite_should_return_403_when_the_backend_grant_is_off(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	f.exec(`UPDATE feature_grants SET enabled = false WHERE feature = 'fleet_agones_backend'`)
	tok := f.scopedToken(managementtoken.ScopeAllocationsWrite)

	got := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"alpha","region":"eu"}`)

	assert.Equal(t, http.StatusForbidden, got.status, got.raw)
}

func TestAllocationWrite_should_allocate_and_audit_with_the_token(t *testing.T) {
	f := newFleetFixture(t)
	fid := f.fleet(f.projectA, "alpha")
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{managementtoken.ScopeAllocationsWrite}, time.Hour)

	got := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"alpha","region":"eu","game_mode":"duel","capacity":2}`)

	allocID := int64(got.body["id"].(float64))
	service, count := f.audit("fleet.allocate.manual", allocID)
	assert.Equal(t,
		[]any{200, float64(fid), "agones", "gs-new", "eu", "198.51.100.1:7777", "ready", 1, "management_token:" + id(tokenID), false},
		[]any{got.status, got.body["fleet_id"], got.body["backend"], got.body["backend_ref"], got.body["region"], got.body["address"],
			got.body["status"], count, service, strings.Contains(got.raw, "labels")}, got.raw)
}

func TestAllocationWrite_should_return_404_for_a_deleted_or_foreign_or_unknown_fleet(t *testing.T) {
	f := newFleetFixture(t)
	gone := f.fleet(f.projectA, "gone")
	f.exec(`UPDATE fleets SET deleted_at = now() WHERE id = $1`, gone)
	f.fleet(f.projectB, "beta")
	tok := f.scopedToken(managementtoken.ScopeAllocationsWrite)

	deleted := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"gone","region":"eu"}`)
	foreign := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"beta","region":"eu"}`)
	unknown := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"nope","region":"eu"}`)

	assert.Equal(t, []int{404, 404, 404}, []int{deleted.status, foreign.status, unknown.status}, deleted.raw+foreign.raw+unknown.raw)
}

func TestAllocationWrite_should_return_422_for_a_missing_fleet_or_region(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	tok := f.scopedToken(managementtoken.ScopeAllocationsWrite)

	noFleet := f.do(tok, http.MethodPost, allocationsPath, `{"region":"eu"}`)
	noRegion := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"alpha","region":" "}`)

	assert.Equal(t, []int{422, 422}, []int{noFleet.status, noRegion.status}, noFleet.raw+noRegion.raw)
}

func TestAllocationWrite_should_not_take_labels_or_join_data(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	tok := f.scopedToken(managementtoken.ScopeAllocationsWrite)

	got := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"alpha","region":"eu","labels":{"a":"b"},"join_code":"X"}`)

	assert.Equal(t, http.StatusUnprocessableEntity, got.status, got.raw)
}

func TestAllocationWrite_should_deallocate_and_audit_with_the_token(t *testing.T) {
	f := newFleetFixture(t)
	fid := f.fleet(f.projectA, "alpha")
	alloc := f.allocation(f.projectA, fid, "eu", "ready")
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{managementtoken.ScopeAllocationsWrite}, time.Hour)

	got := f.do(tok, http.MethodPost, allocationsPath+"/"+id(alloc)+"/deallocate", "")
	again := f.do(tok, http.MethodPost, allocationsPath+"/"+id(alloc)+"/deallocate", "")

	service, count := f.audit("fleet.deallocate.manual", alloc)
	assert.Equal(t, []any{200, "shutdown", 200, "shutdown", 1, "management_token:" + id(tokenID)},
		[]any{got.status, got.body["status"], again.status, again.body["status"], count, service}, got.raw+again.raw)
}

func TestAllocationWrite_deallocate_should_return_404_for_another_project(t *testing.T) {
	f := newFleetFixture(t)
	other := f.allocation(f.projectB, f.fleet(f.projectB, "beta"), "eu", "ready")
	tok := f.scopedToken(managementtoken.ScopeAllocationsWrite)

	got := f.do(tok, http.MethodPost, allocationsPath+"/"+id(other)+"/deallocate", "")

	assert.Equal(t, []any{404, "ready"}, []any{got.status, f.text(`SELECT status::text FROM game_server_allocations WHERE id = $1`, other)}, got.raw)
}

func (f *fixture) text(sql string, args ...any) string {
	f.t.Helper()
	var s string
	assert.NoError(f.t, f.owner.QueryRow(f.t.Context(), sql, args...).Scan(&s))
	return s
}

func TestAllocationWrite_should_return_422_for_input_over_the_limits(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	tok := f.scopedToken(managementtoken.ScopeAllocationsWrite)
	long := strings.Repeat("x", 257)
	cases := map[string]string{
		"long_region":    `{"fleet":"alpha","region":"` + long + `"}`,
		"long_game_mode": `{"fleet":"alpha","region":"eu","game_mode":"` + long + `"}`,
		"nul_region":     `{"fleet":"alpha","region":"e\u0000u"}`,
		"big_capacity":   `{"fleet":"alpha","region":"eu","capacity":6}`,
		"zero_capacity":  `{"fleet":"alpha","region":"eu","capacity":0}`,
		"neg_capacity":   `{"fleet":"alpha","region":"eu","capacity":-1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, http.MethodPost, allocationsPath, body)

			assert.Equal(t, http.StatusUnprocessableEntity, got.status, got.raw)
		})
	}
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM game_server_allocations`))
}

func TestAllocationWrite_should_return_409_when_the_fleet_backend_does_not_match(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(f.projectA, "alpha")
	f.exec(`UPDATE fleets SET backend = 'plugin:demo'`)
	tok := f.scopedToken(managementtoken.ScopeAllocationsWrite)

	got := f.do(tok, http.MethodPost, allocationsPath, `{"fleet":"alpha","region":"eu"}`)

	assert.Equal(t, http.StatusConflict, got.status, got.raw)
}
