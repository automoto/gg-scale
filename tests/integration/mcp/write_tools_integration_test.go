//go:build integration

package mcp_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isError(res map[string]any) bool {
	v, _ := res["isError"].(bool)
	return v
}

func structured(res map[string]any) map[string]any {
	m, _ := res["structuredContent"].(map[string]any)
	return m
}

// scopedToken is a token of the tenant owner with exactly these scopes.
func (f *fixture) scopedToken(scopes ...string) string {
	f.t.Helper()
	tok, _ := f.token(f.tenantA, f.projectA, f.ownerID, scopes, time.Hour)
	return tok
}

// writeCalls returns one valid call for each write tool. Each call is built
// fresh, so a test can run every call once.
func (f *fixture) writeCalls() map[string]struct {
	scope string
	args  map[string]any
} {
	board := f.board(f.projectA, "b"+strconv.FormatInt(time.Now().UnixNano(), 36))
	deleted := f.board(f.projectA, "d"+strconv.FormatInt(time.Now().UnixNano(), 36))
	f.exec(`UPDATE leaderboards SET deleted_at = now() WHERE id = $1`, deleted)
	return map[string]struct {
		scope string
		args  map[string]any
	}{
		"set_remote_config":      {"config:write", map[string]any{"config_json": `{"a":1}`, "expected_revision": 0}},
		"rollback_remote_config": {"config:write", map[string]any{"revision": 1, "expected_revision": 0}},
		"create_leaderboard":     {"leaderboards:write", map[string]any{"name": "new-" + strconv.FormatInt(board, 10)}},
		"update_leaderboard":     {"leaderboards:write", map[string]any{"leaderboard_id": board, "expected_revision": 0, "name": "renamed"}},
		"rollback_leaderboard":   {"leaderboards:write", map[string]any{"leaderboard_id": board, "revision": 1, "expected_revision": 0}},
		"delete_leaderboard":     {"leaderboards:write", map[string]any{"leaderboard_id": board}},
		"restore_leaderboard":    {"leaderboards:write", map[string]any{"leaderboard_id": deleted}},
		"create_api_key":         {"keys:create", map[string]any{"label": "web build"}},
		"set_allowed_origins":    {"origins:write", map[string]any{"origins": []string{"http://localhost:5173"}}},
	}
}

func TestWriteTools_should_be_refused_without_their_scope(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken()

	for name, c := range f.writeCalls() {
		t.Run(name, func(t *testing.T) {
			res := f.call(tok, name, c.args)

			assert.Contains(t, resultText(res), c.scope)
		})
	}
}

// The rollback tools need a revision to exist first, so this test checks
// that each tool gets past both permission checks: the result is not a
// scope or permission refusal.
func TestWriteTools_should_be_permitted_with_only_their_scope(t *testing.T) {
	f := newFixture(t)

	for name, c := range f.writeCalls() {
		t.Run(name, func(t *testing.T) {
			res := f.call(f.scopedToken(c.scope), name, c.args)

			text := resultText(res)
			assert.NotContains(t, text, "scope")
			assert.NotContains(t, text, "permission")
		})
	}
}

func TestWriteTools_should_succeed_with_their_scope(t *testing.T) {
	f := newFixture(t)
	calls := f.writeCalls()

	for _, name := range []string{"set_remote_config", "create_leaderboard", "update_leaderboard",
		"delete_leaderboard", "restore_leaderboard", "create_api_key", "set_allowed_origins"} {
		t.Run(name, func(t *testing.T) {
			c := calls[name]

			res := f.call(f.scopedToken(c.scope), name, c.args)

			assert.False(t, isError(res), resultText(res))
		})
	}
}

func TestWriteTools_should_be_refused_when_creator_lacks_the_pair(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{"config:write"}, time.Hour)
	f.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = 'project:*:config'`)
	require.NoError(t, f.authz.ReloadPolicy())

	res := f.call(tok, "set_remote_config", map[string]any{"config_json": `{"a":1}`, "expected_revision": 0})

	assert.Contains(t, resultText(res), "does not have the update permission")
}

func TestWriteTools_should_refuse_stale_expected_revision(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("config:write", "leaderboards:write")
	require.False(t, isError(f.call(tok, "set_remote_config", map[string]any{"config_json": `{"a":1}`, "expected_revision": 0})))
	created := structured(f.call(tok, "create_leaderboard", map[string]any{"name": "scores"}))
	board := created["leaderboard_id"]
	require.False(t, isError(f.call(tok, "update_leaderboard", map[string]any{"leaderboard_id": board, "expected_revision": 1, "name": "renamed"})))

	cases := map[string]map[string]any{
		"set_remote_config":      {"config_json": `{"a":2}`, "expected_revision": 1},
		"rollback_remote_config": {"revision": 1, "expected_revision": 1},
		"update_leaderboard":     {"leaderboard_id": board, "expected_revision": 1, "name": "again"},
		"rollback_leaderboard":   {"leaderboard_id": board, "revision": 1, "expected_revision": 1},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res := f.call(tok, name, args)

			assert.Contains(t, resultText(res), "not the newest revision")
		})
	}
}

func TestWriteTools_should_record_token_as_actor(t *testing.T) {
	f := newFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"config:write"}, time.Hour)

	res := f.call(tok, "set_remote_config", map[string]any{"config_json": `{"a":1}`, "expected_revision": 0})
	require.False(t, isError(res), resultText(res))

	var service string
	var revisionToken int64
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT actor_service FROM platform_audit_log WHERE action = 'control_panel.remote_config.update'`).Scan(&service))
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT mcp_token_id FROM settings_revisions WHERE revision = 2 AND project_id = $1`, f.projectA).Scan(&revisionToken))
	assert.Equal(t, []any{"mcp_token:" + strconv.FormatInt(tokenID, 10), tokenID}, []any{service, revisionToken})
}

func TestWriteTools_set_remote_config_keeps_large_integers_exact(t *testing.T) {
	f := newFixture(t)

	f.call(f.scopedToken("config:write"), "set_remote_config",
		map[string]any{"config_json": `{"id":12345678901234567890}`, "expected_revision": 0})

	var config string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT remote_config::text FROM projects WHERE id = $1`, f.projectA).Scan(&config))
	assert.Contains(t, config, "12345678901234567890")
}

