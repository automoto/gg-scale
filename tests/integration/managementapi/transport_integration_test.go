//go:build integration

package managementapi_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/cache/memory"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/ratelimit"
)

// countingLimiter records each per-token bucket key that the token
// middleware uses.
type countingLimiter struct {
	ratelimit.Limiter
	mu   sync.Mutex
	keys map[string]int
}

func (l *countingLimiter) Allow(ctx context.Context, key string, rate, burst float64) (ratelimit.Decision, error) {
	if strings.HasPrefix(key, "ratelimit:management_token:") {
		l.mu.Lock()
		l.keys[key]++
		l.mu.Unlock()
	}
	return l.Limiter.Allow(ctx, key, rate, burst)
}

const toolsList = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// get sends a direct HTTP GET and returns the status.
func (f *fixture) get(token, path string) int {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
	require.NoError(f.t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func toolNames(out map[string]any) []string {
	names := []string{}
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	return names
}

var readTools = []string{
	"project_health_check", "matchmaking_ticket_trace", "get_remote_config",
	"list_leaderboards", "get_leaderboard", "list_api_keys", "list_revisions", "get_token",
	"list_players", "get_player", "get_project_limits", "get_matchmaking_queue", "get_steam_auth",
}

func TestTransport_tool_call_should_take_one_unit_from_each_token_bucket(t *testing.T) {
	// Arrange
	limiter := &countingLimiter{Limiter: ratelimit.NewCacheLimiter(memory.New()), keys: map[string]int{}}
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.Limiter = limiter })
	tok, id := f.token(f.tenantA, f.projectA, f.ownerID, readScopes, time.Hour)

	// Act
	res := f.call(tok, "project_health_check", nil)

	// Assert
	require.NotNil(t, structured(res)["checks"], resultText(res))
	n := strconv.FormatInt(id, 10)
	assert.Equal(t, map[string]int{
		"ratelimit:management_token:mcp:" + n: 1,
		"ratelimit:management_token:api:" + n: 1,
	}, limiter.keys)
}

func oneRequestBucket(d *httpapi.Deps) {
	d.ManagementTokenRatePerSecond, d.ManagementTokenBurst = 0.001, 1
}

func TestTransport_token_at_its_api_limit_should_still_list_tools(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, oneRequestBucket)
	tok := f.ownerToken()
	f.get(tok, "/v1/project/health")

	// Act
	direct := f.get(tok, "/v1/project/health")
	resp, _ := f.post(tok, toolsList)

	// Assert
	assert.Equal(t, []int{http.StatusTooManyRequests, http.StatusOK}, []int{direct, resp.StatusCode})
}

func TestTransport_token_at_its_mcp_limit_should_still_get_a_direct_call(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, oneRequestBucket)
	tok := f.ownerToken()
	f.post(tok, toolsList)

	// Act
	resp, _ := f.post(tok, toolsList)
	direct := f.get(tok, "/v1/project/health")

	// Assert
	assert.Equal(t, []int{http.StatusTooManyRequests, http.StatusOK}, []int{resp.StatusCode, direct})
}

func TestTransport_tools_list_should_have_exactly_the_table_tools_for_all_scopes(t *testing.T) {
	f := newFixture(t)
	scopes := []string{}
	for _, s := range managementtoken.Scopes {
		scopes = append(scopes, s.Scope)
	}

	_, out := f.post(f.scopedToken(scopes...), toolsList)

	assert.ElementsMatch(t, append([]string{
		"set_remote_config", "rollback_remote_config", "create_leaderboard", "update_leaderboard",
		"rollback_leaderboard", "delete_leaderboard", "restore_leaderboard", "create_api_key", "update_api_key_label", "update_api_key_features",
		"set_allowed_origins", "set_player_disabled", "request_player_delete", "cancel_player_delete",
		"set_project_invite_limit", "set_project_storage_limit", "revoke_api_key", "create_secret_api_key", "set_steam_auth", "clear_steam_auth",
	}, readTools...), toolNames(out))
}

func TestTransport_tools_list_should_hide_tools_whose_scope_the_token_lacks(t *testing.T) {
	f := newFixture(t)

	_, out := f.post(f.scopedToken(managementtoken.ScopeConfigWrite), toolsList)

	assert.ElementsMatch(t, []string{"set_remote_config", "rollback_remote_config", "project_health_check", "get_token"}, toolNames(out))
}

func TestTransport_should_refuse_a_tool_with_no_row(t *testing.T) {
	f := newFixture(t)

	msg := f.rpcError(f.ownerToken(), "delete_project", nil)

	assert.Equal(t, "unknown tool: delete_project", msg)
}

func TestTransport_should_refuse_arguments_that_do_not_match_the_schema(t *testing.T) {
	f := newFixture(t)

	res := f.call(f.ownerToken(), "get_leaderboard", map[string]any{"leaderboard_id": "not a number"})

	assert.True(t, isError(res))
}

const statelessMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func TestTransport_should_serve_the_stateless_revision(t *testing.T) {
	f := newFixture(t)
	tok := f.ownerToken()
	cases := map[string]struct {
		method string
		params string
		header []string
		value  func(result map[string]any) any
	}{
		"server/discover": {
			method: "server/discover", params: `{` + statelessMeta + `}`,
			value: func(r map[string]any) any { return r["supportedVersions"].([]any)[0] },
		},
		"tools/list": {
			method: "tools/list", params: `{` + statelessMeta + `}`,
			value: func(r map[string]any) any { return len(r["tools"].([]any)) },
		},
		"tools/call": {
			method: "tools/call", params: `{"name":"get_remote_config","arguments":{},` + statelessMeta + `}`,
			header: []string{"Mcp-Name", "get_remote_config"},
			value:  func(r map[string]any) any { return r["isError"] },
		},
	}
	want := map[string]any{"server/discover": "2026-07-28", "tools/list": len(readTools), "tools/call": nil}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"` + c.method + `","params":` + c.params + `}`
			header := append([]string{"MCP-Protocol-Version", "2026-07-28", "Mcp-Method", c.method}, c.header...)

			resp, out := f.post(tok, body, header...)

			result, _ := out["result"].(map[string]any)
			require.NotNil(t, result, "no result in %v", out)
			assert.Equal(t, []any{http.StatusOK, "complete", want[name]},
				[]any{resp.StatusCode, result["resultType"], c.value(result)})
		})
	}
}

func TestTransport_should_refuse_a_body_larger_than_the_limit(t *testing.T) {
	f := newFixture(t)
	prefix := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"`
	suffix := `"}}`
	body := prefix + strings.Repeat("x", 131073-len(prefix)-len(suffix)) + suffix

	resp, _ := f.post(f.ownerToken(), body)

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestTransport_should_accept_a_body_at_the_limit(t *testing.T) {
	f := newFixture(t)
	prefix := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"`
	suffix := `"}}`
	body := prefix + strings.Repeat("x", 131072-len(prefix)-len(suffix)) + suffix

	resp, _ := f.post(f.ownerToken(), body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestTransport_should_answer_405_to_get(t *testing.T) {
	f := newFixture(t)

	code := f.get(f.ownerToken(), "/mcp")

	assert.Equal(t, http.StatusMethodNotAllowed, code)
}
