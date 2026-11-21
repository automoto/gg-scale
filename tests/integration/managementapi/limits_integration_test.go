//go:build integration

package managementapi_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/managementtoken"
)

const (
	limitsPath        = "/v1/project/limits"
	inviteLimitsPath  = "/v1/project/limits/invites"
	storageLimitsPath = "/v1/project/limits/storage"
)

func TestLimits_get_should_return_defaults_and_usage_for_a_new_project(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.board(f.projectA, "one")

	// Act
	got := f.do(f.scopedToken(managementtoken.ScopeLimitsRead), http.MethodGet, limitsPath, "")

	// Assert
	invites := got.body["invites"].(map[string]any)
	storage := got.body["storage"].(map[string]any)
	usage := got.body["usage"].(map[string]any)
	assert.Equal(t, []any{200, float64(0), float64(0), float64(1), float64(1), float64(1)},
		[]any{got.status, invites["inviter_per_hour"], storage["max_value_mb"], storage["effective_max_value_mb"], storage["tenant_max_value_mb"],
			usage["leaderboards"].(map[string]any)["current"]}, got.raw)
}

func TestLimits_should_return_403_without_the_matching_scope(t *testing.T) {
	// Arrange
	f := newFixture(t)
	readOnly := f.scopedToken(managementtoken.ScopeLimitsRead)
	writeOnly := f.scopedToken(managementtoken.ScopeLimitsWrite)

	// Act
	got := []int{
		f.do(writeOnly, http.MethodGet, limitsPath, "").status,
		f.do(readOnly, http.MethodPut, inviteLimitsPath, `{"inviter_per_hour":1,"domain_per_day":1}`).status,
		f.do(readOnly, http.MethodPut, storageLimitsPath, `{"max_value_mb":0.5}`).status,
	}

	// Assert
	assert.Equal(t, []int{403, 403, 403}, got)
}

func TestLimits_should_return_403_when_the_creator_lost_the_project_pair(t *testing.T) {
	// Arrange
	f := newFixture(t)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{managementtoken.ScopeLimitsRead, managementtoken.ScopeLimitsWrite}, time.Hour)
	f.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = 'project' AND v3 = 'manage'`)
	require.NoError(t, f.authz.ReloadPolicy())

	// Act
	got := []int{
		f.do(tok, http.MethodGet, limitsPath, "").status,
		f.do(tok, http.MethodPut, storageLimitsPath, `{"max_value_mb":0.5}`).status,
	}

	// Assert
	assert.Equal(t, []int{403, 403}, got)
}

func TestLimits_should_return_422_with_the_dashboard_message_for_bad_values(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopeLimitsWrite)
	tests := []struct{ name, path, body, want string }{
		{"negative invite", inviteLimitsPath, `{"inviter_per_hour":-1,"domain_per_day":0}`, "Values must be finite non-negative numbers."},
		{"invite above cap", inviteLimitsPath, `{"inviter_per_hour":1000000,"domain_per_day":0}`, "Per Game Project invite quota can't exceed the Account Tenant cap."},
		{"negative storage", storageLimitsPath, `{"max_value_mb":-1}`, "Storage limit must be a non-negative number (MB)."},
		{"storage above tenant limit", storageLimitsPath, `{"max_value_mb":2}`, "Per Game Project storage limit can't exceed the Account Tenant limit."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := f.do(tok, http.MethodPut, tc.path, tc.body)

			// Assert
			assert.Equal(t, []any{422, tc.want}, []any{got.status, got.detail()}, got.raw)
		})
	}
}

func TestLimits_invite_write_should_save_audit_the_token_and_return_the_limits(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{managementtoken.ScopeLimitsWrite}, time.Hour)

	// Act
	got := f.do(tok, http.MethodPut, inviteLimitsPath, `{"inviter_per_hour":5,"domain_per_day":7}`)

	// Assert
	service, audited := f.audit("control_panel.rate_limit.set", f.projectA)
	stored := f.count(`SELECT count(*) FROM rate_limit_overrides WHERE tenant_id = $1 AND project_id = $2`, f.tenantA, f.projectA)
	invites := got.body["invites"].(map[string]any)
	assert.Equal(t, []any{200, float64(5), float64(7), 2, 1, "management_token:" + strconv.FormatInt(tokenID, 10)},
		[]any{got.status, invites["inviter_per_hour"], invites["domain_per_day"], stored, audited, service}, got.raw)
}

func TestLimits_storage_write_should_save_audit_the_token_and_return_the_limits(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{managementtoken.ScopeLimitsWrite}, time.Hour)

	// Act
	got := f.do(tok, http.MethodPut, storageLimitsPath, `{"max_value_mb":0.5}`)

	// Assert
	service, audited := f.audit("control_panel.storage_limit.set", f.projectA)
	stored := f.scalar(`SELECT max_value_bytes FROM storage_limits WHERE tenant_id = $1 AND project_id = $2`, f.tenantA, f.projectA)
	storage := got.body["storage"].(map[string]any)
	assert.Equal(t, []any{200, int64(524288), float64(0.5), 1, "management_token:" + strconv.FormatInt(tokenID, 10)},
		[]any{got.status, stored, storage["max_value_mb"], audited, service}, got.raw)
}

func TestLimits_storage_write_of_zero_should_clear_the_override(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopeLimitsWrite)
	f.do(tok, http.MethodPut, storageLimitsPath, `{"max_value_mb":0.5}`)

	// Act
	got := f.do(tok, http.MethodPut, storageLimitsPath, `{"max_value_mb":0}`)

	// Assert
	assert.Equal(t, []any{200, 0}, []any{got.status, f.count(`SELECT count(*) FROM storage_limits WHERE project_id = $1`, f.projectA)}, got.raw)
}

func TestLimits_storage_write_should_roll_back_when_the_audit_row_fails(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopeLimitsWrite)
	f.exec(`CREATE FUNCTION fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit refused'; END $$`)
	f.exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON platform_audit_log FOR EACH ROW EXECUTE FUNCTION fail_audit()`)

	// Act
	got := f.do(tok, http.MethodPut, storageLimitsPath, `{"max_value_mb":0.5}`)

	// Assert
	assert.Equal(t, []any{500, 0}, []any{got.status, f.count(`SELECT count(*) FROM storage_limits WHERE project_id = $1`, f.projectA)}, got.raw)
}

func TestLimits_health_check_should_leave_out_quotas_without_limits_read(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act
	without := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")
	with := f.do(f.scopedToken(managementtoken.ScopeLimitsRead), http.MethodGet, "/v1/project/health", "")

	// Assert
	_, hasWithout := without.body["checks"].(map[string]any)["quotas"]
	_, hasWith := with.body["checks"].(map[string]any)["quotas"]
	assert.Equal(t, []bool{false, true}, []bool{hasWithout, hasWith}, without.raw)
}

func TestLimits_health_check_should_keep_the_leaderboard_problem_with_leaderboards_read_only(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.exec(`INSERT INTO tenant_quota_overrides (tenant_id, axis, "limit") VALUES ($1, 'leaderboards', 1)`, f.tenantA)
	f.board(f.projectA, "one")
	const problem = "The Game Project is at its leaderboard limit (1). New leaderboards are refused."

	// Act
	boards := f.do(f.scopedToken(managementtoken.ScopeLeaderboardsRead), http.MethodGet, "/v1/project/health", "")
	none := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")

	// Assert
	assert.Contains(t, boards.body["problems"], problem, boards.raw)
	assert.NotContains(t, none.body["problems"], problem, none.raw)
}