func TestCreateAPIKey_should_refuse_a_secret_key_request(t *testing.T) {
	f := newFixture(t)

	res := f.call(f.scopedToken("keys:create"), "create_api_key", map[string]any{"label": "x", "key_type": "secret"})

	assert.True(t, isError(res))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM api_keys`))
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.owner.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func TestCreateAPIKey_should_make_a_publishable_key_for_the_token_project(t *testing.T) {
	f := newFixture(t)

	res := f.call(f.scopedToken("keys:create"), "create_api_key", map[string]any{"label": "web build"})
	require.False(t, isError(res), resultText(res))

	var projectID int64
	var keyType, label string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT project_id, key_type, label FROM api_keys`).Scan(&projectID, &keyType, &label))
	assert.Equal(t, []any{f.projectA, "publishable", "web build (MCP token: test)"}, []any{projectID, keyType, label})
	assert.True(t, strings.HasPrefix(structured(res)["api_key"].(string), "ggp_"))
}

func TestCreateAPIKey_should_refuse_scope_above_feature_grants(t *testing.T) {
	f := newFixture(t)

	res := f.call(f.scopedToken("keys:create"), "create_api_key", map[string]any{"label": "x", "scopes": []string{"p2p_relay"}})

	assert.Contains(t, resultText(res), "scope cannot be granted")
}

func TestCreateAPIKey_scope_error_should_not_name_go_package(t *testing.T) {
	f := newFixture(t)

	res := f.call(f.scopedToken("keys:create"), "create_api_key", map[string]any{"label": "x", "scopes": []string{"p2p_relay"}})

	assert.NotContains(t, resultText(res), "projectadmin")
}

func TestCreateAPIKey_should_refuse_at_the_key_limit(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("keys:create")
	for i := range 3 {
		require.False(t, isError(f.call(tok, "create_api_key", map[string]any{"label": "k" + strconv.Itoa(i)})))
	}

	res := f.call(tok, "create_api_key", map[string]any{"label": "one too many"})

	assert.Contains(t, resultText(res), "maximum number of active API keys")
}

func TestSetAllowedOrigins_should_return_old_list(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("origins:write")
	f.call(tok, "set_allowed_origins", map[string]any{"origins": []string{"https://a.example"}})

	res := f.call(tok, "set_allowed_origins", map[string]any{"origins": []string{"http://localhost:5173"}})

	assert.Equal(t, []any{"https://a.example"}, structured(res)["old_origins"])
}

func TestSetAllowedOrigins_should_refuse_invalid_origin(t *testing.T) {
	f := newFixture(t)

	res := f.call(f.scopedToken("origins:write"), "set_allowed_origins", map[string]any{"origins": []string{"https://*.example"}})

	assert.Contains(t, resultText(res), "wildcard")
}

func TestWriteTools_should_not_find_leaderboards_of_other_projects(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("leaderboards:write")
	otherProject := f.board(f.projectB, "b")
	otherTenant := f.board(f.projectC, "c")
	deletedOther := f.board(f.projectB, "gone")
	f.exec(`UPDATE leaderboards SET deleted_at = now() WHERE id = $1`, deletedOther)

	cases := map[string]map[string]any{
		"update other project":   {"leaderboard_id": otherProject, "expected_revision": 0, "name": "x"},
		"update other tenant":    {"leaderboard_id": otherTenant, "expected_revision": 0, "name": "x"},
		"rollback other project": {"leaderboard_id": otherProject, "revision": 1, "expected_revision": 0},
		"delete other project":   {"leaderboard_id": otherProject},
		"delete other tenant":    {"leaderboard_id": otherTenant},
		"restore other project":  {"leaderboard_id": deletedOther},
	}
	tools := map[string]string{
		"update other project": "update_leaderboard", "update other tenant": "update_leaderboard",
		"rollback other project": "rollback_leaderboard", "delete other project": "delete_leaderboard",
		"delete other tenant": "delete_leaderboard", "restore other project": "restore_leaderboard",
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res := f.call(tok, tools[name], args)

			assert.Equal(t, "not found in this Game Project", resultText(res))
		})
	}
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM leaderboards WHERE id IN ($1, $2) AND (deleted_at IS NOT NULL OR name = 'x')`, otherProject, otherTenant))
}
