//go:build integration

package managementapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/managementtoken"
)

// readScopes are all read scopes, so a token with them can use every read.
var readScopes = []string{
	managementtoken.ScopeConfigRead, managementtoken.ScopeLeaderboardsRead,
	managementtoken.ScopeKeysRead, managementtoken.ScopeMatchmakingRead,
}

func TestReadScopes_leaderboards_read_token_should_list_only_leaderboard_read_tools(t *testing.T) {
	f := newFixture(t)

	_, out := f.post(f.scopedToken(managementtoken.ScopeLeaderboardsRead), toolsList)

	assert.ElementsMatch(t, []string{"project_health_check", "list_leaderboards", "get_leaderboard", "list_revisions", "get_token"}, toolNames(out))
}

func TestReadScopes_should_return_403_when_keys_read_is_missing(t *testing.T) {
	f := newFixture(t)

	got := f.get(f.scopedToken(managementtoken.ScopeLeaderboardsRead), "/v1/project/api-keys")

	assert.Equal(t, http.StatusForbidden, got)
}

func TestReadScopes_should_return_403_for_remote_config_revisions_without_config_read(t *testing.T) {
	f := newFixture(t)

	got := f.get(f.scopedToken(managementtoken.ScopeLeaderboardsRead), "/v1/project/revisions?resource=remote_config")

	assert.Equal(t, http.StatusForbidden, got)
}

func TestReadScopes_health_check_should_leave_out_sections_without_their_read_scope(t *testing.T) {
	f := newFixture(t)

	res := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")

	checks, _ := res.body["checks"].(map[string]any)
	got := []bool{}
	for _, key := range []string{"api_keys", "cors_allowed_origins", "steam_sign_in", "remote_config_bytes", "feature_grants"} {
		_, ok := checks[key]
		got = append(got, ok)
	}
	assert.Equal(t, []bool{false, false, false, false, true}, got, res.raw)
}

func TestReadScopes_health_check_should_not_report_key_problems_without_keys_read(t *testing.T) {
	f := newFixture(t)

	res := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")

	assert.Equal(t, []any{}, res.body["problems"], res.raw)
}

func TestReadScopes_health_check_should_show_every_section_with_all_read_scopes(t *testing.T) {
	f := newFixture(t)

	res := f.do(f.scopedToken(readScopes...), http.MethodGet, "/v1/project/health", "")

	checks, _ := res.body["checks"].(map[string]any)
	got := []bool{}
	for _, key := range []string{"api_keys", "cors_allowed_origins", "steam_sign_in", "remote_config_bytes"} {
		_, ok := checks[key]
		got = append(got, ok)
	}
	assert.Equal(t, []bool{true, true, true, true}, got, res.raw)
}
