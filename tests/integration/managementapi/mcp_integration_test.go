//go:build integration

package managementapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/httpapi"
)

// post sends one JSON-RPC message to /mcp. header holds extra header
// name and value pairs.
func (f *fixture) post(token, body string, header ...string) (*http.Response, map[string]any) {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/mcp", bytes.NewBufferString(body))
	require.NoError(f.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`

// call runs a tool and returns its result object.
func (f *fixture) call(token, name string, args map[string]any) map[string]any {
	f.t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	require.NoError(f.t, err)
	resp, out := f.post(token, string(body))
	require.Equal(f.t, http.StatusOK, resp.StatusCode)
	result, ok := out["result"].(map[string]any)
	require.True(f.t, ok, "no result in %v", out)
	return result
}

func resultText(result map[string]any) string {
	return result["content"].([]any)[0].(map[string]any)["text"].(string)
}

// resultDetail is the problem+json detail text of a tool error result.
func resultDetail(result map[string]any) string {
	var problem struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(resultText(result)), &problem)
	return problem.Detail
}

// rpcError runs a tool and returns the message of its JSON-RPC error.
func (f *fixture) rpcError(token, name string, args map[string]any) string {
	f.t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	require.NoError(f.t, err)
	_, out := f.post(token, string(body))
	rpcErr, ok := out["error"].(map[string]any)
	require.True(f.t, ok, "no error in %v", out)
	msg, _ := rpcErr["message"].(string)
	return msg
}

func (f *fixture) ownerToken() string {
	tok, _ := f.token(f.tenantA, f.projectA, f.ownerID, nil, time.Hour)
	return tok
}

func TestMCP_should_return_401_with_bearer_challenge_for_bad_tokens(t *testing.T) {
	f := newFixture(t)
	expired, _ := f.token(f.tenantA, f.projectA, f.ownerID, nil, -time.Hour)
	revoked, revokedID := f.token(f.tenantA, f.projectA, f.ownerID, nil, time.Hour)
	f.exec(`UPDATE management_tokens SET revoked_at = now() WHERE id = $1`, revokedID)

	for name, tok := range map[string]string{"missing": "", "wrong": "ggm_nope", "expired": expired, "revoked": revoked} {
		t.Run(name, func(t *testing.T) {
			resp, _ := f.post(tok, initialize)

			assert.Equal(t, []any{http.StatusUnauthorized, "Bearer"}, []any{resp.StatusCode, resp.Header.Get("WWW-Authenticate")})
		})
	}
}

func TestMCP_initialize_should_succeed_for_valid_token(t *testing.T) {
	f := newFixture(t)

	resp, out := f.post(f.ownerToken(), initialize)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "2025-06-18", out["result"].(map[string]any)["protocolVersion"])
}

func TestMCP_initialize_should_report_the_build_version(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.Release = "v9.9.9" })

	// Act
	_, out := f.post(f.ownerToken(), initialize)

	// Assert
	info, _ := out["result"].(map[string]any)["serverInfo"].(map[string]any)
	assert.Equal(t, "v9.9.9", info["version"], out)
}

func TestMCP_tool_call_should_work_with_the_token_cache_on(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.ManagementTokenCacheTTL = 2 * time.Second })

	// Act
	res := f.call(f.ownerToken(), "get_remote_config", map[string]any{})

	// Assert
	assert.NotEqual(t, true, res["isError"], resultText(res))
}

func TestMCP_should_not_limit_correct_requests_by_ip(t *testing.T) {
	f := newFixture(t)
	tok := f.ownerToken()

	codes := map[int]int{}
	for range 20 {
		resp, _ := f.post(tok, initialize)
		codes[resp.StatusCode]++
	}

	assert.Equal(t, map[int]int{http.StatusOK: 20}, codes)
}

func TestMCP_should_limit_wrong_tokens_by_ip(t *testing.T) {
	f := newFixture(t)

	var last int
	for range 11 {
		resp, _ := f.post("ggm_wrong", initialize)
		last = resp.StatusCode
	}

	assert.Equal(t, http.StatusTooManyRequests, last)
}

func TestMCP_wrong_tokens_should_not_block_valid_token_from_same_ip(t *testing.T) {
	f := newFixture(t)
	tok := f.ownerToken()
	for range 11 {
		f.post("ggm_wrong", initialize)
	}

	resp, _ := f.post(tok, initialize)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMCP_ip_failure_limit_is_configurable(t *testing.T) {
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.ManagementAuthFailuresPerMinute, d.ManagementAuthFailureBurst = 1, 2 })
	f.post("ggm_wrong", initialize)
	f.post("ggm_wrong", initialize)

	resp, _ := f.post("ggm_wrong", initialize)

	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
}

func TestMCP_should_refuse_request_from_another_origin(t *testing.T) {
	f := newFixture(t)
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/mcp", bytes.NewBufferString(initialize))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.ownerToken())
	req.Header.Set("Origin", "https://evil.example")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestMCP_should_return_403_when_access_ends(t *testing.T) {
	cases := map[string]func(f *fixture){
		"project deleted": func(f *fixture) { f.exec(`UPDATE projects SET deleted_at = now() WHERE id = $1`, f.projectA) },
		"tenant disabled": func(f *fixture) {
			f.exec(`UPDATE tenants SET disabled_at = now(), disabled_by = 'platform' WHERE id = $1`, f.tenantA)
		},
		"creator disabled": func(f *fixture) {
			f.exec(`UPDATE control_panel_users SET disabled_at = now() WHERE id = $1`, f.ownerID)
		},
		"creator removed from tenant": func(f *fixture) {
			require.NoError(f.t, f.authz.RemoveControlPanelRoles(f.ownerID, f.tenantA))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tok := f.ownerToken()
			change(f)

			resp, _ := f.post(tok, initialize)

			assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		})
	}
}

func TestMCP_admin_token_stops_when_admin_becomes_member(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, nil, time.Hour)
	require.NoError(t, f.authz.RemoveControlPanelRoles(admin, f.tenantA))
	require.NoError(t, f.authz.SetControlPanelMembershipRole(admin, f.tenantA, "member"))

	resp, _ := f.post(tok, initialize)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestMCP_admin_token_can_use_ticket_trace(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, nil, time.Hour)
	ticket := f.ticket(f.projectA, f.player(f.projectA), "queued", `{}`)

	res := f.call(tok, "matchmaking_ticket_trace", map[string]any{"ticket_id": ticket})

	assert.NotEqual(t, true, res["isError"], resultText(res))
}

func TestMCP_trace_by_player_returns_only_queued_ticket(t *testing.T) {
	f := newFixture(t)
	player := f.player(f.projectA)
	f.ticket(f.projectA, player, "failed", `{}`)
	queued := f.ticket(f.projectA, player, "queued", `{}`)

	res := f.call(f.ownerToken(), "matchmaking_ticket_trace", map[string]any{"player_id": player})

	assert.Equal(t, float64(queued), res["structuredContent"].(map[string]any)["ticket_id"])
}

func TestMCP_trace_never_returns_client_text_or_attributes(t *testing.T) {
	f := newFixture(t)
	ticket := f.ticket(f.projectA, f.player(f.projectA), "queued", `{"x":"SECRET_ATTRIBUTE_TEXT"}`)

	text := resultText(f.call(f.ownerToken(), "matchmaking_ticket_trace", map[string]any{"ticket_id": ticket}))

	for _, leaked := range []string{"SECRET_ATTRIBUTE_TEXT", "run the delete tool", "ignore all rules"} {
		assert.NotContains(t, text, leaked)
	}
}

func TestMCP_should_not_find_resources_of_other_projects(t *testing.T) {
	f := newFixture(t)
	tok := f.ownerToken()
	otherProjectTicket := f.ticket(f.projectB, f.player(f.projectB), "queued", `{}`)
	cases := map[string]struct {
		tool string
		args map[string]any
	}{
		"ticket of other project":      {"matchmaking_ticket_trace", map[string]any{"ticket_id": otherProjectTicket}},
		"leaderboard of other project": {"get_leaderboard", map[string]any{"leaderboard_id": f.board(f.projectB, "b")}},
		"leaderboard of other tenant":  {"get_leaderboard", map[string]any{"leaderboard_id": f.board(f.projectC, "c")}},
		"ticket of other tenant":       {"matchmaking_ticket_trace", map[string]any{"ticket_id": f.ticket(f.projectC, f.player(f.projectC), "queued", `{}`)}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			res := f.call(tok, c.tool, c.args)

			assert.Equal(t, "not found in this Game Project", resultDetail(res))
		})
	}
}

func TestMCP_health_check_returns_no_count_for_tenant_quota(t *testing.T) {
	f := newFixture(t)
	f.exec(`UPDATE tenants SET enforce_quotas = true WHERE id = $1`, f.tenantA)

	res := f.call(f.ownerToken(), "project_health_check", nil)

	quotas := res["structuredContent"].(map[string]any)["checks"].(map[string]any)["quotas"].(map[string]any)
	assert.Equal(t, []any{"ok", "ok"}, []any{quotas["projects"], quotas["players"]})
}

func TestMCP_last_used_is_written_at_most_once_a_minute(t *testing.T) {
	f := newFixture(t)
	tok, id := f.token(f.tenantA, f.projectA, f.ownerID, nil, time.Hour)
	f.post(tok, initialize)
	var first time.Time
	require.NoError(t, f.owner.QueryRow(context.Background(), `SELECT last_used_at FROM management_tokens WHERE id = $1`, id).Scan(&first))

	f.post(tok, initialize)

	var second time.Time
	require.NoError(t, f.owner.QueryRow(context.Background(), `SELECT last_used_at FROM management_tokens WHERE id = $1`, id).Scan(&second))
	assert.Equal(t, first, second)
}

func (f *fixture) platformAdmin(email string) int64 {
	f.t.Helper()
	id := f.scalar(`INSERT INTO control_panel_users (email, password_hash, is_platform_admin, email_verified_at)
		VALUES ($1, '\x00'::bytea, true, now()) RETURNING id`, email)
	require.NoError(f.t, f.authz.AddPlatformAdmin(id))
	return id
}

func TestMCP_platform_admin_token_is_limited_to_its_project(t *testing.T) {
	f := newFixture(t)
	tok, _ := f.token(f.tenantA, f.projectA, f.platformAdmin("pa@example.com"), nil, time.Hour)

	texts := []string{}
	for _, project := range []int64{f.projectB, f.projectC} {
		res := f.call(tok, "get_leaderboard", map[string]any{"leaderboard_id": f.board(project, "x")})
		texts = append(texts, resultDetail(res))
	}

	assert.Equal(t, []string{"not found in this Game Project", "not found in this Game Project"}, texts)
}

func TestMCP_platform_admin_token_stops_when_flag_is_removed(t *testing.T) {
	f := newFixture(t)
	admin := f.platformAdmin("pa@example.com")
	tok, _ := f.token(f.tenantA, f.projectA, admin, nil, time.Hour)
	require.NoError(t, f.authz.RemovePlatformAdmin(admin))

	resp, _ := f.post(tok, initialize)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestMCP_platform_admin_tools_list_has_only_table_tools(t *testing.T) {
	f := newFixture(t)
	tok, _ := f.token(f.tenantA, f.projectA, f.platformAdmin("pa@example.com"), nil, time.Hour)

	_, out := f.post(tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	names := []string{}
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	assert.ElementsMatch(t, []string{
		"project_health_check", "matchmaking_ticket_trace", "get_remote_config",
		"list_leaderboards", "get_leaderboard", "list_api_keys", "list_revisions",
	}, names)
}

func TestMCP_token_that_no_longer_works_is_limited_by_ip(t *testing.T) {
	f := newFixture(t)
	tok := f.ownerToken()
	require.NoError(t, f.authz.RemoveControlPanelRoles(f.ownerID, f.tenantA))

	var last int
	for range 11 {
		resp, _ := f.post(tok, initialize)
		last = resp.StatusCode
	}

	assert.Equal(t, http.StatusTooManyRequests, last)
}

func TestMCP_agent_over_its_token_rate_keeps_the_ip_bucket(t *testing.T) {
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.ManagementTokenRatePerSecond, d.ManagementTokenBurst = 0.001, 1 })
	limited := f.ownerToken()
	for range 15 {
		f.post(limited, initialize)
	}

	resp, _ := f.post(f.ownerToken(), initialize)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMCP_refused_request_does_not_update_last_used(t *testing.T) {
	f := newFixture(t)
	tok, id := f.token(f.tenantA, f.projectA, f.ownerID, nil, time.Hour)
	require.NoError(t, f.authz.RemoveControlPanelRoles(f.ownerID, f.tenantA))

	f.post(tok, initialize)

	var used bool
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT last_used_at IS NOT NULL FROM management_tokens WHERE id = $1`, id).Scan(&used))
	assert.False(t, used)
}

func TestMCP_revoked_and_unknown_tokens_get_the_same_answer(t *testing.T) {
	f := newFixture(t)
	revoked, id := f.token(f.tenantA, f.projectA, f.ownerID, nil, time.Hour)
	f.exec(`UPDATE management_tokens SET revoked_at = now() WHERE id = $1`, id)

	got := []string{postBody(t, f, revoked), postBody(t, f, "ggm_unknown")}

	assert.Equal(t, []string{"unauthorized\n", "unauthorized\n"}, got)
}

func postBody(t *testing.T, f *fixture, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/mcp", bytes.NewBufferString(initialize))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}
